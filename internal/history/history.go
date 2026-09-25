// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package history persists threads and messages. Signal servers keep no
// history, so this store is the only record of a conversation.
package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/util/dbutil"

	"signal-headless/internal/model"
)

type Store struct {
	db *dbutil.Database
}

func New(db *dbutil.Database) *Store { return &Store{db: db} }

var ErrNotFound = errors.New("not found")

// EnsureThread creates the thread if missing. A non-empty title replaces the
// stored one.
func (s *Store) EnsureThread(ctx context.Context, id model.ThreadID, kind model.ThreadKind, title string) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO sh_thread (id, kind, title) VALUES ($1, $2, $3)
		ON CONFLICT (id) DO UPDATE SET title = CASE WHEN excluded.title <> '' THEN excluded.title ELSE sh_thread.title END`,
		string(id), string(kind), title)
	return err
}

func (s *Store) ThreadExists(ctx context.Context, id model.ThreadID) (bool, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM sh_thread WHERE id=$1`, string(id)).Scan(&n)
	return n > 0, err
}

const threadSelect = `
	SELECT t.id, t.kind, t.title, t.last_ts, t.archived, t.expire_timer,
		(SELECT COUNT(*) FROM sh_message m WHERE m.thread_id = t.id AND m.outgoing = 0 AND m.read = 0 AND m.deleted = 0),
		COALESCE((SELECT m.id FROM sh_message m WHERE m.thread_id = t.id ORDER BY m.ts DESC LIMIT 1), 0)
	FROM sh_thread t`

func (s *Store) scanThreads(ctx context.Context, rows dbutil.Rows) ([]model.Thread, error) {
	defer rows.Close()
	var out []model.Thread
	var lastIDs []int64
	for rows.Next() {
		var t model.Thread
		var lastID int64
		if err := rows.Scan(&t.ID, &t.Kind, &t.Title, &t.LastTS, &t.Archived, &t.ExpireTimer, &t.Unread, &lastID); err != nil {
			return nil, err
		}
		out = append(out, t)
		lastIDs = append(lastIDs, lastID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, id := range lastIDs {
		if id == 0 {
			continue
		}
		m, err := s.messageByID(ctx, id, false)
		if err == nil {
			out[i].LastPreview = m.Preview()
			out[i].LastAuthor = m.Author
		}
	}
	return out, nil
}

// Threads lists threads, most recently active first.
func (s *Store) Threads(ctx context.Context) ([]model.Thread, error) {
	rows, err := s.db.Query(ctx, threadSelect+` ORDER BY t.last_ts DESC, t.title`)
	if err != nil {
		return nil, err
	}
	return s.scanThreads(ctx, rows)
}

func (s *Store) Thread(ctx context.Context, id model.ThreadID) (*model.Thread, error) {
	rows, err := s.db.Query(ctx, threadSelect+` WHERE t.id = $1`, string(id))
	if err != nil {
		return nil, err
	}
	ts, err := s.scanThreads(ctx, rows)
	if err != nil {
		return nil, err
	}
	if len(ts) == 0 {
		return nil, ErrNotFound
	}
	return &ts[0], nil
}

// SetTimer records a thread's disappearing-messages timer. Older versions
// (lower non-zero version numbers) are ignored.
func (s *Store) SetTimer(ctx context.Context, id model.ThreadID, seconds, version uint32) error {
	_, err := s.db.Exec(ctx, `
		UPDATE sh_thread SET expire_timer=$2, expire_version=MAX(expire_version, $3)
		WHERE id=$1 AND ($3 = 0 OR $3 >= expire_version)`, string(id), seconds, version)
	return err
}

// Timer returns a thread's disappearing-messages timer and version.
func (s *Store) Timer(ctx context.Context, id model.ThreadID) (seconds, version uint32, err error) {
	err = s.db.QueryRow(ctx, `SELECT expire_timer, expire_version FROM sh_thread WHERE id=$1`, string(id)).Scan(&seconds, &version)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}

// Title returns a thread's stored title ("" if unknown).
func (s *Store) Title(ctx context.Context, id model.ThreadID) string {
	var t string
	_ = s.db.QueryRow(ctx, `SELECT title FROM sh_thread WHERE id=$1`, string(id)).Scan(&t)
	return t
}

func (s *Store) SetTitle(ctx context.Context, id model.ThreadID, title string) error {
	_, err := s.db.Exec(ctx, `UPDATE sh_thread SET title=$2 WHERE id=$1`, string(id), title)
	return err
}

func (s *Store) SetArchived(ctx context.Context, id model.ThreadID, archived bool) error {
	_, err := s.db.Exec(ctx, `UPDATE sh_thread SET archived=$2 WHERE id=$1`, string(id), archived)
	return err
}

// InsertMessage stores m (and its attachments). Duplicates of an existing
// (thread, author, ts) are ignored and reported as inserted=false. The
// thread must already exist. m.ID is set on success.
func (s *Store) InsertMessage(ctx context.Context, m *model.Message) (inserted bool, err error) {
	if m.ReceivedAt == 0 {
		m.ReceivedAt = time.Now().UnixMilli()
	}
	err = s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		var q model.Quote
		if m.Quote != nil {
			q = *m.Quote
			// A reply can cross the deletion of what it quotes.
			var deleted bool
			err := s.db.QueryRow(ctx, `SELECT deleted FROM sh_message WHERE thread_id=$1 AND author=$2 AND ts=$3`, m.Thread, q.Author, q.TS).Scan(&deleted)
			if err == nil && deleted {
				q.Text = ""
				m.Quote.Text = ""
			} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		read := m.Read || m.Outgoing
		var expireStart int64
		if m.ExpiresIn > 0 && read {
			expireStart = m.ReceivedAt
		}
		res, err := s.db.Exec(ctx, `
			INSERT INTO sh_message (thread_id, author, ts, server_ts, received_at, outgoing, read, status, body,
				quote_author, quote_ts, quote_text, expires_in, expire_start, sticker)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			ON CONFLICT (thread_id, author, ts) DO NOTHING`,
			string(m.Thread), m.Author, m.TS, m.ServerTS, m.ReceivedAt, m.Outgoing, read, string(m.Status), m.Body,
			q.Author, q.TS, q.Text, m.ExpiresIn, expireStart, m.Sticker)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return s.db.QueryRow(ctx, `SELECT id FROM sh_message WHERE thread_id=$1 AND author=$2 AND ts=$3`,
				string(m.Thread), m.Author, m.TS).Scan(&m.ID)
		}
		inserted = true
		if m.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		for i := range m.Attachments {
			a := &m.Attachments[i]
			a.Index = i
			if a.State == "" {
				a.State = model.AttachmentPending
			}
			_, err = s.db.Exec(ctx, `
				INSERT INTO sh_attachment (message_id, idx, content_type, filename, size, path, state, error, voice_note, pointer, kind)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
				m.ID, i, a.ContentType, a.Filename, a.Size, a.Path, string(a.State), a.Error, a.VoiceNote, a.Pointer, a.Kind)
			if err != nil {
				return err
			}
		}
		for i, p := range m.Previews {
			_, err = s.db.Exec(ctx, `
				INSERT INTO sh_preview (message_id, idx, url, title, description, date, image_idx)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				m.ID, i, p.URL, p.Title, p.Description, p.Date, p.Image)
			if err != nil {
				return err
			}
		}
		_, err = s.db.Exec(ctx, `UPDATE sh_thread SET last_ts = MAX(last_ts, $2), archived = 0 WHERE id = $1`, string(m.Thread), m.TS)
		return err
	})
	return inserted, err
}

const messageSelect = `
	SELECT id, thread_id, author, ts, server_ts, received_at, outgoing, read, status, body,
		quote_author, quote_ts, quote_text, edited_at, deleted, expires_in, sticker
	FROM sh_message`

func scanMessage(row dbutil.Scannable) (*model.Message, error) {
	var m model.Message
	var q model.Quote
	err := row.Scan(&m.ID, &m.Thread, &m.Author, &m.TS, &m.ServerTS, &m.ReceivedAt, &m.Outgoing, &m.Read, &m.Status, &m.Body,
		&q.Author, &q.TS, &q.Text, &m.EditedAt, &m.Deleted, &m.ExpiresIn, &m.Sticker)
	if err != nil {
		return nil, err
	}
	if q.TS != 0 {
		m.Quote = &q
	}
	return &m, nil
}

func (s *Store) fillDetails(ctx context.Context, msgs []*model.Message, withPointers bool) error {
	if len(msgs) == 0 {
		return nil
	}
	byID := make(map[int64]*model.Message, len(msgs))
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		byID[m.ID] = m
		ids = append(ids, fmt.Sprint(m.ID))
	}
	in := strings.Join(ids, ",")
	rows, err := s.db.Query(ctx, `
		SELECT message_id, idx, content_type, filename, size, path, state, error, voice_note, pointer, kind
		FROM sh_attachment WHERE message_id IN (`+in+`) ORDER BY message_id, idx`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var a model.Attachment
		if err := rows.Scan(&id, &a.Index, &a.ContentType, &a.Filename, &a.Size, &a.Path, &a.State, &a.Error, &a.VoiceNote, &a.Pointer, &a.Kind); err != nil {
			rows.Close()
			return err
		}
		if !withPointers {
			a.Pointer = nil
		}
		byID[id].Attachments = append(byID[id].Attachments, a)
	}
	rows.Close()
	rows, err = s.db.Query(ctx, `SELECT message_id, url, title, description, date, image_idx FROM sh_preview WHERE message_id IN (`+in+`) ORDER BY message_id, idx`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var p model.LinkPreview
		if err := rows.Scan(&id, &p.URL, &p.Title, &p.Description, &p.Date, &p.Image); err != nil {
			rows.Close()
			return err
		}
		byID[id].Previews = append(byID[id].Previews, p)
	}
	rows.Close()
	rows, err = s.db.Query(ctx, `SELECT message_id, reactor, emoji, ts FROM sh_reaction WHERE message_id IN (`+in+`) ORDER BY ts`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var r model.Reaction
		if err := rows.Scan(&id, &r.Reactor, &r.Emoji, &r.TS); err != nil {
			return err
		}
		byID[id].Reactions = append(byID[id].Reactions, r)
	}
	return rows.Err()
}

func (s *Store) messageByID(ctx context.Context, id int64, withPointers bool) (*model.Message, error) {
	m, err := scanMessage(s.db.QueryRow(ctx, messageSelect+` WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	return m, s.fillDetails(ctx, []*model.Message{m}, withPointers)
}

// Message returns a message by row ID, including attachments and reactions.
func (s *Store) Message(ctx context.Context, id int64) (*model.Message, error) {
	return s.messageByID(ctx, id, false)
}

// MessageByRef finds a message by Signal identity. thread may be empty.
func (s *Store) MessageByRef(ctx context.Context, thread model.ThreadID, ref model.MessageRef) (*model.Message, error) {
	var row *sql.Row
	if thread != "" {
		row = s.db.QueryRow(ctx, messageSelect+` WHERE thread_id=$1 AND author=$2 AND ts=$3`, string(thread), ref.Author, ref.TS)
	} else {
		row = s.db.QueryRow(ctx, messageSelect+` WHERE author=$1 AND ts=$2 LIMIT 1`, ref.Author, ref.TS)
	}
	m, err := scanMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	return m, s.fillDetails(ctx, []*model.Message{m}, false)
}

// Messages returns up to limit messages of a thread older than beforeTS
// (0 = newest), in chronological order.
func (s *Store) Messages(ctx context.Context, thread model.ThreadID, beforeTS int64, limit int) ([]*model.Message, error) {
	if limit <= 0 {
		limit = 100
	}
	if beforeTS <= 0 {
		beforeTS = 1<<62 - 1
	}
	rows, err := s.db.Query(ctx, messageSelect+` WHERE thread_id=$1 AND ts < $2 ORDER BY ts DESC LIMIT $3`,
		string(thread), beforeTS, limit)
	if err != nil {
		return nil, err
	}
	var out []*model.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	rows.Close()
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, s.fillDetails(ctx, out, false)
}

// Search finds messages whose body contains query (case-insensitive), newest first.
func (s *Store) Search(ctx context.Context, thread model.ThreadID, query string, limit int) ([]*model.Message, error) {
	if limit <= 0 {
		limit = 50
	}
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query) + "%"
	q := messageSelect + ` WHERE body LIKE $1 ESCAPE '\' AND deleted = 0`
	args := []any{like}
	if thread != "" {
		q += ` AND thread_id = $2`
		args = append(args, string(thread))
	}
	q += fmt.Sprintf(` ORDER BY ts DESC LIMIT %d`, limit)
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var out []*model.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	rows.Close()
	return out, s.fillDetails(ctx, out, false)
}

// ApplyEdit updates the body of a message. Returns ErrNotFound if unknown.
func (s *Store) ApplyEdit(ctx context.Context, thread model.ThreadID, author string, targetTS, editTS int64, body string) (*model.Message, error) {
	res, err := s.db.Exec(ctx, `UPDATE sh_message SET body=$4, edited_at=$5 WHERE thread_id=$1 AND author=$2 AND ts=$3`,
		string(thread), author, targetTS, body, editTS)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.MessageByRef(ctx, thread, model.MessageRef{Author: author, TS: targetTS})
}

// ApplyDelete marks a message deleted and drops its body and attachments' metadata.
func (s *Store) ApplyDelete(ctx context.Context, thread model.ThreadID, author string, targetTS int64) (*model.Message, error) {
	m, err := s.MessageByRef(ctx, thread, model.MessageRef{Author: author, TS: targetTS})
	if err != nil {
		return nil, err
	}
	err = s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		if _, err := s.db.Exec(ctx, `UPDATE sh_message SET deleted=1, deleted_at=$2, body='', quote_text='' WHERE id=$1`, m.ID, time.Now().UnixMilli()); err != nil {
			return err
		}
		if _, err := s.db.Exec(ctx, `DELETE FROM sh_reaction WHERE message_id=$1`, m.ID); err != nil {
			return err
		}
		if _, err := s.db.Exec(ctx, `DELETE FROM sh_preview WHERE message_id=$1`, m.ID); err != nil {
			return err
		}
		_, err := s.db.Exec(ctx, `DELETE FROM sh_attachment WHERE message_id=$1`, m.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Message(ctx, m.ID)
}

// ApplyReaction sets or removes reactor's reaction on the target message.
func (s *Store) ApplyReaction(ctx context.Context, thread model.ThreadID, target model.MessageRef, reactor, emoji string, remove bool, ts int64) (*model.Message, error) {
	m, err := s.MessageByRef(ctx, thread, target)
	if err != nil {
		return nil, err
	}
	if remove {
		_, err = s.db.Exec(ctx, `DELETE FROM sh_reaction WHERE message_id=$1 AND reactor=$2`, m.ID, reactor)
	} else {
		_, err = s.db.Exec(ctx, `
			INSERT INTO sh_reaction (message_id, reactor, emoji, ts) VALUES ($1, $2, $3, $4)
			ON CONFLICT (message_id, reactor) DO UPDATE SET emoji=excluded.emoji, ts=excluded.ts`,
			m.ID, reactor, emoji, ts)
	}
	if err != nil {
		return nil, err
	}
	return s.Message(ctx, m.ID)
}

// ApplyReceipt advances the status of our outgoing messages with the given
// timestamps. Returns the messages whose status changed.
func (s *Store) ApplyReceipt(ctx context.Context, self string, status model.Status, timestamps []int64) ([]*model.Message, error) {
	var changed []*model.Message
	for _, ts := range timestamps {
		rows, err := s.db.Query(ctx, messageSelect+` WHERE author=$1 AND ts=$2 AND outgoing=1`, self, ts)
		if err != nil {
			return nil, err
		}
		var ms []*model.Message
		for rows.Next() {
			m, err := scanMessage(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			ms = append(ms, m)
		}
		rows.Close()
		for _, m := range ms {
			if status.Rank() <= m.Status.Rank() {
				continue
			}
			if _, err := s.db.Exec(ctx, `UPDATE sh_message SET status=$2 WHERE id=$1`, m.ID, string(status)); err != nil {
				return nil, err
			}
			m.Status = status
			changed = append(changed, m)
		}
	}
	return changed, nil
}

func (s *Store) SetStatus(ctx context.Context, id int64, status model.Status) error {
	_, err := s.db.Exec(ctx, `UPDATE sh_message SET status=$2 WHERE id=$1`, id, string(status))
	return err
}

// startTimer starts the disappearing timer ($2 = now ms) for messages that have one.
const startTimer = `expire_start = CASE WHEN expires_in > 0 AND expire_start = 0 THEN $2 ELSE expire_start END`

// MarkThreadRead marks all incoming messages in a thread read and returns the
// ones that were unread (for sending read receipts).
func (s *Store) MarkThreadRead(ctx context.Context, thread model.ThreadID) ([]model.MessageRef, error) {
	rows, err := s.db.Query(ctx, `SELECT author, ts FROM sh_message WHERE thread_id=$1 AND outgoing=0 AND read=0`, string(thread))
	if err != nil {
		return nil, err
	}
	var refs []model.MessageRef
	for rows.Next() {
		var r model.MessageRef
		if err := rows.Scan(&r.Author, &r.TS); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, r)
	}
	rows.Close()
	if len(refs) == 0 {
		return nil, nil
	}
	_, err = s.db.Exec(ctx, `UPDATE sh_message SET read=1, `+startTimer+` WHERE thread_id=$1 AND outgoing=0 AND read=0`,
		string(thread), time.Now().UnixMilli())
	return refs, err
}

// ApplyReadSync marks messages read on another device: each ref and every
// earlier incoming message in the same thread. Returns affected thread IDs.
func (s *Store) ApplyReadSync(ctx context.Context, refs []model.MessageRef) ([]model.ThreadID, error) {
	seen := map[model.ThreadID]bool{}
	var threads []model.ThreadID
	for _, r := range refs {
		var thread string
		err := s.db.QueryRow(ctx, `SELECT thread_id FROM sh_message WHERE author=$1 AND ts=$2 LIMIT 1`, r.Author, r.TS).Scan(&thread)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			return nil, err
		}
		if _, err := s.db.Exec(ctx, `UPDATE sh_message SET read=1, `+startTimer+` WHERE thread_id=$1 AND outgoing=0 AND read=0 AND ts <= $3`,
			thread, time.Now().UnixMilli(), r.TS); err != nil {
			return nil, err
		}
		if !seen[model.ThreadID(thread)] {
			seen[model.ThreadID(thread)] = true
			threads = append(threads, model.ThreadID(thread))
		}
	}
	return threads, nil
}

// PendingAttachment is an attachment still to be downloaded.
type PendingAttachment struct {
	MessageID int64
	Thread    model.ThreadID
	TS        int64
	model.Attachment
}

func (s *Store) PendingAttachments(ctx context.Context) ([]PendingAttachment, error) {
	rows, err := s.db.Query(ctx, `
		SELECT a.message_id, m.thread_id, m.ts, a.idx, a.content_type, a.filename, a.size, a.voice_note, a.pointer
		FROM sh_attachment a JOIN sh_message m ON m.id = a.message_id
		WHERE a.state = 'pending' AND a.pointer IS NOT NULL
		ORDER BY m.ts DESC, a.message_id, a.idx`) // newest first: live media before a history backlog
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingAttachment
	for rows.Next() {
		var p PendingAttachment
		if err := rows.Scan(&p.MessageID, &p.Thread, &p.TS, &p.Index, &p.ContentType, &p.Filename, &p.Size, &p.VoiceNote, &p.Pointer); err != nil {
			return nil, err
		}
		p.State = model.AttachmentPending
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) UpdateAttachment(ctx context.Context, messageID int64, idx int, state model.AttachmentState, path, errMsg string) error {
	_, err := s.db.Exec(ctx, `UPDATE sh_attachment SET state=$3, path=$4, error=$5 WHERE message_id=$1 AND idx=$2`,
		messageID, idx, string(state), path, errMsg)
	return err
}

// Expired returns messages whose disappearing timer has run out by now (ms).
func (s *Store) Expired(ctx context.Context, now int64) ([]*model.Message, error) {
	rows, err := s.db.Query(ctx, messageSelect+`
		WHERE expires_in > 0 AND expire_start > 0 AND deleted = 0 AND expire_start + expires_in*1000 <= $1`, now)
	if err != nil {
		return nil, err
	}
	var out []*model.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	rows.Close()
	return out, s.fillDetails(ctx, out, false)
}

// Removed identifies a message row that no longer exists.
type Removed struct {
	ID     int64          `json:"id"`
	Thread model.ThreadID `json:"thread"`
}

// PurgeDeleted removes the placeholders of messages deleted at or before
// before (ms), returning what it removed.
func (s *Store) PurgeDeleted(ctx context.Context, before int64) ([]Removed, error) {
	var out []Removed
	err := s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		rows, err := s.db.Query(ctx, `SELECT id, thread_id FROM sh_message WHERE deleted = 1 AND deleted_at <= $1`, before)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r Removed
			if err := rows.Scan(&r.ID, &r.Thread); err != nil {
				rows.Close()
				return err
			}
			out = append(out, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		_, err = s.db.Exec(ctx, `DELETE FROM sh_message WHERE deleted = 1 AND deleted_at <= $1`, before)
		return err
	})
	return out, err
}

// ScrubQuotes clears the quoted text in messages that quote the given
// (deleted) message, returning their IDs.
func (s *Store) ScrubQuotes(ctx context.Context, thread model.ThreadID, author string, ts int64) ([]int64, error) {
	rows, err := s.db.Query(ctx, `SELECT id FROM sh_message WHERE thread_id=$1 AND quote_author=$2 AND quote_ts=$3 AND quote_text != ''`, thread, author, ts)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := s.db.Exec(ctx, `UPDATE sh_message SET quote_text='' WHERE id=$1`, id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// ImportHistory stores transferred messages in one transaction, with their
// reactions and edit markers. Messages already present are left alone.
// It returns how many were new.
func (s *Store) ImportHistory(ctx context.Context, msgs []model.Message) (int, error) {
	n := 0
	err := s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		for i := range msgs {
			m := &msgs[i]
			inserted, err := s.InsertMessage(ctx, m)
			if err != nil {
				return err
			}
			if !inserted {
				continue
			}
			n++
			if m.EditedAt != 0 {
				if _, err := s.db.Exec(ctx, `UPDATE sh_message SET edited_at=$2 WHERE id=$1`, m.ID, m.EditedAt); err != nil {
					return err
				}
			}
			for _, r := range m.Reactions {
				if _, err := s.db.Exec(ctx, `
					INSERT INTO sh_reaction (message_id, reactor, emoji, ts) VALUES ($1, $2, $3, $4)
					ON CONFLICT (message_id, reactor) DO NOTHING`, m.ID, r.Reactor, r.Emoji, r.TS); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return n, err
}

// Stats are counts over the stored history.
type Stats struct {
	Threads            int   `json:"threads"`
	Messages           int   `json:"messages"`
	Attachments        int   `json:"attachments"`
	AttachmentsPending int   `json:"attachmentsPending"`
	AttachmentsFailed  int   `json:"attachmentsFailed"`
	Reactions          int   `json:"reactions"`
	OldestTS           int64 `json:"oldestTs,omitempty"`
	NewestTS           int64 `json:"newestTs,omitempty"`
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	err := s.db.QueryRow(ctx, `SELECT
		(SELECT COUNT(*) FROM sh_thread),
		(SELECT COUNT(*) FROM sh_message),
		(SELECT COUNT(*) FROM sh_attachment),
		(SELECT COUNT(*) FROM sh_attachment WHERE state = 'pending'),
		(SELECT COUNT(*) FROM sh_attachment WHERE state = 'failed'),
		(SELECT COUNT(*) FROM sh_reaction),
		COALESCE((SELECT MIN(ts) FROM sh_message), 0),
		COALESCE((SELECT MAX(ts) FROM sh_message), 0)`).Scan(
		&st.Threads, &st.Messages, &st.Attachments, &st.AttachmentsPending, &st.AttachmentsFailed, &st.Reactions, &st.OldestTS, &st.NewestTS)
	return st, err
}

// PurgeResult describes what a purge removed (or would remove).
type PurgeResult struct {
	Messages    int              `json:"messages"`
	Attachments int              `json:"attachments"`
	Threads     []model.ThreadID `json:"threads,omitempty"` // conversations that lost messages
	Paths       []string         `json:"-"`                 // attachment files to delete
	Refs        []ThreadRef      `json:"-"`                 // the messages (for delete-for-me sync)
}

// ThreadRef names a message by conversation, author and sent timestamp.
type ThreadRef struct {
	Thread model.ThreadID
	model.MessageRef
}

// Purge deletes messages sent before `before` (ms), in one conversation or
// all, with their attachments, reactions and previews. With dryRun it only
// counts. Conversations themselves (titles, timers) are kept.
func (s *Store) Purge(ctx context.Context, before int64, thread model.ThreadID, dryRun bool) (PurgeResult, error) {
	var r PurgeResult
	where := `m.ts < $1`
	args := []any{before}
	if thread != "" {
		where += ` AND m.thread_id = $2`
		args = append(args, string(thread))
	}
	err := s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM sh_message m WHERE `+where, args...).Scan(&r.Messages); err != nil {
			return err
		}
		rows, err := s.db.Query(ctx, `SELECT a.path FROM sh_attachment a JOIN sh_message m ON m.id = a.message_id WHERE `+where, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return err
			}
			r.Attachments++
			if p != "" {
				r.Paths = append(r.Paths, p)
			}
		}
		rows.Close()
		rows, err = s.db.Query(ctx, `SELECT m.thread_id, m.author, m.ts FROM sh_message m WHERE `+where+` AND m.deleted = 0`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var ref ThreadRef
			if err := rows.Scan(&ref.Thread, &ref.Author, &ref.TS); err != nil {
				rows.Close()
				return err
			}
			r.Refs = append(r.Refs, ref)
		}
		rows.Close()
		rows, err = s.db.Query(ctx, `SELECT DISTINCT m.thread_id FROM sh_message m WHERE `+where, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var t model.ThreadID
			if err := rows.Scan(&t); err != nil {
				rows.Close()
				return err
			}
			r.Threads = append(r.Threads, t)
		}
		rows.Close()
		if dryRun {
			return nil
		}
		_, err = s.db.Exec(ctx, `DELETE FROM sh_message WHERE id IN (SELECT m.id FROM sh_message m WHERE `+where+`)`, args...)
		return err
	})
	return r, err
}

// Vacuum rebuilds the database file so space freed by deletions is returned.
func (s *Store) Vacuum(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `VACUUM`)
	return err
}

// RetryFailedAttachments marks every failed download for another try.
func (s *Store) RetryFailedAttachments(ctx context.Context) (int, error) {
	res, err := s.db.Exec(ctx, `UPDATE sh_attachment SET state = 'pending', error = '' WHERE state = 'failed' AND pointer IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DeleteByRef removes one message outright (no placeholder), returning it
// (with attachment paths) for file cleanup. ErrNotFound if absent.
func (s *Store) DeleteByRef(ctx context.Context, thread model.ThreadID, ref model.MessageRef) (*model.Message, error) {
	m, err := s.MessageByRef(ctx, thread, ref)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM sh_message WHERE id=$1`, m.ID); err != nil {
		return nil, err
	}
	return m, nil
}

// DeleteThread removes a conversation and everything in it.
func (s *Store) DeleteThread(ctx context.Context, thread model.ThreadID) error {
	_, err := s.db.Exec(ctx, `DELETE FROM sh_thread WHERE id=$1`, string(thread))
	return err
}
