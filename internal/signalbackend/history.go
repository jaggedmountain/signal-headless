// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package signalbackend

// Message-history transfer ("link and sync"). When the phone links a device
// that advertised the capability, it can offer "Transfer message history":
// it uploads an encrypted archive of its conversations, which signalmeow
// downloads and unpacks into its backup tables. Here we wait for that
// archive after linking, then turn each conversation into a HistoryEvent.
// The conversion follows mautrix-signal's msgconv/from-signal-backup.go,
// but produces our model directly.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/mautrix-signal/pkg/libsignalgo"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/backuppb"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/signalpb"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/store"

	"signal-headless/internal/backend"
	"signal-headless/internal/model"
)

// historyMediaWindow: transferred attachments older than this are not
// fetched automatically (Signal's transit storage has usually dropped them;
// the phone still has them). They can be retried by hand.
const historyMediaWindow = 45 * 24 * time.Hour

const historyPage = 1000

// runHistory waits for a pending transfer (if this device was just linked)
// and imports whatever the backup tables hold. It runs once per start.
func (b *Backend) runHistory(ctx context.Context) {
	status := func(s model.HistoryStatus) { _ = b.emit(ctx, backend.HistoryStatusEvent{Status: s}) }
	waited := b.dev.EphemeralBackupKey != nil
	if waited {
		status(model.HistoryStatus{State: "waiting"})
		b.log.Info().Msg("Waiting for the phone to transfer message history")
		meta, err := b.cli.WaitForTransfer(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			b.log.Err(err).Msg("History transfer")
			status(model.HistoryStatus{State: "failed", Error: err.Error()})
			return
		}
		if meta.Error != "" {
			// The phone skipped the transfer; don't wait again next start.
			b.log.Info().Str("reason", meta.Error).Msg("No message history transferred")
			b.dev.EphemeralBackupKey = nil
			if err := b.dev.DeviceStore.PutDevice(ctx, &b.dev.DeviceData); err != nil {
				b.log.Err(err).Msg("Saving device after declined transfer")
			}
			status(model.HistoryStatus{State: "declined", Error: meta.Error})
			return
		}
		status(model.HistoryStatus{State: "downloading"})
		if err := b.cli.FetchAndProcessTransfer(ctx, meta); err != nil {
			if ctx.Err() == nil {
				b.log.Err(err).Msg("History transfer")
				status(model.HistoryStatus{State: "failed", Error: err.Error()})
			}
			return
		}
	}
	chats, err := b.dev.BackupStore.GetBackupChats(ctx)
	if err != nil {
		b.log.Err(err).Msg("Reading transferred history")
		return
	}
	if len(chats) == 0 {
		if waited {
			status(model.HistoryStatus{State: "done"})
		}
		return
	}
	total := model.HistoryStatus{State: "importing"}
	status(total)
	conv := &historyConverter{b: b, ctx: ctx, recipients: map[uint64]*backuppb.Recipient{}}
	for _, chat := range chats {
		if ctx.Err() != nil {
			return
		}
		evt, err := conv.chat(chat)
		if err != nil {
			b.log.Warn().Err(err).Uint64("chat", chat.Id).Msg("Skipping transferred conversation")
		} else if evt != nil && len(evt.Messages) > 0 {
			if err := b.emit(ctx, *evt); err != nil {
				b.log.Err(err).Str("thread", string(evt.Thread)).Msg("Importing transferred conversation")
				status(model.HistoryStatus{State: "failed", Chats: total.Chats, Messages: total.Messages, Error: err.Error()})
				return
			}
			total.Chats++
			total.Messages += len(evt.Messages)
			status(total)
		}
		// Imported (or unusable): drop it, so an interrupted import resumes
		// with the rest.
		if err := b.dev.BackupStore.DeleteBackupChat(ctx, chat.Id); err != nil {
			b.log.Err(err).Msg("Deleting imported conversation from the backup store")
		}
	}
	if err := b.dev.BackupStore.ClearBackup(ctx); err != nil {
		b.log.Err(err).Msg("Clearing the backup store")
	}
	b.log.Info().Int("chats", total.Chats).Int("messages", total.Messages).Msg("Imported message history")
	total.State = "done"
	status(total)
}

type historyConverter struct {
	b          *Backend
	ctx        context.Context
	recipients map[uint64]*backuppb.Recipient
	now        time.Time // for tests; zero means time.Now()
}

func (c *historyConverter) recipient(id uint64) (*backuppb.Recipient, error) {
	if r, ok := c.recipients[id]; ok {
		return r, nil
	}
	r, err := c.b.dev.BackupStore.GetBackupRecipient(c.ctx, id)
	if err != nil {
		return nil, err
	}
	c.recipients[id] = r
	return r, nil
}

// aci returns the ACI string for a backup recipient id ("" if unknown).
func (c *historyConverter) aci(id uint64) string {
	r, err := c.recipient(id)
	if err != nil || r == nil {
		return ""
	}
	switch d := r.Destination.(type) {
	case *backuppb.Recipient_Self:
		return c.b.self()
	case *backuppb.Recipient_Contact:
		if u, ok := uuidOf(d.Contact.GetAci()); ok {
			return u.String()
		}
	}
	return ""
}

func (c *historyConverter) chat(chat *store.BackupChat) (*backend.HistoryEvent, error) {
	r, err := c.recipient(chat.RecipientId)
	if err != nil || r == nil {
		return nil, err
	}
	evt := &backend.HistoryEvent{
		Archived:      chat.Archived,
		ExpireTimer:   uint32(chat.GetExpirationTimerMs() / 1000),
		ExpireVersion: chat.GetExpireTimerVersion(),
	}
	switch d := r.Destination.(type) {
	case *backuppb.Recipient_Self:
		evt.Thread, evt.Kind = model.ThreadID(c.b.self()), model.Direct
	case *backuppb.Recipient_Contact:
		u, ok := uuidOf(d.Contact.GetAci())
		if !ok {
			return nil, nil // PNI-only or phone-number-only chats: nothing to thread on
		}
		evt.Thread, evt.Kind = model.ThreadID(u.String()), model.Direct
	case *backuppb.Recipient_Group:
		if len(d.Group.MasterKey) != libsignalgo.GroupMasterKeyLength {
			return nil, nil
		}
		gid, err := libsignalgo.GroupMasterKey(d.Group.MasterKey).GroupIdentifier()
		if err != nil {
			return nil, err
		}
		evt.Thread, evt.Kind = model.ThreadID(base64.StdEncoding.EncodeToString(gid[:])), model.Group
	default:
		return nil, nil // release notes, call links, distribution lists
	}
	var anchor time.Time
	for {
		items, err := c.b.dev.BackupStore.GetBackupChatItems(c.ctx, chat.Id, anchor, false, historyPage)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if m, ok := c.message(evt.Thread, it); ok {
				evt.Messages = append(evt.Messages, m)
			}
		}
		if len(items) < historyPage {
			break
		}
		anchor = time.UnixMilli(int64(items[len(items)-1].DateSent))
	}
	return evt, nil
}

func (c *historyConverter) nowTime() time.Time {
	if c.now.IsZero() {
		return time.Now()
	}
	return c.now
}

// message converts one chat item; ok is false for items we don't keep
// (deleted, expired, unknown author, nothing displayable).
func (c *historyConverter) message(thread model.ThreadID, it *backuppb.ChatItem) (model.Message, bool) {
	author := c.aci(it.AuthorId)
	if author == "" {
		return model.Message{}, false
	}
	now := c.nowTime()
	if exp := it.GetExpiresInMs(); exp > 0 {
		if start := it.GetExpireStartDate(); start > 0 && time.UnixMilli(int64(start+exp)).Before(now) {
			return model.Message{}, false
		}
	}
	m := model.Message{
		Thread: thread, Author: author, TS: int64(it.DateSent),
		ExpiresIn: int64(it.GetExpiresInMs() / 1000),
		// The phone's start, so the copy here disappears when the phone's does.
		ExpireStart: int64(it.GetExpireStartDate()),
		EditedAt:    0,
	}
	switch d := it.DirectionalDetails.(type) {
	case *backuppb.ChatItem_Incoming:
		m.ServerTS = int64(d.Incoming.GetDateServerSent())
		m.ReceivedAt = int64(d.Incoming.GetDateReceived())
		m.Read = d.Incoming.GetRead()
	case *backuppb.ChatItem_Outgoing:
		m.Outgoing = true
		m.Read = true
		m.Status = outgoingStatus(d.Outgoing.GetSendStatus())
	default:
		if author == c.b.self() {
			m.Outgoing, m.Read, m.Status = true, true, model.StatusSent
		} else {
			m.Read = true
		}
	}
	if len(it.Revisions) > 0 {
		m.EditedAt = m.TS
	}
	old := time.UnixMilli(m.TS).Before(now.Add(-historyMediaWindow))
	var reactions []*backuppb.Reaction
	switch ti := it.Item.(type) {
	case *backuppb.ChatItem_StandardMessage:
		sm := ti.StandardMessage
		reactions = sm.Reactions
		if t := sm.GetText(); t != nil {
			m.Body = renderBody(t.GetBody(), bodyRanges(t.GetBodyRanges()), c.b.mentionName(c.ctx))
		}
		if q := sm.GetQuote(); q != nil && q.TargetSentTimestamp != nil {
			m.Quote = &model.Quote{Author: c.aci(q.AuthorId), TS: int64(q.GetTargetSentTimestamp()), Text: q.GetText().GetBody()}
		}
		for _, a := range sm.GetAttachments() {
			m.Attachments = append(m.Attachments, historyAttachment(a.GetPointer(), a.GetFlag(), old))
		}
		if m.Body == "" && len(m.Attachments) == 0 && len(sm.GetLinkPreview()) > 0 {
			m.Body = sm.GetLinkPreview()[0].GetUrl()
		}
		for _, lp := range sm.GetLinkPreview() {
			var img *model.Attachment
			if lp.GetImage() != nil {
				a := historyAttachment(lp.GetImage(), 0, old)
				img = &a
			}
			addPreview(&m, lp.GetUrl(), lp.GetTitle(), lp.GetDescription(), int64(lp.GetDate()), img)
		}
	case *backuppb.ChatItem_StickerMessage:
		reactions = ti.StickerMessage.Reactions
		st := ti.StickerMessage.GetSticker()
		m.Sticker = st.GetEmoji()
		if m.Sticker == "" {
			m.Sticker = "sticker"
		}
		if st.GetData() != nil {
			a := historyAttachment(st.GetData(), 0, old)
			if a.Filename == "" {
				a.Filename = "sticker.webp"
			}
			m.Attachments = append(m.Attachments, a)
		}
	case *backuppb.ChatItem_ViewOnceMessage:
		reactions = ti.ViewOnceMessage.Reactions
		m.Body = "👁 view-once media (open it on the phone)"
	case *backuppb.ChatItem_ContactMessage:
		reactions = ti.ContactMessage.Reactions
		m.Body = "👤 shared contact (view on the phone)"
	case *backuppb.ChatItem_Poll:
		m.Body = "📊 poll: " + ti.Poll.GetQuestion() + " (vote on the phone)"
	case *backuppb.ChatItem_PaymentNotification:
		m.Body = "💸 payment (view on the phone)"
	default:
		// Remote-deleted and update messages, gift badges: nothing to show.
		return model.Message{}, false
	}
	if m.Body == "" && len(m.Attachments) == 0 && m.Sticker == "" {
		return model.Message{}, false
	}
	for _, r := range reactions {
		if who := c.aci(r.AuthorId); who != "" && r.Emoji != "" {
			m.Reactions = append(m.Reactions, model.Reaction{Reactor: who, Emoji: r.Emoji, TS: int64(r.SentTimestamp)})
		}
	}
	return m, true
}

func outgoingStatus(ss []*backuppb.SendStatus) model.Status {
	best := model.StatusSent
	failed := len(ss) > 0
	for _, s := range ss {
		st := model.StatusSent
		switch {
		case s.GetRead() != nil || s.GetViewed() != nil:
			st = model.StatusRead
		case s.GetDelivered() != nil:
			st = model.StatusDelivered
		case s.GetFailed() != nil:
			continue
		}
		failed = false
		if st.Rank() > best.Rank() {
			best = st
		}
	}
	if failed {
		return model.StatusFailed
	}
	return best
}

func bodyRanges(in []*backuppb.BodyRange) []*signalpb.BodyRange {
	var out []*signalpb.BodyRange
	for _, r := range in {
		start, length := r.GetStart(), r.GetLength()
		br := &signalpb.BodyRange{Start: &start, Length: &length}
		switch av := r.AssociatedValue.(type) {
		case *backuppb.BodyRange_MentionAci:
			br.AssociatedValue = &signalpb.BodyRange_MentionAciBinary{MentionAciBinary: av.MentionAci}
		case *backuppb.BodyRange_Style_:
			br.AssociatedValue = &signalpb.BodyRange_Style_{Style: signalpb.BodyRange_Style(av.Style)}
		default:
			continue
		}
		out = append(out, br)
	}
	return out
}

// historyAttachment makes a downloadable attachment from a backup file
// pointer when it still has a transit-CDN location; otherwise (or when old)
// it is recorded as failed with an explanation, pointer kept for a retry.
func historyAttachment(fp *backuppb.FilePointer, flag backuppb.MessageAttachment_Flag, old bool) model.Attachment {
	ap := &signalpb.AttachmentPointer{
		ContentType: fp.ContentType,
		FileName:    fp.FileName,
		Width:       fp.Width,
		Height:      fp.Height,
	}
	if flag == backuppb.MessageAttachment_VOICE_MESSAGE {
		f := uint32(signalpb.AttachmentPointer_VOICE_MESSAGE)
		ap.Flags = &f
	}
	var plaintextHash []byte
	li := fp.GetLocatorInfo()
	if li != nil {
		size := li.GetSize()
		ap.Size = &size
		ap.Key = li.GetKey()
		if li.TransitCdnKey != nil {
			ap.AttachmentIdentifier = &signalpb.AttachmentPointer_CdnKey{CdnKey: li.GetTransitCdnKey()}
			ap.CdnNumber = li.TransitCdnNumber
			ap.Digest = li.GetEncryptedDigest()
			if ap.Digest == nil {
				plaintextHash = li.GetPlaintextHash()
			}
		}
	}
	a := attachmentFromPointer(ap)
	a.Pointer = wrapPointer(a.Pointer, plaintextHash)
	switch {
	case ap.AttachmentIdentifier == nil || len(ap.Key) == 0:
		a.State, a.Error = model.AttachmentFailed, "not included in the transfer (open it on the phone)"
	case old:
		a.State, a.Error = model.AttachmentFailed, "older than 45 days; retry to try downloading it"
	}
	return a
}

// Pointers from a transfer may be verified by the plaintext hash instead of
// the encrypted digest; that hash rides in front of the protobuf.
var plaintextHashMagic = []byte("shph1:")

func wrapPointer(ptr, plaintextHash []byte) []byte {
	if len(plaintextHash) == 0 {
		return ptr
	}
	out := append([]byte{}, plaintextHashMagic...)
	out = append(out, byte(len(plaintextHash)))
	out = append(out, plaintextHash...)
	return append(out, ptr...)
}

func unwrapPointer(p []byte) (ptr, plaintextHash []byte, err error) {
	if !bytes.HasPrefix(p, plaintextHashMagic) {
		return p, nil, nil
	}
	rest := p[len(plaintextHashMagic):]
	if len(rest) < 1 || len(rest) < 1+int(rest[0]) {
		return nil, nil, errors.New("bad attachment pointer")
	}
	n := int(rest[0])
	return rest[1+n:], rest[1 : 1+n], nil
}

func uuidOf(b []byte) (uuid.UUID, bool) {
	if len(b) != 16 {
		return uuid.Nil, false
	}
	u := uuid.UUID(b)
	return u, u != uuid.Nil
}
