package history

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"signal-headless/internal/db"
	"signal-headless/internal/model"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return New(d.Database)
}

func TestMessageLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	const self, bob = "self-aci", "bob-aci"
	th := model.ThreadID(bob)
	if err := s.EnsureThread(ctx, th, model.Direct, "Bob"); err != nil {
		t.Fatal(err)
	}
	// Empty title must not clobber an existing one.
	if err := s.EnsureThread(ctx, th, model.Direct, ""); err != nil {
		t.Fatal(err)
	}

	in := &model.Message{Thread: th, Author: bob, TS: 1000, Body: "hi",
		Attachments: []model.Attachment{{ContentType: "image/png", Filename: "a.png", Pointer: []byte{1}}}}
	ok, err := s.InsertMessage(ctx, in)
	if err != nil || !ok {
		t.Fatalf("insert: %v %v", ok, err)
	}
	dup := &model.Message{Thread: th, Author: bob, TS: 1000, Body: "hi"}
	if ok, err := s.InsertMessage(ctx, dup); err != nil || ok || dup.ID != in.ID {
		t.Fatalf("duplicate insert: ok=%v err=%v id=%d want %d", ok, err, dup.ID, in.ID)
	}
	out := &model.Message{Thread: th, Author: self, TS: 2000, Body: "hello", Outgoing: true, Status: model.StatusSent,
		Quote: &model.Quote{Author: bob, TS: 1000, Text: "hi"}}
	if _, err := s.InsertMessage(ctx, out); err != nil {
		t.Fatal(err)
	}

	thr, err := s.Thread(ctx, th)
	if err != nil {
		t.Fatal(err)
	}
	if thr.Title != "Bob" || thr.Unread != 1 || thr.LastTS != 2000 || thr.LastPreview != "hello" {
		t.Fatalf("thread = %+v", thr)
	}

	msgs, err := s.Messages(ctx, th, 0, 10)
	if err != nil || len(msgs) != 2 || msgs[0].TS != 1000 || msgs[1].Quote == nil {
		t.Fatalf("messages = %+v err=%v", msgs, err)
	}
	if len(msgs[0].Attachments) != 1 || msgs[0].Attachments[0].Pointer != nil {
		t.Fatalf("attachments should load without pointers: %+v", msgs[0].Attachments)
	}

	pend, err := s.PendingAttachments(ctx)
	if err != nil || len(pend) != 1 || string(pend[0].Pointer) != "\x01" {
		t.Fatalf("pending = %+v err=%v", pend, err)
	}
	if err := s.UpdateAttachment(ctx, pend[0].MessageID, 0, model.AttachmentDone, "/x/a.png", ""); err != nil {
		t.Fatal(err)
	}
	if pend, _ := s.PendingAttachments(ctx); len(pend) != 0 {
		t.Fatalf("still pending: %+v", pend)
	}

	// Receipts only move status forward.
	ch, err := s.ApplyReceipt(ctx, self, model.StatusRead, []int64{2000})
	if err != nil || len(ch) != 1 || ch[0].Status != model.StatusRead {
		t.Fatalf("read receipt: %+v %v", ch, err)
	}
	if ch, _ := s.ApplyReceipt(ctx, self, model.StatusDelivered, []int64{2000}); len(ch) != 0 {
		t.Fatalf("delivered after read should be a no-op: %+v", ch)
	}

	m, err := s.ApplyReaction(ctx, th, model.MessageRef{Author: bob, TS: 1000}, self, "👍", false, 3000)
	if err != nil || len(m.Reactions) != 1 {
		t.Fatalf("reaction: %+v %v", m, err)
	}
	m, _ = s.ApplyReaction(ctx, th, model.MessageRef{Author: bob, TS: 1000}, self, "❤️", false, 3001)
	if len(m.Reactions) != 1 || m.Reactions[0].Emoji != "❤️" {
		t.Fatalf("reaction replace: %+v", m.Reactions)
	}

	m, err = s.ApplyEdit(ctx, th, bob, 1000, 4000, "hi there")
	if err != nil || m.Body != "hi there" || m.EditedAt != 4000 {
		t.Fatalf("edit: %+v %v", m, err)
	}
	if _, err := s.ApplyEdit(ctx, th, bob, 999, 4000, "x"); err != ErrNotFound {
		t.Fatalf("edit unknown: %v", err)
	}

	refs, err := s.MarkThreadRead(ctx, th)
	if err != nil || len(refs) != 1 || refs[0].TS != 1000 {
		t.Fatalf("mark read: %+v %v", refs, err)
	}
	if thr, _ := s.Thread(ctx, th); thr.Unread != 0 {
		t.Fatalf("unread after mark = %d", thr.Unread)
	}

	found, err := s.Search(ctx, "", "THERE", 10)
	if err != nil || len(found) != 1 {
		t.Fatalf("search: %+v %v", found, err)
	}

	m, err = s.ApplyDelete(ctx, th, bob, 1000)
	if err != nil || !m.Deleted || m.Body != "" || len(m.Attachments) != 0 || len(m.Reactions) != 0 {
		t.Fatalf("delete: %+v %v", m, err)
	}
}

func TestReadSync(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	th := model.ThreadID("carol")
	s.EnsureThread(ctx, th, model.Direct, "")
	for _, ts := range []int64{1, 2, 3} {
		s.InsertMessage(ctx, &model.Message{Thread: th, Author: "carol", TS: ts, Body: "m"})
	}
	threads, err := s.ApplyReadSync(ctx, []model.MessageRef{{Author: "carol", TS: 2}, {Author: "nobody", TS: 9}})
	if err != nil || len(threads) != 1 || threads[0] != th {
		t.Fatalf("read sync: %v %v", threads, err)
	}
	if thr, _ := s.Thread(ctx, th); thr.Unread != 1 {
		t.Fatalf("unread = %d, want 1", thr.Unread)
	}
}

func TestDisappearing(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	th := model.ThreadID("dave")
	s.EnsureThread(ctx, th, model.Direct, "")
	if err := s.SetTimer(ctx, th, 60, 2); err != nil {
		t.Fatal(err)
	}
	s.SetTimer(ctx, th, 5, 1) // stale version: ignored
	if sec, ver, _ := s.Timer(ctx, th); sec != 60 || ver != 2 {
		t.Fatalf("timer = %d v%d", sec, ver)
	}
	s.InsertMessage(ctx, &model.Message{Thread: th, Author: "dave", TS: 10, Body: "poof", ExpiresIn: 1})
	s.InsertMessage(ctx, &model.Message{Thread: th, Author: "me", TS: 11, Body: "mine", ExpiresIn: 1, Outgoing: true})
	far := time.Now().Add(time.Hour).UnixMilli()
	exp, err := s.Expired(ctx, far)
	if err != nil || len(exp) != 1 || exp[0].TS != 11 {
		t.Fatalf("before read, only the sent message expires: %+v %v", exp, err)
	}
	s.MarkThreadRead(ctx, th)
	if exp, _ := s.Expired(ctx, far); len(exp) != 2 {
		t.Fatalf("after read both expire: %+v", exp)
	}
	if exp, _ := s.Expired(ctx, time.Now().Add(-time.Minute).UnixMilli()); len(exp) != 0 {
		t.Fatalf("nothing expires in the past: %+v", exp)
	}
}
