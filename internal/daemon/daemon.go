// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package daemon owns the Signal connection: it persists everything the
// backend reports and serves clients over the unix socket.
package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"signal-headless/internal/backend"
	"signal-headless/internal/history"
	"signal-headless/internal/model"
	"signal-headless/internal/rpc"
)

type Config struct {
	Socket         string
	AttachmentsDir string
	DBPath         string // for its size in stats
	Version        string
	// DeletedTTL is how long "This message was deleted." placeholders stay
	// before they are removed (default DefaultDeletedTTL).
	DeletedTTL time.Duration
	// LinkPreview fetches a preview for an outgoing link (nil: none).
	LinkPreview func(ctx context.Context, url string) (*model.OutgoingPreview, error)
	// SweepInterval is how often disappearing messages and old placeholders
	// are cleaned up (default 15s).
	SweepInterval time.Duration
}

// DefaultDeletedTTL keeps a deleted message's placeholder for an hour: long
// enough to notice something was deleted, not forever.
const DefaultDeletedTTL = time.Hour

type Daemon struct {
	cfg  Config
	be   backend.Backend
	hist *history.Store
	log  zerolog.Logger
	srv  *rpc.Server
	acct model.Account

	mu         sync.Mutex
	previews   map[string]cachedPreview // link previews fetched recently
	history    *model.HistoryStatus     // message-history transfer, if one ran
	conn       model.ConnState
	connErr    string
	queueEmpty bool
	lastTS     int64
	names      map[string]string // author ID → display name cache

	stop       context.CancelFunc
	attachWake chan struct{}
	// compatPending holds compat envelopes waiting for attachment downloads.
	compatPending map[int64]bool
}

func New(cfg Config, be backend.Backend, hist *history.Store, log zerolog.Logger) *Daemon {
	return &Daemon{
		cfg: cfg, be: be, hist: hist, log: log,
		acct:          be.Account(),
		conn:          model.ConnConnecting,
		names:         map[string]string{},
		attachWake:    make(chan struct{}, 1),
		compatPending: map[int64]bool{},
	}
}

// Run serves clients and runs the backend until ctx is cancelled or the
// backend stops (e.g. the device was unlinked).
func (d *Daemon) Run(ctx context.Context) error {
	srv, err := rpc.Listen(d.cfg.Socket, d.handle)
	if err != nil {
		return err
	}
	d.srv = srv
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	d.mu.Lock()
	d.stop = cancel
	d.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); _ = srv.Serve(ctx) }()
	go func() { defer wg.Done(); d.attachmentWorker(ctx) }()
	go func() { defer wg.Done(); d.expiryWorker(ctx) }()
	d.log.Info().Str("socket", d.cfg.Socket).Str("account", d.acct.Number).Int("device", d.acct.DeviceID).Msg("Daemon started")

	err = d.be.Run(ctx, d.onEvent)
	cancel()
	wg.Wait()
	return err
}

func (d *Daemon) broadcast(method string, params any) {
	if d.srv == nil {
		return
	}
	d.srv.Each(func(c *rpc.Conn) {
		if c.Native.Load() {
			c.Notify(method, params)
		}
	})
}

func (d *Daemon) broadcastCompat(env any) {
	if d.srv == nil {
		return
	}
	params := map[string]any{"envelope": env, "account": d.acct.Number}
	d.srv.Each(func(c *rpc.Conn) {
		if !c.Native.Load() {
			c.Notify(rpc.EvReceive, params)
		}
	})
}

// --- names and threads ---

func (d *Daemon) name(ctx context.Context, id string) string {
	d.mu.Lock()
	n, ok := d.names[id]
	d.mu.Unlock()
	if ok {
		return n
	}
	if id == d.acct.ACI {
		n = "me"
	} else if c, ok := d.be.Contact(ctx, id); ok {
		n = c.DisplayName()
		if n == c.ID {
			n = ""
		}
	}
	d.mu.Lock()
	d.names[id] = n
	d.mu.Unlock()
	return n
}

func (d *Daemon) decorate(ctx context.Context, m *model.Message) *model.Message {
	if m != nil && m.AuthorName == "" {
		m.AuthorName = d.name(ctx, m.Author)
	}
	return m
}

// ensureThread creates the thread record, looking up kind and title.
func (d *Daemon) ensureThread(ctx context.Context, id model.ThreadID) error {
	exists, err := d.hist.ThreadExists(ctx, id)
	if err != nil || exists {
		return err
	}
	kind, title, err := d.be.ThreadInfo(ctx, id)
	if err != nil {
		d.log.Debug().Err(err).Str("thread", string(id)).Msg("Thread info lookup failed")
		if kind == "" {
			kind = model.Direct
		}
	}
	return d.hist.EnsureThread(ctx, id, kind, title)
}

func (d *Daemon) thread(ctx context.Context, id model.ThreadID) (*model.Thread, error) {
	t, err := d.hist.Thread(ctx, id)
	if err != nil {
		return nil, err
	}
	d.fillThread(ctx, t)
	return t, nil
}

func (d *Daemon) fillThread(ctx context.Context, t *model.Thread) {
	t.NoteToSelf = string(t.ID) == d.acct.ACI
	if t.NoteToSelf {
		t.Title = "Note to Self"
	}
	if t.Title == "" && t.Kind == model.Direct {
		if c, ok := d.be.Contact(ctx, string(t.ID)); ok {
			t.Title = c.DisplayName()
		}
	}
	if t.Title == "" {
		t.Title = shortID(string(t.ID))
	}
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "PNI:")
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

func (d *Daemon) pushThread(ctx context.Context, id model.ThreadID) {
	if t, err := d.thread(ctx, id); err == nil {
		d.broadcast(rpc.EvThread, t)
	}
}

// --- backend events ---

func (d *Daemon) onEvent(ctx context.Context, evt backend.Event) error {
	switch e := evt.(type) {
	case backend.MessageEvent:
		return d.onMessage(ctx, e.Message)
	case backend.EditEvent:
		m, err := d.hist.ApplyEdit(ctx, e.Thread, e.Author, e.TargetTS, e.NewTS, e.Body)
		if errors.Is(err, history.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		d.broadcast(rpc.EvMessageUpdate, d.decorate(ctx, m))
		d.pushThread(ctx, e.Thread)
	case backend.DeleteEvent:
		return d.deleteMessage(ctx, e.Thread, e.Author, e.TargetTS)
	case backend.ReactionEvent:
		m, err := d.hist.ApplyReaction(ctx, e.Thread, e.Target, e.Reactor, e.Emoji, e.Remove, e.TS)
		if errors.Is(err, history.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		d.broadcast(rpc.EvMessageUpdate, d.decorate(ctx, m))
		d.broadcastCompat(d.compatReaction(ctx, e))
	case backend.ReceiptEvent:
		status := model.StatusDelivered
		if e.Kind != backend.ReceiptDelivered {
			status = model.StatusRead
		}
		changed, err := d.hist.ApplyReceipt(ctx, d.acct.ACI, status, e.Timestamps)
		if err != nil {
			return err
		}
		for _, m := range changed {
			if full, err := d.hist.Message(ctx, m.ID); err == nil {
				d.broadcast(rpc.EvMessageUpdate, d.decorate(ctx, full))
			}
		}
		d.broadcastCompat(d.compatReceipt(ctx, e))
	case backend.ReadSyncEvent:
		threads, err := d.hist.ApplyReadSync(ctx, e.Refs)
		if err != nil {
			return err
		}
		for _, t := range threads {
			d.pushThread(ctx, t)
		}
	case backend.TypingEvent:
		d.broadcast(rpc.EvTyping, rpc.TypingEvent{Thread: e.Thread, Sender: e.Sender, Name: d.name(ctx, e.Sender), Typing: e.Typing})
	case backend.HistoryEvent:
		return d.onHistory(ctx, e)
	case backend.DeleteForMeEvent:
		return d.onDeleteForMe(ctx, e)
	case backend.HistoryStatusEvent:
		d.mu.Lock()
		st := e.Status
		d.history = &st
		d.mu.Unlock()
		d.log.Info().Str("state", st.State).Int("chats", st.Chats).Int("messages", st.Messages).Str("error", st.Error).Msg("History transfer")
		d.broadcast(rpc.EvConnection, d.status())
	case backend.TimerEvent:
		if err := d.ensureThread(ctx, e.Thread); err != nil {
			return err
		}
		if err := d.hist.SetTimer(ctx, e.Thread, e.Seconds, e.Version); err != nil {
			return err
		}
		d.pushThread(ctx, e.Thread)
	case backend.ConnectionEvent:
		d.mu.Lock()
		d.conn, d.connErr = e.State, e.Err
		if e.State != model.ConnConnected {
			d.queueEmpty = false
		}
		d.mu.Unlock()
		lg := d.log.Info()
		if e.Err != "" {
			lg = d.log.Warn().Str("error", e.Err)
		}
		lg.Str("state", string(e.State)).Msg("Connection")
		d.broadcast(rpc.EvConnection, d.status())
	case backend.QueueEmptyEvent:
		d.mu.Lock()
		first := !d.queueEmpty
		d.queueEmpty = true
		d.mu.Unlock()
		if first {
			d.log.Info().Msg("Caught up with queued messages")
			d.broadcast(rpc.EvConnection, d.status())
		}
	case backend.ContactsEvent:
		d.mu.Lock()
		d.names = map[string]string{}
		d.mu.Unlock()
		d.refreshTitles(ctx)
		d.broadcast(rpc.EvContacts, nil)
	}
	return nil
}

func (d *Daemon) onMessage(ctx context.Context, m model.Message) error {
	if err := d.ensureThread(ctx, m.Thread); err != nil {
		return err
	}
	if m.Outgoing {
		m.Read = true
	}
	inserted, err := d.hist.InsertMessage(ctx, &m)
	if err != nil || !inserted {
		return err
	}
	d.log.Info().Str("thread", string(m.Thread)).Str("from", d.name(ctx, m.Author)).Bool("outgoing", m.Outgoing).
		Int("attachments", len(m.Attachments)).Msg("Message")
	full, err := d.hist.Message(ctx, m.ID)
	if err != nil {
		return err
	}
	d.broadcast(rpc.EvMessage, d.decorate(ctx, full))
	d.pushThread(ctx, m.Thread)
	if len(m.Attachments) > 0 {
		// signal-cli only announces a message once its attachments are on
		// disk; compat clients rely on that.
		d.mu.Lock()
		d.compatPending[m.ID] = true
		d.mu.Unlock()
		d.wakeAttachments()
	} else {
		d.broadcastCompat(d.compatEnvelope(ctx, full))
	}
	return nil
}

func (d *Daemon) deleteMessage(ctx context.Context, thread model.ThreadID, author string, ts int64) error {
	old, err := d.hist.MessageByRef(ctx, thread, model.MessageRef{Author: author, TS: ts})
	if errors.Is(err, history.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	m, err := d.hist.ApplyDelete(ctx, thread, author, ts)
	if err != nil {
		return err
	}
	d.removeFiles(old)
	d.broadcast(rpc.EvMessageUpdate, d.decorate(ctx, m))
	// Replies quoting it lose the quoted text too.
	ids, err := d.hist.ScrubQuotes(ctx, thread, author, ts)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if q, err := d.hist.Message(ctx, id); err == nil {
			d.broadcast(rpc.EvMessageUpdate, d.decorate(ctx, q))
		}
	}
	d.pushThread(ctx, thread)
	return nil
}

// removeFiles deletes downloaded attachments that live in our attachments dir.
func (d *Daemon) removeFiles(m *model.Message) {
	for _, a := range m.Attachments {
		if a.Path != "" && filepath.Dir(a.Path) == filepath.Clean(d.cfg.AttachmentsDir) {
			_ = os.Remove(a.Path)
		}
	}
}

func (d *Daemon) refreshTitles(ctx context.Context) {
	threads, err := d.hist.Threads(ctx)
	if err != nil {
		return
	}
	for _, t := range threads {
		kind, title, err := d.be.ThreadInfo(ctx, t.ID)
		if err != nil || title == "" || title == t.Title || kind != t.Kind {
			continue
		}
		if d.hist.SetTitle(ctx, t.ID, title) == nil {
			d.pushThread(ctx, t.ID)
		}
	}
}

func (d *Daemon) status() rpc.StatusResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	if d.srv != nil {
		n = d.srv.NumConns()
	}
	lp := true
	if s, ok := d.be.(interface{ LinkPreviewsEnabled() bool }); ok {
		lp = s.LinkPreviewsEnabled()
	}
	return rpc.StatusResult{Account: d.acct, Connection: d.conn, Error: d.connErr, QueueEmpty: d.queueEmpty, Clients: n, Version: d.cfg.Version, History: d.history, LinkPreviews: lp, Protocol: rpc.ProtocolVersion}
}

// onHistory stores one transferred conversation. Old messages are not
// announced one by one (no notifications, no signal-cli receive events);
// clients get the thread update and a "history" event to reload it.
func (d *Daemon) onHistory(ctx context.Context, e backend.HistoryEvent) error {
	if err := d.ensureThread(ctx, e.Thread); err != nil {
		return err
	}
	n, err := d.hist.ImportHistory(ctx, e.Messages)
	if err != nil {
		return err
	}
	if e.ExpireTimer > 0 || e.ExpireVersion > 0 {
		if err := d.hist.SetTimer(ctx, e.Thread, e.ExpireTimer, e.ExpireVersion); err != nil {
			return err
		}
	}
	if e.Archived {
		if err := d.hist.SetArchived(ctx, e.Thread, true); err != nil {
			return err
		}
	}
	d.log.Info().Str("thread", string(e.Thread)).Int("messages", n).Msg("Imported history")
	d.pushThread(ctx, e.Thread)
	d.broadcast(rpc.EvHistory, rpc.HistoryImported{Thread: e.Thread, Messages: n})
	d.wakeAttachments()
	return nil
}

// nextTS returns a unique, increasing millisecond timestamp for sending.
func (d *Daemon) nextTS() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	ts := time.Now().UnixMilli()
	if ts <= d.lastTS {
		ts = d.lastTS + 1
	}
	d.lastTS = ts
	return ts
}

// --- workers ---

func (d *Daemon) wakeAttachments() {
	select {
	case d.attachWake <- struct{}{}:
	default:
	}
}

func (d *Daemon) attachmentWorker(ctx context.Context) {
	attempts := map[string]int{}
	retry := time.NewTicker(5 * time.Minute)
	defer retry.Stop()
	d.wakeAttachments()
	again := false
	for {
		if !again {
			select {
			case <-ctx.Done():
				return
			case <-d.attachWake:
			case <-retry.C:
			}
		}
		again = false
		pend, err := d.hist.PendingAttachments(ctx)
		if err != nil {
			d.log.Err(err).Msg("Listing pending attachments")
			continue
		}
		touched := map[int64]bool{}
		for _, p := range pend {
			if ctx.Err() != nil {
				return
			}
			key := fmt.Sprintf("%d/%d", p.MessageID, p.Index)
			dest := filepath.Join(d.cfg.AttachmentsDir, attachmentName(p))
			dctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			err := d.be.DownloadAttachment(dctx, p.Pointer, dest)
			cancel()
			if err != nil {
				attempts[key]++
				d.log.Warn().Err(err).Str("attachment", key).Int("attempt", attempts[key]).Msg("Attachment download failed")
				if attempts[key] < 3 {
					continue
				}
				delete(attempts, key)
				err = d.hist.UpdateAttachment(ctx, p.MessageID, p.Index, model.AttachmentFailed, "", err.Error())
			} else {
				delete(attempts, key)
				err = d.hist.UpdateAttachment(ctx, p.MessageID, p.Index, model.AttachmentDone, dest, "")
			}
			if err != nil {
				d.log.Err(err).Msg("Updating attachment state")
			}
			touched[p.MessageID] = true
			// New attachments (newest first) jump a long backlog, such as
			// transferred history.
			select {
			case <-d.attachWake:
				again = true
			default:
			}
			if again {
				break
			}
		}
		for id := range touched {
			m, err := d.hist.Message(ctx, id)
			if err != nil {
				continue
			}
			d.broadcast(rpc.EvMessageUpdate, d.decorate(ctx, m))
			if !hasPending(m) {
				d.mu.Lock()
				want := d.compatPending[id]
				delete(d.compatPending, id)
				d.mu.Unlock()
				if want {
					d.broadcastCompat(d.compatEnvelope(ctx, m))
				}
			}
		}
	}
}

func hasPending(m *model.Message) bool {
	for _, a := range m.Attachments {
		if a.State == model.AttachmentPending {
			return true
		}
	}
	return false
}

// attachmentName builds a unique, readable file name:
// <ts>-<message row id>-<index>-<name>.
func attachmentName(p history.PendingAttachment) string {
	name := sanitize(p.Filename)
	if name == "" {
		name = "attachment" + extFor(p.ContentType)
	}
	return fmt.Sprintf("%d-%d-%d-%s", p.TS, p.MessageID, p.Index, name)
}

func sanitize(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "." || name == "/" || name == ".." {
		return ""
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == '/' || r == '\\' || r == 0x7f {
			return '_'
		}
		return r
	}, name)
	if len(name) > 120 {
		ext := filepath.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		name = name[:120-len(ext)] + ext
	}
	return name
}

var extByType = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp", "image/heic": ".heic",
	"video/mp4": ".mp4", "video/quicktime": ".mov", "audio/aac": ".aac", "audio/mp4": ".m4a", "audio/mpeg": ".mp3",
	"audio/ogg": ".ogg", "application/pdf": ".pdf", "text/plain": ".txt", "text/x-signal-plain": ".txt",
}

func extFor(ct string) string {
	if e, ok := extByType[strings.ToLower(ct)]; ok {
		return e
	}
	return ""
}

func (d *Daemon) expiryWorker(ctx context.Context) {
	every := d.cfg.SweepInterval
	if every <= 0 {
		every = 15 * time.Second
		if ttl := d.cfg.DeletedTTL; ttl > 0 && ttl < 2*every {
			every = ttl / 2
		}
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		msgs, err := d.hist.Expired(ctx, time.Now().UnixMilli())
		if err != nil {
			d.log.Err(err).Msg("Expiry sweep")
			continue
		}
		for _, m := range msgs {
			if err := d.deleteMessage(ctx, m.Thread, m.Author, m.TS); err != nil {
				d.log.Err(err).Msg("Expiring message")
			}
		}
		d.purgeDeleted(ctx)
	}
}

// purgeDeleted removes deleted-message placeholders older than DeletedTTL
// and tells clients to drop them.
func (d *Daemon) purgeDeleted(ctx context.Context) {
	ttl := d.cfg.DeletedTTL
	if ttl <= 0 {
		ttl = DefaultDeletedTTL
	}
	removed, err := d.hist.PurgeDeleted(ctx, time.Now().Add(-ttl).UnixMilli())
	if err != nil {
		d.log.Err(err).Msg("Purging deleted messages")
		return
	}
	threads := map[model.ThreadID]bool{}
	for _, r := range removed {
		d.broadcast(rpc.EvMessageRemoved, rpc.MessageRemoved{ID: r.ID, Thread: r.Thread})
		threads[r.Thread] = true
	}
	for t := range threads {
		d.pushThread(ctx, t)
	}
	if len(removed) > 0 {
		d.log.Debug().Int("count", len(removed)).Msg("Purged deleted-message placeholders")
	}
}

// --- RPC ---

func decode(params json.RawMessage, v any) error {
	if len(params) == 0 || string(params) == "null" {
		return nil
	}
	if err := json.Unmarshal(params, v); err != nil {
		return rpc.Errorf(rpc.CodeInvalidParams, "invalid params: %v", err)
	}
	return nil
}

func (d *Daemon) handle(ctx context.Context, c *rpc.Conn, method string, params json.RawMessage) (any, error) {
	switch method {
	case rpc.MVersion:
		return rpc.VersionResult{Version: d.cfg.Version}, nil
	case rpc.MStatus:
		return d.status(), nil
	case rpc.MSubscribe:
		c.Native.Store(true)
		return d.status(), nil
	case rpc.MListThreads:
		ts, err := d.hist.Threads(ctx)
		if err != nil {
			return nil, err
		}
		for i := range ts {
			d.fillThread(ctx, &ts[i])
		}
		if ts == nil {
			ts = []model.Thread{}
		}
		return ts, nil
	case rpc.MGetThread:
		var p rpc.ThreadParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return d.thread(ctx, p.Thread)
	case rpc.MGetMessages:
		var p rpc.GetMessagesParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		msgs, err := d.hist.Messages(ctx, p.Thread, p.Before, p.Limit)
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			d.decorate(ctx, m)
		}
		if msgs == nil {
			msgs = []*model.Message{}
		}
		return msgs, nil
	case rpc.MSearch:
		var p rpc.SearchParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		msgs, err := d.hist.Search(ctx, p.Thread, p.Query, p.Limit)
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			d.decorate(ctx, m)
		}
		if msgs == nil {
			msgs = []*model.Message{}
		}
		return msgs, nil
	case rpc.MSend:
		return d.rpcSend(ctx, params)
	case rpc.MSendReaction:
		return d.rpcReaction(ctx, params)
	case rpc.MRemoteDelete:
		return d.rpcDelete(ctx, params)
	case rpc.MSendTyping:
		var p rpc.TypingParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return struct{}{}, d.be.SendTyping(ctx, p.Thread, p.Typing)
	case rpc.MMarkRead:
		var p rpc.ThreadParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return struct{}{}, d.markRead(ctx, p.Thread)
	case rpc.MArchive:
		var p rpc.ArchiveParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		if err := d.hist.SetArchived(ctx, p.Thread, p.Archived); err != nil {
			return nil, err
		}
		d.pushThread(ctx, p.Thread)
		return struct{}{}, nil
	case rpc.MListContacts:
		cs, err := d.be.Contacts(ctx)
		if cs == nil {
			cs = []model.Contact{}
		}
		return cs, err
	case rpc.MListGroups:
		gs, err := d.be.Groups(ctx)
		if gs == nil {
			gs = []model.GroupInfo{}
		}
		return gs, err
	case rpc.MResolve:
		var p rpc.ResolveParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		id, err := d.resolve(ctx, p.Recipient)
		if err != nil {
			return nil, err
		}
		if err := d.ensureThread(ctx, id); err != nil {
			return nil, err
		}
		t, err := d.thread(ctx, id)
		if err != nil {
			return nil, err
		}
		return rpc.ResolveResult{Thread: id, Title: t.Title}, nil
	case rpc.MRetry:
		var p rpc.RetryParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		m, err := d.hist.Message(ctx, p.MessageID)
		if err != nil {
			return nil, err
		}
		for _, a := range m.Attachments {
			if a.State == model.AttachmentFailed {
				_ = d.hist.UpdateAttachment(ctx, m.ID, a.Index, model.AttachmentPending, "", "")
			}
		}
		d.wakeAttachments()
		return struct{}{}, nil
	case rpc.MShutdown:
		d.mu.Lock()
		stop := d.stop
		d.mu.Unlock()
		d.log.Info().Msg("Shutdown requested by a client")
		// Let the reply reach the client before the socket closes.
		time.AfterFunc(300*time.Millisecond, stop)
		return struct{}{}, nil
	case rpc.MUnlink:
		var p rpc.UnlinkParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		if p.Number != d.acct.Number {
			return nil, rpc.Errorf(rpc.CodeInvalidParams, "confirmation %q does not match the account number", p.Number)
		}
		if err := d.be.Unlink(ctx); err != nil {
			return nil, err
		}
		d.mu.Lock()
		d.conn, d.connErr = model.ConnLoggedOut, "device unlinked"
		stop := d.stop
		d.mu.Unlock()
		d.broadcast(rpc.EvConnection, d.status())
		d.log.Warn().Msg("Unlinked; shutting down")
		// Let the reply reach the client before the socket closes.
		time.AfterFunc(500*time.Millisecond, stop)
		return struct{}{}, nil
	case rpc.MStats:
		return d.stats(ctx)
	case rpc.MPurge:
		var p rpc.PurgeParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return d.purge(ctx, p)
	case rpc.MRetryFailed:
		n, err := d.hist.RetryFailedAttachments(ctx)
		if err != nil {
			return nil, err
		}
		d.wakeAttachments()
		return rpc.RetryFailedResult{Count: n}, nil
	case rpc.MLinkPreview:
		var p rpc.LinkPreviewParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return d.linkPreview(ctx, p.URL)
	case "debugInject":
		// Development only: the fake backend can inject incoming messages.
		inj, ok := d.be.(interface{ Inject(backend.Event) error })
		if !ok {
			return nil, rpc.Errorf(rpc.CodeMethodNotFound, "method not found: %s", method)
		}
		var p struct {
			Message        model.Message `json:"message"`
			AttachmentData []string      `json:"attachmentData"`
		}
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		for i := range p.Message.Attachments {
			if i < len(p.AttachmentData) {
				data := p.AttachmentData[i]
				// "base64:…" carries binary content (the fake backend's
				// download writes the pointer bytes as the file).
				if b64, ok := strings.CutPrefix(data, "base64:"); ok {
					raw, err := base64.StdEncoding.DecodeString(b64)
					if err != nil {
						return nil, rpc.Errorf(rpc.CodeInvalidParams, "attachmentData %d: %v", i, err)
					}
					data = string(raw)
				}
				p.Message.Attachments[i].Pointer = []byte(data)
			}
		}
		if p.Message.TS == 0 {
			p.Message.TS = d.nextTS()
		}
		return struct{}{}, inj.Inject(backend.MessageEvent{Message: p.Message})
	case "listIdentities", "updateProfile", "listDevices", "getUserStatus", "sendReceipt", "subscribeReceive", "unsubscribeReceive":
		// signal-cli methods clients commonly call; accepted as no-ops.
		return []any{}, nil
	}
	return nil, rpc.Errorf(rpc.CodeMethodNotFound, "method not found: %s", method)
}

func (d *Daemon) markRead(ctx context.Context, thread model.ThreadID) error {
	refs, err := d.hist.MarkThreadRead(ctx, thread)
	if err != nil {
		return err
	}
	d.pushThread(ctx, thread)
	if len(refs) > 0 {
		go func() {
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			if err := d.be.MarkRead(sctx, thread, refs); err != nil {
				d.log.Warn().Err(err).Msg("Sending read receipts")
			}
		}()
	}
	return nil
}

// Send records the message as "sending", delivers it, and updates its status.
func (d *Daemon) Send(ctx context.Context, out model.Outgoing) (*model.Message, error) {
	if strings.TrimSpace(out.Body) == "" && len(out.Attachments) == 0 {
		return nil, rpc.Errorf(rpc.CodeInvalidParams, "nothing to send")
	}
	for i, p := range out.Attachments {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		st, err := os.Stat(abs)
		if err != nil {
			return nil, rpc.Errorf(rpc.CodeInvalidParams, "attachment: %v", err)
		}
		if st.IsDir() {
			return nil, rpc.Errorf(rpc.CodeInvalidParams, "attachment %s is a directory", abs)
		}
		out.Attachments[i] = abs
	}
	if err := d.ensureThread(ctx, out.Thread); err != nil {
		return nil, err
	}
	var err error
	out.ExpireTimer, out.ExpireVersion, err = d.hist.Timer(ctx, out.Thread)
	if err != nil {
		return nil, err
	}
	out.TS = d.nextTS()
	m := &model.Message{
		Thread: out.Thread, Author: d.acct.ACI, TS: out.TS, Outgoing: true, Status: model.StatusSending,
		Body: out.Body, Quote: out.Quote, ExpiresIn: int64(out.ExpireTimer),
	}
	for _, p := range out.Attachments {
		st, _ := os.Stat(p)
		m.Attachments = append(m.Attachments, model.Attachment{
			Filename: filepath.Base(p), Path: p, Size: st.Size(), State: model.AttachmentDone,
			ContentType: contentTypeFor(p),
		})
	}
	// Link previews: kept only for an https link in the text, like Signal's
	// apps (a bad one is dropped, not an error).
	previews := out.Previews[:0:0]
	for _, p := range out.Previews {
		if !strings.HasPrefix(p.URL, "https://") || !strings.Contains(out.Body, p.URL) {
			continue
		}
		lp := model.LinkPreview{URL: p.URL, Title: p.Title, Description: p.Description, Date: p.Date, Image: -1}
		if p.Image != "" {
			if st, err := os.Stat(p.Image); err == nil && !st.IsDir() {
				lp.Image = len(m.Attachments)
				m.Attachments = append(m.Attachments, model.Attachment{
					Filename: "preview" + filepath.Ext(p.Image), Path: p.Image, Size: st.Size(), State: model.AttachmentDone,
					ContentType: contentTypeFor(p.Image), Kind: model.AttachmentPreview,
				})
			} else {
				p.Image = ""
			}
		}
		m.Previews = append(m.Previews, lp)
		previews = append(previews, p)
	}
	out.Previews = previews
	if _, err := d.hist.InsertMessage(ctx, m); err != nil {
		return nil, err
	}
	full, _ := d.hist.Message(ctx, m.ID)
	d.broadcast(rpc.EvMessage, d.decorate(ctx, full))
	d.pushThread(ctx, out.Thread)

	sendErr := d.be.Send(ctx, out)
	status := model.StatusSent
	if sendErr != nil {
		status = model.StatusFailed
		d.log.Warn().Err(sendErr).Str("thread", string(out.Thread)).Msg("Send failed")
	} else {
		d.log.Info().Str("thread", string(out.Thread)).Int("attachments", len(out.Attachments)).Msg("Sent")
	}
	// A receipt may already have advanced the status; only move forward.
	if cur, err := d.hist.Message(ctx, m.ID); err == nil && (status == model.StatusFailed || status.Rank() > cur.Status.Rank()) {
		_ = d.hist.SetStatus(ctx, m.ID, status)
	}
	full, err = d.hist.Message(ctx, m.ID)
	if err == nil {
		d.broadcast(rpc.EvMessageUpdate, d.decorate(ctx, full))
	}
	if sendErr != nil {
		return full, sendErr
	}
	return full, nil
}

type cachedPreview struct {
	p   *model.OutgoingPreview
	err error
	at  time.Time
}

// linkPreview fetches (or recalls) the preview for an outgoing link. Results
// are cached briefly: every open compose box asks for the same link.
func (d *Daemon) linkPreview(ctx context.Context, url string) (*model.OutgoingPreview, error) {
	if d.cfg.LinkPreview == nil {
		return nil, rpc.Errorf(rpc.CodeFailed, "link previews are not available")
	}
	d.mu.Lock()
	c, ok := d.previews[url]
	d.mu.Unlock()
	if ok && time.Since(c.at) < 10*time.Minute {
		if c.p != nil && c.p.Image != "" {
			if _, err := os.Stat(c.p.Image); err != nil {
				ok = false // image removed since; fetch again
			}
		}
		if ok {
			return c.p, c.err
		}
	}
	fctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	p, err := d.cfg.LinkPreview(fctx, url)
	if err != nil {
		d.log.Debug().Err(err).Str("url", url).Msg("No link preview")
		err = rpc.Errorf(rpc.CodeFailed, "no preview: %v", err)
	}
	d.mu.Lock()
	if d.previews == nil {
		d.previews = map[string]cachedPreview{}
	}
	for k, v := range d.previews {
		if time.Since(v.at) > 10*time.Minute {
			delete(d.previews, k)
		}
	}
	d.previews[url] = cachedPreview{p: p, err: err, at: time.Now()}
	d.mu.Unlock()
	return p, err
}

func fileSize(p string) int64 {
	if st, err := os.Stat(p); err == nil {
		return st.Size()
	}
	return 0
}

func (d *Daemon) dbBytes() int64 {
	if d.cfg.DBPath == "" {
		return 0
	}
	return fileSize(d.cfg.DBPath) + fileSize(d.cfg.DBPath+"-wal")
}

func (d *Daemon) stats(ctx context.Context) (rpc.StatsResult, error) {
	st, err := d.hist.Stats(ctx)
	if err != nil {
		return rpc.StatsResult{}, err
	}
	r := rpc.StatsResult{Stats: st, DataDir: filepath.Dir(d.cfg.AttachmentsDir), DBBytes: d.dbBytes()}
	entries, _ := os.ReadDir(d.cfg.AttachmentsDir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			r.AttachmentFiles++
			r.AttachmentBytes += info.Size()
		}
	}
	return r, nil
}

// purge deletes old local history, removes the attachment files, compacts
// the database and tells clients to reload the affected conversations.
func (d *Daemon) purge(ctx context.Context, p rpc.PurgeParams) (rpc.PurgeResult, error) {
	if p.Before <= 0 {
		return rpc.PurgeResult{}, rpc.Errorf(rpc.CodeInvalidParams, "purge needs a cutoff time")
	}
	if p.AllDevices && !p.DryRun {
		// Tell the other devices first: if that fails, nothing is deleted.
		plan, err := d.hist.Purge(ctx, p.Before, p.Thread, true)
		if err != nil {
			return rpc.PurgeResult{}, err
		}
		if len(plan.Refs) > 0 {
			deletes := make([]backend.MessageDelete, len(plan.Refs))
			for i, r := range plan.Refs {
				deletes[i] = backend.MessageDelete{Thread: r.Thread, Ref: r.MessageRef}
			}
			if err := d.be.DeleteForMe(ctx, deletes); err != nil {
				return rpc.PurgeResult{}, rpc.Errorf(rpc.CodeFailed, "not deleted anywhere: telling the other devices failed: %v", err)
			}
			d.log.Info().Int("messages", len(deletes)).Msg("Sent delete-for-me to our other devices")
		}
	}
	res, err := d.hist.Purge(ctx, p.Before, p.Thread, p.DryRun)
	if err != nil {
		return rpc.PurgeResult{}, err
	}
	out := rpc.PurgeResult{PurgeResult: res}
	own := filepath.Clean(d.cfg.AttachmentsDir)
	for _, path := range res.Paths {
		if filepath.Dir(path) != own {
			continue // files we sent from elsewhere stay where they are
		}
		out.Bytes += fileSize(path)
		if !p.DryRun {
			_ = os.Remove(path)
		}
	}
	if p.DryRun {
		return out, nil
	}
	if res.Messages > 0 {
		if err := d.hist.Vacuum(ctx); err != nil {
			d.log.Warn().Err(err).Msg("Compacting the database after purge")
		}
	}
	out.DBBytesAfter = d.dbBytes()
	d.log.Info().Int("messages", res.Messages).Int("attachments", res.Attachments).Int64("before", p.Before).Str("thread", string(p.Thread)).Msg("Purged history")
	for _, t := range res.Threads {
		d.pushThread(ctx, t)
		d.broadcast(rpc.EvHistory, rpc.HistoryImported{Thread: t})
	}
	return out, nil
}

// onDeleteForMe applies a "delete for me" from another of our devices:
// the messages (or conversations) go, without a placeholder.
func (d *Daemon) onDeleteForMe(ctx context.Context, e backend.DeleteForMeEvent) error {
	touched := map[model.ThreadID]bool{}
	for _, md := range e.Messages {
		m, err := d.hist.DeleteByRef(ctx, md.Thread, md.Ref)
		if errors.Is(err, history.ErrNotFound) {
			continue
		} else if err != nil {
			return err
		}
		d.removeFiles(m)
		d.broadcast(rpc.EvMessageRemoved, rpc.MessageRemoved{ID: m.ID, Thread: md.Thread})
		touched[md.Thread] = true
	}
	removedThreads := false
	for _, cd := range e.Conversations {
		before := cd.Through + 1
		if cd.Through == 0 {
			before = math.MaxInt64
		}
		res, err := d.hist.Purge(ctx, before, cd.Thread, false)
		if err != nil {
			return err
		}
		for _, p := range res.Paths {
			if filepath.Dir(p) == filepath.Clean(d.cfg.AttachmentsDir) {
				_ = os.Remove(p)
			}
		}
		if cd.Full {
			if err := d.hist.DeleteThread(ctx, cd.Thread); err != nil {
				return err
			}
			removedThreads = true
		} else {
			touched[cd.Thread] = true
			d.broadcast(rpc.EvHistory, rpc.HistoryImported{Thread: cd.Thread})
		}
	}
	for t := range touched {
		d.pushThread(ctx, t)
	}
	if removedThreads {
		d.broadcast(rpc.EvContacts, nil) // clients refetch the thread list
	}
	d.log.Info().Int("messages", len(e.Messages)).Int("conversations", len(e.Conversations)).Msg("Deleted for me (from another device)")
	return nil
}
