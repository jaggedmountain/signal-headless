// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

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

func TestPurgeDeleted(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	th := model.ThreadID("erin")
	s.EnsureThread(ctx, th, model.Direct, "")
	s.InsertMessage(ctx, &model.Message{Thread: th, Author: "erin", TS: 10, Body: "gone soon"})
	s.InsertMessage(ctx, &model.Message{Thread: th, Author: "erin", TS: 20, Body: "kept", Quote: &model.Quote{Author: "erin", TS: 10, Text: "gone soon"}})
	before := time.Now().UnixMilli()
	if _, err := s.ApplyDelete(ctx, th, "erin", 10); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.ScrubQuotes(ctx, th, "erin", 10); err != nil || len(ids) != 1 {
		t.Fatalf("scrub = %v %v", ids, err)
	}
	if r, err := s.PurgeDeleted(ctx, before-1); err != nil || len(r) != 0 {
		t.Fatalf("too early to purge: %+v %v", r, err)
	}
	r, err := s.PurgeDeleted(ctx, time.Now().UnixMilli())
	if err != nil || len(r) != 1 || r[0].Thread != th {
		t.Fatalf("purge = %+v %v", r, err)
	}
	msgs, _ := s.Messages(ctx, th, 0, 10)
	if len(msgs) != 1 || msgs[0].Body != "kept" {
		t.Fatalf("left = %+v", msgs)
	}
	if q := msgs[0].Quote; q == nil || q.Text != "" || q.TS != 10 {
		t.Fatalf("quote of a deleted message keeps its text: %+v", q)
	}
	if ts, _ := s.Threads(ctx); len(ts) != 1 || ts[0].LastPreview != "kept" {
		t.Fatalf("thread preview = %+v", ts)
	}
	// A reply arriving after the deletion doesn't bring the text back.
	s.InsertMessage(ctx, &model.Message{Thread: th, Author: "erin", TS: 30, Body: "late", Quote: &model.Quote{Author: "erin", TS: 20, Text: "kept"}})
	s.ApplyDelete(ctx, th, "erin", 20)
	s.InsertMessage(ctx, &model.Message{Thread: th, Author: "frank", TS: 40, Body: "later", Quote: &model.Quote{Author: "erin", TS: 20, Text: "kept"}})
	msgs, _ = s.Messages(ctx, th, 0, 10)
	if q := msgs[len(msgs)-1].Quote; q == nil || q.Text != "" {
		t.Fatalf("late reply revived deleted text: %+v", q)
	}
}

func TestLinkPreviewStorage(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	th := model.ThreadID("gina")
	s.EnsureThread(ctx, th, model.Direct, "")
	m := &model.Message{Thread: th, Author: "gina", TS: 5, Body: "https://example.com/",
		Attachments: []model.Attachment{{Filename: "doc.pdf"}, {Filename: "preview", Kind: model.AttachmentPreview, Pointer: []byte("p")}},
		Previews:    []model.LinkPreview{{URL: "https://example.com/", Title: "Example", Description: "An example", Date: 7, Image: 1}}}
	if _, err := s.InsertMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, err := s.Message(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Previews) != 1 || got.Previews[0] != m.Previews[0] {
		t.Fatalf("previews = %+v", got.Previews)
	}
	if len(got.Attachments) != 2 || got.Attachments[1].Kind != model.AttachmentPreview || got.Attachments[0].Kind != "" {
		t.Fatalf("attachments = %+v", got.Attachments)
	}
	if _, err := s.ApplyDelete(ctx, th, "gina", 5); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Message(ctx, m.ID); len(got.Previews) != 0 {
		t.Fatalf("deleted message keeps its preview: %+v", got.Previews)
	}
}

func TestStatsAndPurge(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	for _, th := range []model.ThreadID{"hal", "ida"} {
		s.EnsureThread(ctx, th, model.Direct, "")
	}
	s.InsertMessage(ctx, &model.Message{Thread: "hal", Author: "hal", TS: 100, Body: "old",
		Attachments: []model.Attachment{{Filename: "a", Path: "/att/a", State: model.AttachmentDone}, {Filename: "b", State: model.AttachmentFailed, Pointer: []byte("p")}}})
	s.InsertMessage(ctx, &model.Message{Thread: "hal", Author: "hal", TS: 300, Body: "new"})
	s.InsertMessage(ctx, &model.Message{Thread: "ida", Author: "ida", TS: 150, Body: "old too"})
	st, err := s.Stats(ctx)
	if err != nil || st.Threads != 2 || st.Messages != 3 || st.Attachments != 2 || st.AttachmentsFailed != 1 || st.OldestTS != 100 || st.NewestTS != 300 {
		t.Fatalf("stats = %+v %v", st, err)
	}
	dry, err := s.Purge(ctx, 200, "", true)
	if err != nil || dry.Messages != 2 || dry.Attachments != 2 || len(dry.Paths) != 1 || len(dry.Threads) != 2 {
		t.Fatalf("dry run = %+v %v", dry, err)
	}
	if st, _ := s.Stats(ctx); st.Messages != 3 {
		t.Fatal("dry run deleted something")
	}
	if r, _ := s.Purge(ctx, 200, "ida", false); r.Messages != 1 || len(r.Threads) != 1 {
		t.Fatalf("scoped purge = %+v", r)
	}
	if n, err := s.RetryFailedAttachments(ctx); err != nil || n != 1 {
		t.Fatalf("retry failed = %d %v", n, err)
	}
	r, err := s.Purge(ctx, 200, "", false)
	if err != nil || r.Messages != 1 || r.Attachments != 2 {
		t.Fatalf("purge = %+v %v", r, err)
	}
	st, _ = s.Stats(ctx)
	if st.Messages != 1 || st.Attachments != 0 || st.Threads != 2 {
		t.Fatalf("after purge = %+v", st)
	}
	if err := s.Vacuum(ctx); err != nil {
		t.Fatal(err)
	}
}
