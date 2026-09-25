// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"signal-headless/internal/backend"
	"signal-headless/internal/db"
	"signal-headless/internal/fakebackend"
	"signal-headless/internal/history"
	"signal-headless/internal/model"
	"signal-headless/internal/rpc"
)

type env struct {
	t    *testing.T
	fake *fakebackend.Fake
	d    *Daemon
	dir  string
	sock string
	done chan struct{}
}

func start(t *testing.T, opts ...func(*Config)) *env {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(context.Background(), filepath.Join(dir, "t.db"), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	fake := fakebackend.New()
	att := filepath.Join(dir, "attachments")
	os.MkdirAll(att, 0o700)
	sock := filepath.Join(dir, "d.sock")
	cfg := Config{Socket: sock, AttachmentsDir: att, Version: "test"}
	for _, o := range opts {
		o(&cfg)
	}
	d := New(cfg, fake, history.New(database.Database), zerolog.Nop())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		database.Close()
	})
	<-fake.Ready()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return &env{t: t, fake: fake, d: d, dir: dir, sock: sock, done: done}
}

func (e *env) dial(native bool) *rpc.Client {
	e.t.Helper()
	c, err := rpc.Dial(e.sock)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { c.Close() })
	if native {
		if err := c.Call(context.Background(), rpc.MSubscribe, nil, nil); err != nil {
			e.t.Fatal(err)
		}
	}
	return c
}

func (e *env) call(c *rpc.Client, method string, params, out any) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Call(ctx, method, params, out); err != nil {
		e.t.Fatalf("%s: %v", method, err)
	}
}

// next waits for a notification with the given method.
func next(t *testing.T, c *rpc.Client, method string) json.RawMessage {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case n, ok := <-c.Notifications():
			if !ok {
				t.Fatalf("connection closed waiting for %s", method)
			}
			if n.Method == method {
				return n.Params
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", method)
		}
	}
}

func TestCompatNoteToSelf(t *testing.T) {
	e := start(t)
	c := e.dial(false)
	ts := time.Now().UnixMilli()
	if err := e.fake.Inject(backend.MessageEvent{Message: model.Message{
		Thread: fakebackend.SelfACI, Author: fakebackend.SelfACI, TS: ts, Body: "hello agent", Outgoing: true, Status: model.StatusSent,
	}}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Envelope struct {
			Timestamp   int64 `json:"timestamp"`
			SyncMessage struct {
				SentMessage struct {
					DestinationNumber string `json:"destinationNumber"`
					Message           string `json:"message"`
					Timestamp         int64  `json:"timestamp"`
				} `json:"sentMessage"`
			} `json:"syncMessage"`
		} `json:"envelope"`
		Account string `json:"account"`
	}
	json.Unmarshal(next(t, c, rpc.EvReceive), &got)
	sm := got.Envelope.SyncMessage.SentMessage
	if sm.DestinationNumber != "+15550000000" || sm.Message != "hello agent" || sm.Timestamp != ts || got.Account != "+15550000000" {
		t.Fatalf("compat envelope = %+v", got)
	}

	// signal_agent's reply: send to its own number, then react to the note.
	var sr rpc.SendResult
	e.call(c, rpc.MSend, map[string]any{"recipient": []string{"+15550000000"}, "message": "4"}, &sr)
	if sr.Timestamp == 0 || sr.Message == nil || sr.Message.Status != model.StatusSent {
		t.Fatalf("send result = %+v", sr)
	}
	e.call(c, rpc.MSendReaction, map[string]any{
		"recipient": []string{"+15550000000"}, "emoji": "👀", "targetAuthor": "+15550000000", "targetTimestamp": ts,
	}, nil)
	sent := e.fake.Sent()
	if len(sent) != 2 || sent[0].Out.Thread != fakebackend.SelfACI || sent[0].Out.Body != "4" ||
		sent[1].Kind != "reaction" || sent[1].Ref.TS != ts || sent[1].Ref.Author != fakebackend.SelfACI {
		t.Fatalf("sent = %+v", sent)
	}
}

func TestCompatAttachmentWaitsForDownload(t *testing.T) {
	e := start(t)
	c := e.dial(false)
	e.fake.Inject(backend.MessageEvent{Message: model.Message{
		Thread: fakebackend.SelfACI, Author: fakebackend.SelfACI, TS: 42, Outgoing: true, Body: "see file",
		Attachments: []model.Attachment{{ContentType: "text/plain", Filename: "../evil name.txt", Pointer: []byte("content")}},
	}})
	var got struct {
		Envelope struct {
			SyncMessage struct {
				SentMessage struct {
					Attachments []struct {
						ID string `json:"id"`
					} `json:"attachments"`
				} `json:"sentMessage"`
			} `json:"syncMessage"`
		} `json:"envelope"`
	}
	json.Unmarshal(next(t, c, rpc.EvReceive), &got)
	atts := got.Envelope.SyncMessage.SentMessage.Attachments
	if len(atts) != 1 || atts[0].ID != "42-1-0-evil name.txt" {
		t.Fatalf("attachments = %+v", atts)
	}
	b, err := os.ReadFile(filepath.Join(e.dir, "attachments", atts[0].ID))
	if err != nil || string(b) != "content" {
		t.Fatalf("attachment file: %q %v", b, err)
	}
}

func TestNativeFlow(t *testing.T) {
	e := start(t)
	c := e.dial(true)
	ctx := context.Background()

	e.fake.Inject(backend.MessageEvent{Message: model.Message{Thread: fakebackend.AliceACI, Author: fakebackend.AliceACI, TS: 1000, Body: "hi"}})
	var m model.Message
	json.Unmarshal(next(t, c, rpc.EvMessage), &m)
	if m.Body != "hi" || m.AuthorName != "Alice Liddell" {
		t.Fatalf("message = %+v", m)
	}
	var th model.Thread
	json.Unmarshal(next(t, c, rpc.EvThread), &th)
	if th.Title != "Alice Liddell" || th.Unread != 1 {
		t.Fatalf("thread = %+v", th)
	}

	// Disappearing timer set by the peer must ride on our replies.
	e.fake.Inject(backend.TimerEvent{Thread: fakebackend.AliceACI, Seconds: 3600, Version: 1})

	var sr rpc.SendResult
	e.call(c, rpc.MSend, rpc.SendParams{Thread: fakebackend.AliceACI, Body: "hello",
		Quote: &model.Quote{Author: fakebackend.AliceACI, TS: 1000}}, &sr)
	sent := e.fake.Sent()
	if len(sent) != 1 || sent[0].Out.ExpireTimer != 3600 || sent[0].Out.Quote.Text != "hi" {
		t.Fatalf("sent = %+v", sent)
	}
	if sr.Message.ExpiresIn != 3600 || sr.Message.Status != model.StatusSent {
		t.Fatalf("send result = %+v", sr.Message)
	}

	e.fake.Inject(backend.ReceiptEvent{From: fakebackend.AliceACI, Kind: backend.ReceiptRead, Timestamps: []int64{sr.Timestamp}})
	for {
		var u model.Message
		json.Unmarshal(next(t, c, rpc.EvMessageUpdate), &u)
		if u.ID == sr.Message.ID && u.Status == model.StatusRead {
			break
		}
	}

	e.call(c, rpc.MMarkRead, rpc.ThreadParams{Thread: fakebackend.AliceACI}, nil)
	time.Sleep(50 * time.Millisecond)
	var got []model.Thread
	e.call(c, rpc.MListThreads, nil, &got)
	if len(got) != 1 || got[0].Unread != 0 || got[0].ExpireTimer != 3600 {
		t.Fatalf("threads = %+v", got)
	}
	var reads []fakebackend.Sent
	for _, s := range e.fake.Sent() {
		if s.Kind == "read" {
			reads = append(reads, s)
		}
	}
	if len(reads) != 1 || len(reads[0].Refs) != 1 || reads[0].Refs[0].TS != 1000 {
		t.Fatalf("read receipts = %+v", reads)
	}

	e.fake.Inject(backend.DeleteEvent{Thread: fakebackend.AliceACI, Author: fakebackend.AliceACI, TargetTS: 1000})
	var msgs []model.Message
	e.call(c, rpc.MGetMessages, rpc.GetMessagesParams{Thread: fakebackend.AliceACI}, &msgs)
	if len(msgs) != 2 || !msgs[0].Deleted || msgs[1].Body != "hello" {
		t.Fatalf("messages = %+v", msgs)
	}

	var res rpc.ResolveResult
	e.call(c, rpc.MResolve, rpc.ResolveParams{Recipient: "bob"}, &res)
	if res.Thread != fakebackend.BobACI || res.Title != "Bob" {
		t.Fatalf("resolve = %+v", res)
	}
	e.call(c, rpc.MResolve, rpc.ResolveParams{Recipient: "tea"}, &res)
	if res.Thread != fakebackend.GroupID {
		t.Fatalf("resolve group by prefix = %+v", res)
	}
	e.call(c, rpc.MResolve, rpc.ResolveParams{Recipient: "echo"}, &res)
	if res.Thread != fakebackend.EchoACI {
		t.Fatalf("resolve by prefix = %+v", res)
	}
	e.call(c, rpc.MResolve, rpc.ResolveParams{Recipient: "li"}, &res)
	if res.Thread != fakebackend.AliceACI {
		t.Fatalf("resolve by substring = %+v", res)
	}
	if err := c.Call(ctx, rpc.MResolve, rpc.ResolveParams{Recipient: "o"}, nil); err == nil {
		t.Fatal("'o' is ambiguous and should fail")
	}
	_ = ctx
}

func TestSendFailureIsRecorded(t *testing.T) {
	e := start(t)
	c := e.dial(true)
	e.fake.SendErr = os.ErrDeadlineExceeded
	err := c.Call(context.Background(), rpc.MSend, rpc.SendParams{To: "self", Body: "x"}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var msgs []model.Message
	e.call(c, rpc.MGetMessages, rpc.GetMessagesParams{Thread: fakebackend.SelfACI}, &msgs)
	if len(msgs) != 1 || msgs[0].Status != model.StatusFailed {
		t.Fatalf("messages = %+v", msgs)
	}
	if err := c.Call(context.Background(), rpc.MSend, rpc.SendParams{To: "self", Attachments: []string{"/nonexistent"}}, nil); err == nil {
		t.Fatal("missing attachment should fail")
	}
}

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{"../../etc/passwd": "passwd", "a\x00b": "a_b", "": "", "..": ""} {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnlink(t *testing.T) {
	e := start(t)
	c := e.dial(true)
	if err := c.Call(context.Background(), rpc.MUnlink, rpc.UnlinkParams{Number: "+19999999999"}, nil); err == nil {
		t.Fatal("wrong confirmation number must be rejected")
	}
	for _, s := range e.fake.Sent() {
		if s.Kind == "unlink" {
			t.Fatal("unlinked despite wrong confirmation")
		}
	}
	e.call(c, rpc.MUnlink, rpc.UnlinkParams{Number: "+15550000000"}, nil)
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop after unlink")
	}
	var n int
	for _, s := range e.fake.Sent() {
		if s.Kind == "unlink" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("unlink calls = %d", n)
	}
}

func TestDeletedPlaceholdersExpire(t *testing.T) {
	e := start(t, func(c *Config) {
		c.DeletedTTL = 300 * time.Millisecond
		c.SweepInterval = 50 * time.Millisecond
	})
	c := e.dial(true)
	alice := model.ThreadID(fakebackend.AliceACI)
	e.fake.Inject(backend.MessageEvent{Message: model.Message{Thread: alice, Author: fakebackend.AliceACI, TS: 1000, Body: "oops"}})
	e.fake.Inject(backend.MessageEvent{Message: model.Message{Thread: alice, Author: fakebackend.AliceACI, TS: 2000, Body: "stays"}})
	var gone model.Message
	json.Unmarshal(next(t, c, rpc.EvMessage), &gone)
	e.fake.Inject(backend.DeleteEvent{Thread: alice, Author: fakebackend.AliceACI, TargetTS: 1000})

	var msgs []model.Message
	e.call(c, rpc.MGetMessages, rpc.GetMessagesParams{Thread: alice}, &msgs)
	if len(msgs) != 2 || !msgs[0].Deleted {
		t.Fatalf("placeholder should show first: %+v", msgs)
	}
	var r rpc.MessageRemoved
	json.Unmarshal(next(t, c, rpc.EvMessageRemoved), &r)
	if r.ID != gone.ID || r.Thread != alice {
		t.Fatalf("removed = %+v, want id %d", r, gone.ID)
	}
	e.call(c, rpc.MGetMessages, rpc.GetMessagesParams{Thread: alice}, &msgs)
	if len(msgs) != 1 || msgs[0].Body != "stays" {
		t.Fatalf("after purge: %+v", msgs)
	}
}

func TestHistoryImportIsQuiet(t *testing.T) {
	e := start(t)
	c := e.dial(true)
	compat := e.dial(false)
	alice := model.ThreadID(fakebackend.AliceACI)
	e.fake.Inject(backend.HistoryStatusEvent{Status: model.HistoryStatus{State: "importing"}})
	err := e.fake.Inject(backend.HistoryEvent{Thread: alice, Kind: model.Direct, Archived: true, ExpireTimer: 60, ExpireVersion: 1, Messages: []model.Message{
		{Thread: alice, Author: fakebackend.AliceACI, TS: 100, Body: "old news", Read: true,
			Reactions: []model.Reaction{{Reactor: fakebackend.SelfACI, Emoji: "👍", TS: 101}}},
		{Thread: alice, Author: fakebackend.SelfACI, TS: 200, Body: "old reply", Outgoing: true, Status: model.StatusRead},
		{Thread: alice, Author: fakebackend.AliceACI, TS: 300, Body: "unread one",
			Attachments: []model.Attachment{{Filename: "a.txt", State: model.AttachmentPending, Pointer: []byte("data")}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var h rpc.HistoryImported
	json.Unmarshal(next(t, c, rpc.EvHistory), &h)
	if h.Thread != alice || h.Messages != 3 {
		t.Fatalf("history event = %+v", h)
	}
	var msgs []model.Message
	e.call(c, rpc.MGetMessages, rpc.GetMessagesParams{Thread: alice}, &msgs)
	if len(msgs) != 3 || len(msgs[0].Reactions) != 1 || msgs[1].Status != model.StatusRead {
		t.Fatalf("messages = %+v", msgs)
	}
	var th model.Thread
	e.call(c, rpc.MGetThread, rpc.ThreadParams{Thread: alice}, &th)
	if !th.Archived || th.ExpireTimer != 60 || th.Unread != 1 {
		t.Fatalf("thread = %+v", th)
	}
	var st rpc.StatusResult
	e.call(c, rpc.MStatus, nil, &st)
	if st.History == nil || st.History.State != "importing" {
		t.Fatalf("status history = %+v", st.History)
	}
	// Attachments of imported messages download like any other.
	for {
		var u model.Message
		json.Unmarshal(next(t, c, rpc.EvMessageUpdate), &u)
		if u.TS == 300 && len(u.Attachments) == 1 && u.Attachments[0].State == model.AttachmentDone {
			break
		}
	}
	// Old messages are not announced: no native message events, no
	// signal-cli receive envelopes.
	drain := time.After(300 * time.Millisecond)
	for {
		select {
		case n := <-c.Notifications():
			if n.Method == rpc.EvMessage {
				t.Fatalf("history announced as a new message: %s", n.Params)
			}
		case n := <-compat.Notifications():
			if n.Method == rpc.EvReceive {
				t.Fatalf("history sent to signal-cli clients: %s", n.Params)
			}
		case <-drain:
			return
		}
	}
}

func TestOutgoingLinkPreview(t *testing.T) {
	e := start(t, func(c *Config) { c.LinkPreview = fakebackend.LinkPreview(c.AttachmentsDir) })
	c := e.dial(true)
	url := "https://tea.example/brew"
	var p model.OutgoingPreview
	e.call(c, rpc.MLinkPreview, rpc.LinkPreviewParams{URL: url}, &p)
	if p.Title != "Preview of tea.example" || p.Image == "" {
		t.Fatalf("preview = %+v", p)
	}
	if err := c.Call(context.Background(), rpc.MLinkPreview, rpc.LinkPreviewParams{URL: "https://tea.example/nopreview"}, nil); err == nil {
		t.Fatal("expected no preview")
	}
	var st rpc.StatusResult
	e.call(c, rpc.MStatus, nil, &st)
	if !st.LinkPreviews {
		t.Fatal("status should carry the account's link-preview setting")
	}
	var sr rpc.SendResult
	e.call(c, rpc.MSend, rpc.SendParams{Thread: fakebackend.BobACI, Body: "read this " + url,
		Previews: []model.OutgoingPreview{p, {URL: "https://not-in-the-text.example/", Title: "dropped"}}}, &sr)
	m := sr.Message
	if len(m.Previews) != 1 || m.Previews[0].URL != url || m.Previews[0].Image != 0 {
		t.Fatalf("stored previews = %+v", m.Previews)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].Kind != model.AttachmentPreview || m.Attachments[0].Path != p.Image {
		t.Fatalf("stored attachments = %+v", m.Attachments)
	}
	var sent model.Outgoing
	for _, s := range e.fake.Sent() {
		if s.Kind == "message" {
			sent = s.Out
		}
	}
	if len(sent.Previews) != 1 || sent.Previews[0].Image != p.Image {
		t.Fatalf("backend got previews %+v", sent.Previews)
	}
}

func TestPurgeRemovesFilesAndNotifies(t *testing.T) {
	e := start(t)
	c := e.dial(true)
	bob := model.ThreadID(fakebackend.BobACI)
	e.fake.Inject(backend.MessageEvent{Message: model.Message{Thread: bob, Author: fakebackend.BobACI, TS: 1000, Body: "ancient",
		Attachments: []model.Attachment{{Filename: "old.txt", Pointer: []byte("0123456789")}}}})
	e.fake.Inject(backend.MessageEvent{Message: model.Message{Thread: bob, Author: fakebackend.BobACI, TS: time.Now().UnixMilli(), Body: "fresh"}})
	var path string
	for path == "" {
		var u model.Message
		json.Unmarshal(next(t, c, rpc.EvMessageUpdate), &u)
		if u.TS == 1000 && len(u.Attachments) == 1 && u.Attachments[0].State == model.AttachmentDone {
			path = u.Attachments[0].Path
		}
	}
	var st rpc.StatsResult
	e.call(c, rpc.MStats, nil, &st)
	if st.Messages != 2 || st.AttachmentFiles != 1 || st.AttachmentBytes != 10 {
		t.Fatalf("stats = %+v", st)
	}
	cutoff := time.Now().Add(-time.Hour).UnixMilli()
	var dry rpc.PurgeResult
	e.call(c, rpc.MPurge, rpc.PurgeParams{Before: cutoff, DryRun: true}, &dry)
	if dry.Messages != 1 || dry.Bytes != 10 {
		t.Fatalf("dry run = %+v", dry)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("dry run removed the file")
	}
	var r rpc.PurgeResult
	e.call(c, rpc.MPurge, rpc.PurgeParams{Before: cutoff}, &r)
	if r.Messages != 1 || r.Bytes != 10 {
		t.Fatalf("purge = %+v", r)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("attachment file kept")
	}
	var h rpc.HistoryImported
	json.Unmarshal(next(t, c, rpc.EvHistory), &h)
	if h.Thread != bob {
		t.Fatalf("reload event = %+v", h)
	}
	var msgs []model.Message
	e.call(c, rpc.MGetMessages, rpc.GetMessagesParams{Thread: bob}, &msgs)
	if len(msgs) != 1 || msgs[0].Body != "fresh" {
		t.Fatalf("left = %+v", msgs)
	}
	if err := c.Call(context.Background(), rpc.MPurge, rpc.PurgeParams{}, nil); err == nil {
		t.Fatal("purge without a cutoff must fail")
	}
}

func TestPurgeAllDevices(t *testing.T) {
	e := start(t)
	c := e.dial(true)
	bob := model.ThreadID(fakebackend.BobACI)
	old := time.Now().Add(-48 * time.Hour).UnixMilli()
	e.fake.Inject(backend.MessageEvent{Message: model.Message{Thread: bob, Author: fakebackend.BobACI, TS: old, Body: "old one"}})
	e.fake.Inject(backend.MessageEvent{Message: model.Message{Thread: bob, Author: fakebackend.SelfACI, TS: old + 1, Body: "old reply", Outgoing: true}})
	e.fake.Inject(backend.MessageEvent{Message: model.Message{Thread: bob, Author: fakebackend.BobACI, TS: time.Now().UnixMilli(), Body: "new"}})
	cutoff := time.Now().Add(-time.Hour).UnixMilli()

	// If the other devices can't be told, nothing is deleted here either.
	e.fake.DeleteErr = errors.New("offline")
	if err := c.Call(context.Background(), rpc.MPurge, rpc.PurgeParams{Before: cutoff, AllDevices: true}, nil); err == nil {
		t.Fatal("expected failure")
	}
	var msgs []model.Message
	e.call(c, rpc.MGetMessages, rpc.GetMessagesParams{Thread: bob}, &msgs)
	if len(msgs) != 3 {
		t.Fatalf("deleted despite failed sync: %d left", len(msgs))
	}
	e.fake.DeleteErr = nil

	var r rpc.PurgeResult
	e.call(c, rpc.MPurge, rpc.PurgeParams{Before: cutoff, AllDevices: true}, &r)
	if r.Messages != 2 {
		t.Fatalf("purge = %+v", r)
	}
	var sync *fakebackend.Sent
	for _, s := range e.fake.Sent() {
		if s.Kind == "deleteForMe" {
			s := s
			sync = &s
		}
	}
	if sync == nil || len(sync.Deletes) != 2 || sync.Deletes[0].Thread != bob {
		t.Fatalf("delete-for-me sync = %+v", sync)
	}
	e.call(c, rpc.MGetMessages, rpc.GetMessagesParams{Thread: bob}, &msgs)
	if len(msgs) != 1 || msgs[0].Body != "new" {
		t.Fatalf("left = %+v", msgs)
	}
}

func TestDeleteForMeFromPhone(t *testing.T) {
	e := start(t)
	c := e.dial(true)
	alice, bob := model.ThreadID(fakebackend.AliceACI), model.ThreadID(fakebackend.BobACI)
	for i, th := range []model.ThreadID{alice, alice, alice, bob} {
		e.fake.Inject(backend.MessageEvent{Message: model.Message{Thread: th, Author: string(th), TS: int64(100 + i), Body: fmt.Sprint("m", i)}})
	}
	var first model.Message
	json.Unmarshal(next(t, c, rpc.EvMessage), &first)
	err := e.fake.Inject(backend.DeleteForMeEvent{
		Messages:      []backend.MessageDelete{{Thread: alice, Ref: model.MessageRef{Author: string(alice), TS: 100}}},
		Conversations: []backend.ConversationDelete{{Thread: alice, Through: 101}, {Thread: bob, Full: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var r rpc.MessageRemoved
	json.Unmarshal(next(t, c, rpc.EvMessageRemoved), &r)
	if r.ID != first.ID {
		t.Fatalf("removed %+v, want id %d", r, first.ID)
	}
	var msgs []model.Message
	e.call(c, rpc.MGetMessages, rpc.GetMessagesParams{Thread: alice}, &msgs)
	if len(msgs) != 1 || msgs[0].TS != 102 {
		t.Fatalf("alice left = %+v", msgs)
	}
	var threads []model.Thread
	e.call(c, rpc.MListThreads, nil, &threads)
	for _, th := range threads {
		if th.ID == bob {
			t.Fatal("fully deleted conversation still listed")
		}
	}
}

func TestProtocolAndShutdown(t *testing.T) {
	e := start(t)
	c := e.dial(true)
	var st rpc.StatusResult
	e.call(c, rpc.MStatus, nil, &st)
	if st.Protocol != rpc.ProtocolVersion || st.Protocol < 1 {
		t.Fatalf("protocol = %d", st.Protocol)
	}
	e.call(c, rpc.MShutdown, nil, nil)
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
}
