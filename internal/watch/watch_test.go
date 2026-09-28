// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package watch

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"signal-headless/internal/backend"
	"signal-headless/internal/daemon"
	"signal-headless/internal/db"
	"signal-headless/internal/fakebackend"
	"signal-headless/internal/history"
	"signal-headless/internal/model"
	"signal-headless/internal/rpc"
)

// harness runs a daemon on the fake backend that can be stopped and started
// again on the same database and socket, like a restarted real daemon.
type harness struct {
	t    *testing.T
	dir  string
	sock string
	fake *fakebackend.Fake
	stop func()
}

func newHarness(t *testing.T) *harness {
	sockDir, err := os.MkdirTemp("", "shw") // short: macOS limits socket paths
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	h := &harness{t: t, dir: t.TempDir(), sock: filepath.Join(sockDir, "d.sock")}
	h.start()
	t.Cleanup(func() {
		if h.stop != nil {
			h.stop()
		}
	})
	return h
}

func (h *harness) start() {
	h.t.Helper()
	database, err := db.Open(context.Background(), filepath.Join(h.dir, "t.db"), zerolog.Nop())
	if err != nil {
		h.t.Fatal(err)
	}
	att := filepath.Join(h.dir, "attachments")
	os.MkdirAll(att, 0o700)
	h.fake = fakebackend.New()
	d := daemon.New(daemon.Config{Socket: h.sock, AttachmentsDir: att, Version: "test"}, h.fake, history.New(database.Database), zerolog.Nop())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	h.stop = func() { cancel(); <-done; database.Close(); h.stop = nil }
	<-h.fake.Ready()
	for i := 0; i < 200; i++ {
		if c, err := rpc.Dial(h.sock); err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatal("daemon didn't come up")
}

func (h *harness) dial(ctx context.Context) (*rpc.Client, error) { return rpc.Dial(h.sock) }

// phone injects a Note to Self message as if typed on the phone.
func (h *harness) phone(ts int64, body string, atts ...model.Attachment) {
	h.t.Helper()
	err := h.fake.Inject(backend.MessageEvent{Message: model.Message{
		Thread: fakebackend.SelfACI, Author: fakebackend.SelfACI, TS: ts, Body: body,
		Outgoing: true, Status: model.StatusSent, Attachments: atts,
	}})
	if err != nil {
		h.t.Fatal(err)
	}
}

// send sends to Note to Self through the daemon, as an agent's reply would.
func (h *harness) send(body string) {
	h.t.Helper()
	c, err := rpc.Dial(h.sock)
	if err != nil {
		h.t.Fatal(err)
	}
	defer c.Close()
	if err := c.Call(context.Background(), rpc.MSend, rpc.SendParams{To: "self", Body: body}, nil); err != nil {
		h.t.Fatal(err)
	}
}

// collector runs Run in the background and gathers what it emits.
type collector struct {
	mu   sync.Mutex
	got  []model.Message
	done chan error
}

func (h *harness) watch(ctx context.Context, o Options) *collector {
	c := &collector{done: make(chan error, 1)}
	o.Thread = fakebackend.SelfACI
	o.Dial = h.dial
	o.Emit = func(m model.Message) error {
		c.mu.Lock()
		c.got = append(c.got, m)
		c.mu.Unlock()
		return nil
	}
	go func() { c.done <- Run(ctx, o) }()
	return c
}

func (c *collector) bodies() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, m := range c.got {
		out = append(out, m.Body)
	}
	return out
}

func (c *collector) waitFor(t *testing.T, n int) []string {
	t.Helper()
	for i := 0; i < 500; i++ {
		if b := c.bodies(); len(b) >= n {
			return b
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("got %q, want %d messages", c.bodies(), n)
	return nil
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestNoteToSelfInstructions(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UnixMilli()
	h.phone(now-5000, "old instruction, before the watch")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := h.watch(ctx, Options{})
	time.Sleep(200 * time.Millisecond) // subscribed
	h.phone(now, "run the tests")
	h.send("tests pass") // our own reply: not an instruction
	h.phone(now+1000, "now deploy")
	if got := c.waitFor(t, 2); !equal(got, []string{"run the tests", "now deploy"}) {
		t.Fatalf("emitted %q", got)
	}
	time.Sleep(200 * time.Millisecond)
	if got := c.bodies(); len(got) != 2 {
		t.Fatalf("our own reply was emitted: %q", got)
	}
}

func TestOnce(t *testing.T) {
	h := newHarness(t)
	c := h.watch(context.Background(), Options{Once: true})
	time.Sleep(200 * time.Millisecond)
	now := time.Now().UnixMilli()
	h.phone(now, "first")
	h.phone(now+1, "second")
	select {
	case err := <-c.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Once didn't return")
	}
	if got := c.bodies(); !equal(got, []string{"first"}) {
		t.Fatalf("emitted %q", got)
	}
}

func TestSinceResumes(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UnixMilli()
	h.phone(now-3000, "a")
	h.phone(now-2000, "b")
	h.phone(now-1000, "c")
	cl, _ := h.dial(context.Background())
	var msgs []model.Message
	cl.Call(context.Background(), rpc.MGetMessages, rpc.GetMessagesParams{Thread: fakebackend.SelfACI}, &msgs)
	cl.Close()
	var aID int64
	for _, m := range msgs {
		if m.Body == "a" {
			aID = m.ID
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := h.watch(ctx, Options{Since: aID})
	if got := c.waitFor(t, 2); !equal(got, []string{"b", "c"}) {
		t.Fatalf("emitted %q", got)
	}
}

func TestAttachmentsSettled(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := h.watch(ctx, Options{})
	time.Sleep(200 * time.Millisecond)
	h.phone(time.Now().UnixMilli(), "see attached", model.Attachment{ContentType: "text/plain", Filename: "notes.txt", Pointer: []byte("hello")})
	c.waitFor(t, 1)
	c.mu.Lock()
	m := c.got[0]
	c.mu.Unlock()
	if len(m.Attachments) != 1 || m.Attachments[0].State != model.AttachmentDone || m.Attachments[0].Path == "" {
		t.Fatalf("attachment = %+v", m.Attachments)
	}
	if b, err := os.ReadFile(m.Attachments[0].Path); err != nil || string(b) != "hello" {
		t.Fatalf("file = %q, %v", b, err)
	}
}

func TestSurvivesDaemonRestart(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := h.watch(ctx, Options{})
	time.Sleep(200 * time.Millisecond)
	now := time.Now().UnixMilli()
	h.phone(now, "before the restart")
	c.waitFor(t, 1)
	h.stop()
	h.start()
	// Arrives before the watcher has reconnected (its backoff is a second):
	// the catch-up has to find it.
	h.phone(now+1000, "while it was reconnecting")
	h.phone(now+2000, "after")
	if got := c.waitFor(t, 3); !equal(got, []string{"before the restart", "while it was reconnecting", "after"}) {
		t.Fatalf("emitted %q", got)
	}
}
