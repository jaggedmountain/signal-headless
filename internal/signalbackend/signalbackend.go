// Package signalbackend implements backend.Backend on top of signalmeow.
package signalbackend

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/mautrix-signal/pkg/libsignalgo"
	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/events"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/signalpb"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/store"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/types"

	"signal-headless/internal/backend"
	"signal-headless/internal/db"
	"signal-headless/internal/model"
)

// ErrNoDevice means no linked device is stored yet.
var ErrNoDevice = errors.New("no linked Signal device; run --link or --import-signal-cli first")

type Backend struct {
	db  *db.DB
	dev *store.Device
	cli *signalmeow.Client
	log zerolog.Logger

	mu       sync.Mutex
	handler  backend.Handler
	runCtx   context.Context
	groupRev map[types.GroupIdentifier]uint32
	// forbidden caches groups we are no longer a member of (HTTP 403).
	forbidden map[types.GroupIdentifier]time.Time
}

var _ backend.Backend = (*Backend)(nil)

// LoadDevice returns the single stored device, or ErrNoDevice.
func LoadDevice(ctx context.Context, d *db.DB) (*store.Device, error) {
	devs, err := d.Signal.GetAllDevices(ctx)
	if err != nil {
		return nil, err
	}
	switch len(devs) {
	case 0:
		return nil, ErrNoDevice
	case 1:
		return devs[0], nil
	default:
		return nil, fmt.Errorf("%d devices in store; only one is supported", len(devs))
	}
}

func New(ctx context.Context, d *db.DB, log zerolog.Logger) (*Backend, error) {
	dev, err := LoadDevice(ctx, d)
	if err != nil {
		return nil, err
	}
	signalmeow.SetLogger(log.With().Str("component", "libsignal").Logger().Level(zerolog.WarnLevel))
	b := &Backend{db: d, dev: dev, log: log, groupRev: map[types.GroupIdentifier]uint32{}, forbidden: map[types.GroupIdentifier]time.Time{}}
	b.cli = signalmeow.NewClient(dev, log.With().Str("component", "signalmeow").Logger(), b.onEvent)
	b.cli.SyncContactsOnConnect = true
	if n, err := b.pruneDeadSessions(ctx); err != nil {
		return nil, fmt.Errorf("prune sessions: %w", err)
	} else if n > 0 {
		log.Info().Int("count", n).Msg("Pruned sessions without current state")
	}
	return b, nil
}

func (b *Backend) Account() model.Account {
	return model.Account{
		ACI:      b.dev.ACI.String(),
		PNI:      b.dev.PNIServiceID().String(),
		Number:   b.dev.Number,
		DeviceID: b.dev.DeviceID,
	}
}

func (b *Backend) self() string { return b.dev.ACI.String() }

// Run connects and blocks until ctx is done or the device is logged out.
func (b *Backend) Run(ctx context.Context, h backend.Handler) error {
	ctx = b.log.WithContext(ctx)
	b.mu.Lock()
	b.handler, b.runCtx = h, ctx
	b.mu.Unlock()

	emit := func(e backend.Event) { _ = h(ctx, e) }
	emit(backend.ConnectionEvent{State: model.ConnConnecting})

	var statusChan chan signalmeow.SignalConnectionStatus
	for attempt := 0; ; attempt++ {
		var err error
		statusChan, err = b.cli.StartReceiveLoops(ctx)
		if err == nil {
			break
		}
		delay := min(time.Duration(2<<min(attempt, 6))*time.Second, 2*time.Minute)
		b.log.Err(err).Dur("retry_in", delay).Msg("Failed to start receive loops")
		emit(backend.ConnectionEvent{State: model.ConnError, Err: err.Error()})
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
	defer func() {
		if err := b.cli.StopReceiveLoops(); err != nil {
			b.log.Debug().Err(err).Msg("Stopping receive loops")
		}
	}()

	synced := false
	for {
		select {
		case <-ctx.Done():
			return nil
		case st, ok := <-statusChan:
			if !ok {
				return nil
			}
			var errStr string
			if st.Err != nil {
				errStr = st.Err.Error()
			}
			switch st.Event {
			case signalmeow.SignalConnectionEventConnected:
				emit(backend.ConnectionEvent{State: model.ConnConnected})
				if !synced && b.dev.MasterKey != nil {
					// Storage service holds contact names and the account
					// record, whose read-receipt/typing settings signalmeow
					// honours when sending.
					synced = true
					go b.cli.SyncStorage(ctx)
				}
			case signalmeow.SignalConnectionEventDisconnected:
				emit(backend.ConnectionEvent{State: model.ConnDisconnected, Err: errStr})
			case signalmeow.SignalConnectionEventLoggedOut:
				emit(backend.ConnectionEvent{State: model.ConnLoggedOut, Err: errStr})
				return fmt.Errorf("logged out by Signal: %s", errStr)
			case signalmeow.SignalConnectionEventError, signalmeow.SignalConnectionEventFatalError:
				emit(backend.ConnectionEvent{State: model.ConnError, Err: errStr})
			case signalmeow.SignalConnectionCleanShutdown:
				emit(backend.ConnectionEvent{State: model.ConnDisconnected})
			}
		}
	}
}

// onEvent is signalmeow's synchronous event callback. Returning false makes
// signalmeow leave the envelope un-acked so the server redelivers it.
func (b *Backend) onEvent(evt events.SignalEvent) bool {
	b.mu.Lock()
	h, ctx := b.handler, b.runCtx
	b.mu.Unlock()
	if h == nil {
		return false
	}
	var out []backend.Event
	switch e := evt.(type) {
	case *events.ChatEvent:
		out = b.translateChat(ctx, e)
	case *events.Receipt:
		kind := backend.ReceiptDelivered
		switch e.Content.GetType() {
		case signalpb.ReceiptMessage_READ:
			kind = backend.ReceiptRead
		case signalpb.ReceiptMessage_VIEWED:
			kind = backend.ReceiptViewed
		}
		out = append(out, backend.ReceiptEvent{From: e.Sender.String(), Kind: kind, Timestamps: toInt64s(e.Content.GetTimestamp())})
	case *events.ReadSelf:
		var refs []model.MessageRef
		for _, r := range e.Messages {
			aci, err := signalmeow.ParseStringOrBinaryUUID(r.GetSenderAci(), r.GetSenderAciBinary())
			if err != nil {
				continue
			}
			refs = append(refs, model.MessageRef{Author: aci.String(), TS: int64(r.GetTimestamp())})
		}
		out = append(out, backend.ReadSyncEvent{Refs: refs})
	case *events.ContactList:
		out = append(out, backend.ContactsEvent{})
	case *events.QueueEmpty:
		out = append(out, backend.QueueEmptyEvent{})
	case *events.LoggedOut:
		msg := ""
		if e.Error != nil {
			msg = e.Error.Error()
		}
		out = append(out, backend.ConnectionEvent{State: model.ConnLoggedOut, Err: msg})
	case *events.DecryptionError:
		b.log.Warn().Err(e.Err).Stringer("sender", e.Sender).Uint64("ts", e.Timestamp).Msg("Decryption error")
	case *events.Call:
		// Calls aren't supported; surface them as a note in the thread.
		if e.IsRinging {
			out = append(out, backend.MessageEvent{Message: model.Message{
				Thread: model.ThreadID(e.Info.ChatID), Author: e.Info.Sender.String(),
				TS: int64(e.Timestamp), ServerTS: int64(e.Info.ServerTimestamp),
				Body: "📞 incoming call (answer on the phone)",
			}})
		}
	default:
		b.log.Debug().Type("event", evt).Msg("Ignoring event")
	}
	for _, e := range out {
		if err := h(ctx, e); err != nil {
			b.log.Err(err).Type("event", e).Msg("Handler failed; message will be redelivered")
			return false
		}
	}
	return true
}

func toInt64s(in []uint64) []int64 {
	out := make([]int64, len(in))
	for i, v := range in {
		out[i] = int64(v)
	}
	return out
}

func (b *Backend) translateChat(ctx context.Context, e *events.ChatEvent) []backend.Event {
	thread := model.ThreadID(e.Info.ChatID)
	sender := e.Info.Sender.String()
	switch c := e.Event.(type) {
	case *signalpb.DataMessage:
		b.noteGroupRevision(thread, e.Info.GroupRevision)
		return b.translateData(ctx, thread, sender, int64(e.Info.ServerTimestamp), c)
	case *signalpb.EditMessage:
		dm := c.GetDataMessage()
		return []backend.Event{backend.EditEvent{
			Thread: thread, Author: sender, TargetTS: int64(c.GetTargetSentTimestamp()),
			NewTS: int64(dm.GetTimestamp()), Body: renderBody(dm.GetBody(), dm.GetBodyRanges(), b.mentionName(ctx)),
		}}
	case *signalpb.TypingMessage:
		return []backend.Event{backend.TypingEvent{
			Thread: thread, Sender: sender, Typing: c.GetAction() == signalpb.TypingMessage_STARTED,
		}}
	}
	return nil
}

// noteGroupRevision remembers the newest revision seen per group so that
// RetrieveGroupByID refetches metadata after a change.
func (b *Backend) noteGroupRevision(thread model.ThreadID, rev uint32) {
	if rev == 0 {
		return
	}
	if _, gid, err := parseThread(thread); err == nil && gid != "" {
		b.mu.Lock()
		if rev > b.groupRev[gid] {
			b.groupRev[gid] = rev
		}
		b.mu.Unlock()
	}
}

func (b *Backend) translateData(ctx context.Context, thread model.ThreadID, sender string, serverTS int64, dm *signalpb.DataMessage) []backend.Event {
	ts := int64(dm.GetTimestamp())
	if r := dm.GetReaction(); r != nil {
		aci, err := signalmeow.ParseStringOrBinaryUUID(r.GetTargetAuthorAci(), r.GetTargetAuthorAciBinary())
		if err != nil {
			return nil
		}
		return []backend.Event{backend.ReactionEvent{
			Thread: thread, Reactor: sender, Emoji: r.GetEmoji(), Remove: r.GetRemove(), TS: ts,
			Target: model.MessageRef{Author: aci.String(), TS: int64(r.GetTargetSentTimestamp())},
		}}
	}
	if d := dm.GetDelete(); d != nil {
		return []backend.Event{backend.DeleteEvent{Thread: thread, Author: sender, TargetTS: int64(d.GetTargetSentTimestamp())}}
	}
	var evts []backend.Event
	flags := dm.GetFlags()
	if dm.ExpireTimer != nil || flags&uint32(signalpb.DataMessage_EXPIRATION_TIMER_UPDATE) != 0 {
		evts = append(evts, backend.TimerEvent{Thread: thread, Seconds: dm.GetExpireTimer(), Version: dm.GetExpireTimerVersion()})
	}
	if flags&uint32(signalpb.DataMessage_EXPIRATION_TIMER_UPDATE) != 0 {
		label := "off"
		if s := dm.GetExpireTimer(); s > 0 {
			label = (time.Duration(s) * time.Second).String()
		}
		evts = append(evts, backend.MessageEvent{Message: model.Message{
			Thread: thread, Author: sender, TS: ts, ServerTS: serverTS,
			Body: "⏱ disappearing messages: " + label,
		}})
		return evts
	}
	if flags&uint32(signalpb.DataMessage_PROFILE_KEY_UPDATE) != 0 && dm.Body == nil && len(dm.Attachments) == 0 {
		return evts
	}

	msg := model.Message{
		Thread: thread, Author: sender, TS: ts, ServerTS: serverTS,
		Body:      renderBody(dm.GetBody(), dm.GetBodyRanges(), b.mentionName(ctx)),
		ExpiresIn: int64(dm.GetExpireTimer()),
	}
	if sender == b.self() {
		msg.Outgoing = true
		msg.Status = model.StatusSent
	}
	if q := dm.GetQuote(); q != nil {
		aci, _ := signalmeow.ParseStringOrBinaryUUID(q.GetAuthorAci(), q.GetAuthorAciBinary())
		msg.Quote = &model.Quote{Author: aci.String(), TS: int64(q.GetId()), Text: q.GetText()}
	}
	for _, a := range dm.GetAttachments() {
		msg.Attachments = append(msg.Attachments, attachmentFromPointer(a))
	}
	if st := dm.GetSticker(); st != nil {
		msg.Sticker = st.GetEmoji()
		if msg.Sticker == "" {
			msg.Sticker = "sticker"
		}
		if st.GetData() != nil {
			a := attachmentFromPointer(st.GetData())
			if a.Filename == "" {
				a.Filename = "sticker.webp"
			}
			msg.Attachments = append(msg.Attachments, a)
		}
	}
	if msg.Body == "" && len(msg.Attachments) == 0 && msg.Sticker == "" {
		switch {
		case dm.GetGroupV2() != nil && len(dm.GetGroupV2().GetGroupChange()) > 0:
			msg.Body = "ℹ group updated"
		case dm.GetGroupCallUpdate() != nil:
			return evts
		case dm.GetPayment() != nil:
			msg.Body = "💸 payment (view on the phone)"
		case dm.GetPollCreate() != nil:
			msg.Body = "📊 poll: " + dm.GetPollCreate().GetQuestion() + " (vote on the phone)"
		case len(dm.GetContact()) > 0:
			msg.Body = "👤 shared contact (view on the phone)"
		default:
			b.log.Debug().Int64("ts", ts).Msg("Ignoring data message with no displayable content")
			return evts
		}
	}
	return append(evts, backend.MessageEvent{Message: msg})
}

func attachmentFromPointer(a *signalpb.AttachmentPointer) model.Attachment {
	ptr, _ := proto.Marshal(a)
	return model.Attachment{
		ContentType: a.GetContentType(),
		Filename:    a.GetFileName(),
		Size:        int64(a.GetSize()),
		VoiceNote:   a.GetFlags()&uint32(signalpb.AttachmentPointer_VOICE_MESSAGE) != 0,
		State:       model.AttachmentPending,
		Pointer:     ptr,
	}
}

func (b *Backend) mentionName(ctx context.Context) func(aci string) string {
	return func(aci string) string {
		if n := b.ContactName(ctx, aci); n != "" {
			return n
		}
		return aci[:min(8, len(aci))]
	}
}

// --- sending ---

func parseThread(thread model.ThreadID) (libsignalgo.ServiceID, types.GroupIdentifier, error) {
	if sid, err := libsignalgo.ServiceIDFromString(string(thread)); err == nil {
		return sid, "", nil
	}
	gid := types.GroupIdentifier(thread)
	if _, err := gid.Bytes(); err != nil {
		return libsignalgo.ServiceID{}, "", fmt.Errorf("invalid thread ID %q", thread)
	}
	return libsignalgo.ServiceID{}, gid, nil
}

func (b *Backend) sendContent(ctx context.Context, thread model.ThreadID, content *signalpb.Content) error {
	var err error
	for range 4 {
		err = b.sendContentOnce(ctx, thread, content)
		if !b.pruneFromError(ctx, err) {
			return err
		}
	}
	return err
}

func (b *Backend) sendContentOnce(ctx context.Context, thread model.ThreadID, content *signalpb.Content) error {
	sid, gid, err := parseThread(thread)
	if err != nil {
		return err
	}
	if gid != "" {
		res, err := b.cli.SendGroupMessage(ctx, gid, content)
		if err != nil {
			return err
		}
		if len(res.SuccessfullySentTo) == 0 && len(res.FailedToSendTo) > 0 {
			return fmt.Errorf("failed to send to all %d group members", len(res.FailedToSendTo))
		}
		if len(res.FailedToSendTo) > 0 {
			b.log.Warn().Int("failed", len(res.FailedToSendTo)).Int("ok", len(res.SuccessfullySentTo)).Msg("Partial group send")
		}
		return nil
	}
	res := b.cli.SendMessage(ctx, sid, content)
	if !res.WasSuccessful {
		if res.Error != nil {
			return res.Error
		}
		return errors.New("send failed")
	}
	return nil
}

func nowTS() uint64 { return uint64(time.Now().UnixMilli()) }

func (b *Backend) Send(ctx context.Context, out model.Outgoing) error {
	ts := uint64(out.TS)
	if ts == 0 {
		ts = nowTS()
	}
	dm := &signalpb.DataMessage{Timestamp: &ts}
	if out.Body != "" {
		dm.Body = proto.String(out.Body)
	}
	if out.ExpireTimer > 0 {
		dm.ExpireTimer = proto.Uint32(out.ExpireTimer)
		if out.ExpireVersion > 0 {
			dm.ExpireTimerVersion = proto.Uint32(out.ExpireVersion)
		}
	}
	for _, path := range out.Attachments {
		ptr, err := b.upload(ctx, path)
		if err != nil {
			return fmt.Errorf("attachment %s: %w", path, err)
		}
		dm.Attachments = append(dm.Attachments, ptr)
	}
	if q := out.Quote; q != nil {
		aci, err := uuid.Parse(q.Author)
		if err != nil {
			return fmt.Errorf("quote author %q: %w", q.Author, err)
		}
		dm.Quote = &signalpb.DataMessage_Quote{
			Id:              proto.Uint64(uint64(q.TS)),
			AuthorAciBinary: aci[:],
			Text:            proto.String(q.Text),
			Type:            signalpb.DataMessage_Quote_NORMAL.Enum(),
		}
	}
	if dm.Body == nil && len(dm.Attachments) == 0 {
		return errors.New("empty message")
	}
	return b.sendContent(ctx, out.Thread, signalmeow.WrapDataMessage(dm))
}

func (b *Backend) upload(ctx context.Context, path string) (*signalpb.AttachmentPointer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("file is empty")
	}
	ptr, err := b.cli.UploadAttachment(ctx, data)
	if err != nil {
		return nil, err
	}
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if ct == "" {
		ct = http.DetectContentType(data)
	}
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	ptr.ContentType = proto.String(ct)
	ptr.FileName = proto.String(filepath.Base(path))
	uploadTS := nowTS()
	ptr.UploadTimestamp = &uploadTS
	return ptr, nil
}

func (b *Backend) SendReaction(ctx context.Context, thread model.ThreadID, target model.MessageRef, emoji string, remove bool) error {
	aci, err := uuid.Parse(target.Author)
	if err != nil {
		return fmt.Errorf("target author %q: %w", target.Author, err)
	}
	ts := nowTS()
	return b.sendContent(ctx, thread, signalmeow.WrapDataMessage(&signalpb.DataMessage{
		Timestamp: &ts,
		Reaction: &signalpb.DataMessage_Reaction{
			Emoji:                 proto.String(emoji),
			Remove:                proto.Bool(remove),
			TargetAuthorAciBinary: aci[:],
			TargetSentTimestamp:   proto.Uint64(uint64(target.TS)),
		},
	}))
}

func (b *Backend) SendDelete(ctx context.Context, thread model.ThreadID, targetTS int64) error {
	ts := nowTS()
	return b.sendContent(ctx, thread, signalmeow.WrapDataMessage(&signalpb.DataMessage{
		Timestamp: &ts,
		Delete:    &signalpb.DataMessage_Delete{TargetSentTimestamp: proto.Uint64(uint64(targetTS))},
	}))
}

func (b *Backend) SendTyping(ctx context.Context, thread model.ThreadID, typing bool) error {
	if thread == model.ThreadID(b.self()) {
		return nil
	}
	return b.sendContent(ctx, thread, signalmeow.TypingMessage(typing))
}

func (b *Backend) MarkRead(ctx context.Context, thread model.ThreadID, refs []model.MessageRef) error {
	byAuthor := map[string][]uint64{}
	for _, r := range refs {
		if r.Author == b.self() {
			continue
		}
		byAuthor[r.Author] = append(byAuthor[r.Author], uint64(r.TS))
	}
	var errs []error
	for author, tss := range byAuthor {
		aci, err := uuid.Parse(author)
		if err != nil {
			continue
		}
		// SendMessage also syncs the read state to our other devices.
		res := b.cli.SendMessage(ctx, libsignalgo.NewACIServiceID(aci), signalmeow.ReadReceptMessageForTimestamps(tss))
		if !res.WasSuccessful && res.Error != nil {
			errs = append(errs, res.Error)
		}
	}
	return errors.Join(errs...)
}

// --- metadata ---

func (b *Backend) Contacts(ctx context.Context) ([]model.Contact, error) {
	rs, err := b.dev.RecipientStore.LoadAllContacts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]model.Contact, 0, len(rs))
	for _, r := range rs {
		c := recipientToContact(r)
		if c.ID == "" {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func recipientToContact(r *types.Recipient) model.Contact {
	c := model.Contact{
		Number: r.E164, Name: r.ContactName, Nickname: r.Nickname,
		Profile: strings.TrimSpace(r.Profile.Name), Blocked: r.Blocked,
	}
	if r.ACI != uuid.Nil {
		c.ID = r.ACI.String()
	} else if r.PNI != uuid.Nil {
		c.ID = libsignalgo.NewPNIServiceID(r.PNI).String()
	}
	return c
}

func (b *Backend) Groups(ctx context.Context) ([]model.GroupInfo, error) {
	rows, err := b.db.Query(ctx, `SELECT group_identifier FROM signalmeow_groups WHERE account_id=$1`, b.dev.ACI)
	if err != nil {
		return nil, err
	}
	var ids []types.GroupIdentifier
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, types.GroupIdentifier(id))
	}
	rows.Close()
	out := make([]model.GroupInfo, 0, len(ids))
	for _, id := range ids {
		g, err := b.group(ctx, id)
		if errors.Is(err, errNotMember) {
			continue
		} else if err != nil {
			b.log.Debug().Err(err).Stringer("group", id).Msg("Group lookup failed")
			out = append(out, model.GroupInfo{ID: model.ThreadID(id)})
			continue
		}
		gi := model.GroupInfo{ID: model.ThreadID(id), Title: g.Title}
		for _, m := range g.Members {
			gi.Members = append(gi.Members, m.ACI.String())
		}
		out = append(out, gi)
	}
	return out, nil
}

var errNotMember = errors.New("not a member of this group")

func (b *Backend) group(ctx context.Context, id types.GroupIdentifier) (*signalmeow.Group, error) {
	b.mu.Lock()
	rev := b.groupRev[id]
	if t, ok := b.forbidden[id]; ok && time.Since(t) < time.Hour && rev == 0 {
		b.mu.Unlock()
		return nil, errNotMember
	}
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	g, _, err := b.cli.RetrieveGroupByID(ctx, id, rev)
	if err != nil && strings.Contains(err.Error(), "status: 403") {
		b.mu.Lock()
		b.forbidden[id] = time.Now()
		b.mu.Unlock()
		return nil, errNotMember
	}
	return g, err
}

func (b *Backend) ThreadInfo(ctx context.Context, thread model.ThreadID) (model.ThreadKind, string, error) {
	sid, gid, err := parseThread(thread)
	if err != nil {
		return "", "", err
	}
	if gid != "" {
		g, err := b.group(ctx, gid)
		if err != nil {
			return model.Group, "", err
		}
		return model.Group, g.Title, nil
	}
	if sid.Type == libsignalgo.ServiceIDTypeACI && sid.UUID == b.dev.ACI {
		return model.Direct, "Note to Self", nil
	}
	return model.Direct, b.ContactName(ctx, sid.String()), nil
}

// Contact looks up a service ID in the local store only (no network).
func (b *Backend) Contact(ctx context.Context, id string) (model.Contact, bool) {
	sid, err := libsignalgo.ServiceIDFromString(id)
	if err != nil {
		return model.Contact{}, false
	}
	var aci, pni uuid.UUID
	if sid.Type == libsignalgo.ServiceIDTypePNI {
		pni = sid.UUID
	} else {
		aci = sid.UUID
	}
	r, err := b.dev.RecipientStore.LoadAndUpdateRecipient(ctx, aci, pni, nil)
	if err != nil || r == nil {
		return model.Contact{}, false
	}
	c := recipientToContact(r)
	if c.ID == "" {
		c.ID = id
	}
	return c, true
}

// ContactName returns a display name for id, or "" if nothing better than the ID is known.
func (b *Backend) ContactName(ctx context.Context, id string) string {
	c, ok := b.Contact(ctx, id)
	if !ok {
		return ""
	}
	if name := c.DisplayName(); name != c.ID {
		return name
	}
	return ""
}

func (b *Backend) ResolveRecipient(ctx context.Context, s string) (model.ThreadID, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", errors.New("empty recipient")
	case strings.EqualFold(s, "self") || strings.EqualFold(s, "me") || s == b.dev.Number:
		return model.ThreadID(b.self()), nil
	case strings.HasPrefix(s, "+"):
		return b.resolveNumber(ctx, s)
	}
	if sid, err := libsignalgo.ServiceIDFromString(s); err == nil {
		return model.ThreadID(sid.String()), nil
	}
	if _, gid, err := parseThread(model.ThreadID(s)); err == nil && gid != "" {
		return model.ThreadID(gid), nil
	}
	return "", fmt.Errorf("can't resolve recipient %q (use +E164, UUID, group ID or 'self')", s)
}

func (b *Backend) resolveNumber(ctx context.Context, e164 string) (model.ThreadID, error) {
	r, err := b.dev.RecipientStore.LoadRecipientByE164(ctx, e164)
	if err == nil && r != nil && r.ACI != uuid.Nil {
		return model.ThreadID(r.ACI.String()), nil
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(e164, "+"), 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid number %q", e164)
	}
	resp, err := b.cli.LookupPhone(ctx, n)
	if err != nil {
		return "", fmt.Errorf("contact discovery for %s: %w", e164, err)
	}
	entry, ok := resp[n]
	if !ok || entry.ACI == uuid.Nil {
		return "", fmt.Errorf("%s is not on Signal", e164)
	}
	if _, err := b.dev.RecipientStore.LoadAndUpdateRecipient(ctx, entry.ACI, entry.PNI, func(r *types.Recipient) (bool, error) {
		if r.E164 != e164 {
			r.E164 = e164
			return true, nil
		}
		return false, nil
	}); err != nil {
		b.log.Warn().Err(err).Msg("Failed to save discovered recipient")
	}
	return model.ThreadID(entry.ACI.String()), nil
}

func (b *Backend) DownloadAttachment(ctx context.Context, pointer []byte, dest string) error {
	var ptr signalpb.AttachmentPointer
	if err := proto.Unmarshal(pointer, &ptr); err != nil {
		return fmt.Errorf("bad attachment pointer: %w", err)
	}
	tmp := dest + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = signalmeow.DownloadAttachmentWithPointer(ctx, &ptr, nil, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}
