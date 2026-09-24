// Package daemon owns the Signal connection: it persists everything the
// backend reports and serves clients over the unix socket.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	Version        string
}

type Daemon struct {
	cfg  Config
	be   backend.Backend
	hist *history.Store
	log  zerolog.Logger
	srv  *rpc.Server
	acct model.Account

	mu         sync.Mutex
	conn       model.ConnState
	connErr    string
	queueEmpty bool
	lastTS     int64
	names      map[string]string // author ID → display name cache

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
	return rpc.StatusResult{Account: d.acct, Connection: d.conn, Error: d.connErr, QueueEmpty: d.queueEmpty, Clients: n, Version: d.cfg.Version}
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
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.attachWake:
		case <-retry.C:
		}
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

// attachmentName builds a unique, readable file name: <ts>-<idx>-<name>.
func attachmentName(p history.PendingAttachment) string {
	name := sanitize(p.Filename)
	if name == "" {
		name = "attachment" + extFor(p.ContentType)
	}
	return fmt.Sprintf("%d-%d-%s", p.TS, p.Index, name)
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
	t := time.NewTicker(15 * time.Second)
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
				p.Message.Attachments[i].Pointer = []byte(p.AttachmentData[i])
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
