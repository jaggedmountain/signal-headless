package daemon

import (
	"context"
	"encoding/json"
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

func start(t *testing.T) *env {
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
	d := New(Config{Socket: sock, AttachmentsDir: att, Version: "test"}, fake, history.New(database.Database), zerolog.Nop())
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
