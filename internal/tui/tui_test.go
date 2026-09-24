package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"signal-headless/internal/model"
)

func testModel() *Model {
	m := New(nil, Options{})
	m.width, m.height = 100, 30
	m.status.Account.ACI = "me-aci"
	m.threads = []model.Thread{
		{ID: "a", Title: "Alice", LastTS: 3, Unread: 1},
		{ID: "b", Title: "Bob", LastTS: 2},
		{ID: "g", Title: "Group", Kind: model.Group, LastTS: 1, Unread: 2},
	}
	m.cur = "a"
	m.loaded["a"] = true
	m.exhausted["a"] = true
	for i := int64(1); i <= 5; i++ {
		m.msgs["a"] = append(m.msgs["a"], &model.Message{ID: i, Thread: "a", Author: "alice", AuthorName: "Alice", TS: i * 1000, Body: "msg"})
	}
	m.layout()
	return m
}

func TestMoveSel(t *testing.T) {
	m := testModel()
	m.moveSel(-1)
	if m.sel != 4 {
		t.Fatalf("first k should highlight newest (4), got %d", m.sel)
	}
	m.moveSel(-2)
	if m.sel != 2 {
		t.Fatalf("sel = %d, want 2", m.sel)
	}
	m.moveSel(-10)
	if m.sel != 0 {
		t.Fatalf("clamped sel = %d", m.sel)
	}
	m.moveSel(10)
	if m.sel != -1 {
		t.Fatalf("moving past the end should follow newest, got %d", m.sel)
	}
}

func TestUpsertKeepsOrderAndSelection(t *testing.T) {
	m := testModel()
	m.sel = 2 // message TS 3000
	m.upsertMessage(&model.Message{ID: 10, Thread: "a", Author: "bob", TS: 2500, Body: "late"})
	if got := m.msgs["a"][m.sel].TS; got != 3000 {
		t.Fatalf("selection moved to ts %d", got)
	}
	for i := 1; i < len(m.msgs["a"]); i++ {
		if m.msgs["a"][i-1].TS > m.msgs["a"][i].TS {
			t.Fatal("messages out of order")
		}
	}
	m.upsertMessage(&model.Message{ID: 10, Thread: "a", Author: "bob", TS: 2500, Body: "edited"})
	if len(m.msgs["a"]) != 6 {
		t.Fatalf("update duplicated message: %d", len(m.msgs["a"]))
	}
	// Threads not yet loaded are left for the initial fetch.
	m.upsertMessage(&model.Message{ID: 11, Thread: "b", TS: 1})
	if len(m.msgs["b"]) != 0 {
		t.Fatal("unloaded thread should not accumulate messages")
	}
}

func TestNextUnreadAndThreadMove(t *testing.T) {
	m := testModel()
	m.nextUnread()
	if m.cur != "g" {
		t.Fatalf("next unread = %s", m.cur)
	}
	m.moveThread(-1)
	if m.cur != "b" {
		t.Fatalf("prev thread = %s", m.cur)
	}
	m.moveThread(-5)
	if m.cur != "a" {
		t.Fatalf("clamped = %s", m.cur)
	}
}

func TestDraftsPerThread(t *testing.T) {
	m := testModel()
	m.compose.SetValue("for alice")
	m.open("b")
	if m.compose.Value() != "" {
		t.Fatal("bob's compose box should start empty")
	}
	m.compose.SetValue("for bob")
	m.open("a")
	if m.compose.Value() != "for alice" {
		t.Fatalf("alice draft = %q", m.compose.Value())
	}
}

func TestRenderFitsWidth(t *testing.T) {
	m := testModel()
	long := strings.Repeat("supercalifragilistic ", 20) + strings.Repeat("x", 300)
	m.msgs["a"] = append(m.msgs["a"], &model.Message{
		ID: 99, Thread: "a", Author: "me-aci", Outgoing: true, Status: model.StatusRead, TS: time.Now().UnixMilli(),
		Body: long, Quote: &model.Quote{Author: "alice", TS: 1000, Text: long},
		Attachments: []model.Attachment{{Filename: strings.Repeat("f", 200) + ".png", Size: 12345, State: model.AttachmentPending}},
		Reactions:   []model.Reaction{{Reactor: "alice", Emoji: "👍"}},
	})
	for _, w := range []int{40, 80, 100, 200} {
		m.width = w
		m.layout()
		view := m.View().Content
		for i, line := range strings.Split(view, "\n") {
			if lw := ansi.StringWidth(line); lw > w {
				t.Fatalf("width %d: line %d is %d wide: %q", w, i, lw, ansi.Strip(line))
			}
		}
		if got := strings.Count(view, "\n") + 1; got > m.height {
			t.Fatalf("width %d: %d lines > height %d", w, got, m.height)
		}
	}
}

func TestCompletePath(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "report.pdf"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir, ".hidden"), []byte("x"), 0o600)
	os.Mkdir(filepath.Join(dir, "reports"), 0o700)
	got := completePath(dir + "/re")
	want := []string{dir + "/readme.txt", dir + "/report.pdf", dir + "/reports/"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("completePath = %v", got)
	}
	if p := commonPrefix(got); p != dir+"/re" {
		t.Fatalf("common prefix = %q", p)
	}
	if got := completePath(dir + "/"); len(got) != 3 {
		t.Fatalf("hidden files should be skipped: %v", got)
	}
}

func TestCompleteRecipient(t *testing.T) {
	m := testModel()
	m.contacts = []model.Contact{{ID: "x", Name: "Alan Turing"}, {ID: "y", Name: "Blocked Al", Blocked: true}}
	got := m.completeRecipient("al")
	if strings.Join(got, ",") != "Alan Turing,Alice" {
		t.Fatalf("completeRecipient = %v", got)
	}
}
