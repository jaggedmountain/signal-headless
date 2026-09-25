// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
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

func TestEmojize(t *testing.T) {
	for in, want := range map[string]string{
		"haha :joy:":              "haha 😂",
		":JOY::+1:":               "😂👍",
		"meet at 12:30:45":        "meet at 12:30:45",
		":not_an_emoji: stays":    ":not_an_emoji: stays",
		"a:b:c":                   "a:b:c",
		"party :tada: :thumbsup:": "party 🎉 👍",
	} {
		if got := emojize(in); got != want {
			t.Errorf("emojize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuitNeedsConfirmation(t *testing.T) {
	press := func(m *Model, k string) {
		var msg tea.KeyPressMsg
		switch k {
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		default:
			msg = tea.KeyPressMsg{Code: rune(k[0]), Text: k}
		}
		m.handleKey(msg)
	}
	m := testModel()
	press(m, "q")
	if m.quit || m.mode != modePrompt || m.promptKind != promptConfirmQuit {
		t.Fatalf("q should ask first: quit=%v mode=%v", m.quit, m.mode)
	}
	press(m, "x")
	if m.quit || m.mode != modeNormal {
		t.Fatal("other keys should cancel")
	}
	press(m, "q")
	press(m, "q")
	if !m.quit {
		t.Fatal("q q should quit")
	}
	m = testModel()
	press(m, "q")
	press(m, "enter")
	if !m.quit {
		t.Fatal("q enter should quit")
	}
	m = testModel()
	m.cur = ""
	press(m, "q")
	if m.mode != modePrompt {
		t.Fatal("quit prompt should work with no thread open")
	}
}

func TestRemoveMessageKeepsSelection(t *testing.T) {
	m := testModel()
	m.sel = 3 // TS 4000
	m.removeMessage("a", 2)
	if len(m.msgs["a"]) != 4 || m.msgs["a"][m.sel].TS != 4000 {
		t.Fatalf("sel = %d over %d messages", m.sel, len(m.msgs["a"]))
	}
	m.removeMessage("a", 4) // the selected one
	if m.msgs["a"][m.sel].TS != 3000 {
		t.Fatalf("selection should fall back to the previous message, got ts %d", m.msgs["a"][m.sel].TS)
	}
	m.removeMessage("a", 99) // unknown: no-op
	if len(m.msgs["a"]) != 3 {
		t.Fatal("unknown id removed something")
	}
}

func TestDraftLinkPreview(t *testing.T) {
	m := testModel()
	m.status.LinkPreviews = true
	m.mode = modeCompose
	m.compose.SetValue("tea: https://tea.example/brew")
	check := func() tea.Cmd {
		m.previewSeq++
		return m.checkPreview(previewCheckMsg{seq: m.previewSeq, thread: m.cur})
	}
	if cmd := check(); cmd == nil || !m.preview.loading || m.preview.url != "https://tea.example/brew" {
		t.Fatalf("lookup should start: %+v", m.preview)
	}
	if !strings.Contains(ansi.Strip(m.View().Content), "fetching preview") {
		t.Fatal("loading line missing")
	}
	// A stale check (typing continued) does nothing.
	if cmd := m.checkPreview(previewCheckMsg{seq: m.previewSeq - 1, thread: m.cur}); cmd != nil {
		t.Fatal("stale check ran")
	}
	lp := &model.OutgoingPreview{URL: "https://tea.example/brew", Title: "How to  brew", Image: "/tmp/x.png"}
	m.gotPreview(previewMsg{thread: m.cur, url: lp.URL, p: lp})
	if view := ansi.Strip(m.View().Content); !strings.Contains(view, "🔗 How to brew — tea.example") || !strings.Contains(view, "ctrl+x") {
		t.Fatalf("preview line missing:\n%s", view)
	}
	m.sendDraft()
	if len(m.lastSend.Previews) != 1 || m.lastSend.Previews[0].Image != "/tmp/x.png" {
		t.Fatalf("sent previews = %+v", m.lastSend.Previews)
	}
	if m.preview.url != "" {
		t.Fatal("preview kept after sending")
	}

	// ctrl+x drops it, and the same link is not offered again.
	m.compose.SetValue("again https://tea.example/brew")
	check()
	m.gotPreview(previewMsg{thread: m.cur, url: lp.URL, p: lp})
	m.handleKey(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
	if m.preview.url != "" || check() != nil {
		t.Fatalf("dropped preview came back: %+v", m.preview)
	}
	m.sendDraft()
	if len(m.lastSend.Previews) != 0 {
		t.Fatal("dropped preview was sent")
	}

	// Off: nothing is looked up.
	m.opts.LinkPreviews = "off"
	m.compose.SetValue("https://tea.example/other")
	if check() != nil {
		t.Fatal("lookup while off")
	}
}

func TestReceivedPreviewLine(t *testing.T) {
	m := testModel()
	m.msgs["a"] = append(m.msgs["a"], &model.Message{ID: 50, Thread: "a", Author: "alice", AuthorName: "Alice", TS: 9000,
		Body: "see https://www.tea.example/x", Previews: []model.LinkPreview{{URL: "https://www.tea.example/x", Title: "Tea time", Image: -1}}})
	if view := ansi.Strip(m.View().Content); !strings.Contains(view, "🔗 Tea time — tea.example") {
		t.Fatalf("received preview line missing:\n%s", view)
	}
}

func TestRealCursorOnInput(t *testing.T) {
	m := testModel()
	m.mode = modeCompose
	m.compose.Focus()
	m.compose.SetValue("hello")
	v := m.View()
	if v.Cursor == nil {
		t.Fatal("compose should place the terminal cursor")
	}
	lines := strings.Split(v.Content, "\n")
	if got := ansi.Strip(lines[v.Cursor.Y]); !strings.Contains(got, "hello") {
		t.Fatalf("cursor row %d is %q, want the compose line", v.Cursor.Y, got)
	}
	if v.Cursor.X != 2+len("hello") {
		t.Fatalf("cursor x = %d", v.Cursor.X)
	}
	// With a reply line above, the cursor moves down with the input.
	m.replyTo[m.cur] = m.msgs["a"][0]
	v = m.View()
	lines = strings.Split(v.Content, "\n")
	if got := ansi.Strip(lines[v.Cursor.Y]); !strings.Contains(got, "hello") {
		t.Fatalf("with reply: cursor row is %q", got)
	}
	m.mode = modeNormal
	m.compose.Blur()
	if m.View().Cursor != nil {
		t.Fatal("no cursor outside the input")
	}
}

func TestComposeRows(t *testing.T) {
	for _, c := range []struct {
		text string
		w    int
		want int
	}{
		{"", 20, 1}, {"short", 20, 1}, {"a\nb\nc", 20, 3},
		{strings.Repeat("word ", 10), 20, 3}, // 50 columns wrap to 3 rows
	} {
		if got := composeRows(c.text, c.w); got != c.want {
			t.Errorf("composeRows(%q, %d) = %d, want %d", c.text, c.w, got, c.want)
		}
	}
}

func TestClearAndRestoreDraft(t *testing.T) {
	m := testModel()
	m.compose.SetValue("half-written thought")
	m.replyTo["a"] = m.msgs["a"][1]
	m.handleKey(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if m.compose.Value() != "" || m.replyTo["a"] != nil {
		t.Fatalf("x should clear: %q %v", m.compose.Value(), m.replyTo["a"])
	}
	m.handleKey(tea.KeyPressMsg{Code: 'u', Text: "u"})
	if m.compose.Value() != "half-written thought" || m.replyTo["a"] != m.msgs["a"][1] {
		t.Fatalf("u should restore: %q", m.compose.Value())
	}
	m.handleKey(tea.KeyPressMsg{Code: 'u', Text: "u"})
	if m.compose.Value() != "half-written thought" {
		t.Fatal("a second u changes nothing")
	}
	// Restoring only applies to the thread it was cleared in.
	m.handleKey(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m.open("b")
	m.handleKey(tea.KeyPressMsg{Code: 'u', Text: "u"})
	if m.compose.Value() != "" {
		t.Fatal("restored into the wrong thread")
	}
}

func TestTimerLabels(t *testing.T) {
	for secs, want := range map[uint32]string{0: "off", 30: "30 seconds", 60: "1 minute", 3600: "1 hour", 28800: "8 hours", 86400: "1 day", 604800: "1 week", 2419200: "4 weeks", 90: "90 seconds"} {
		if got := model.TimerLabel(secs); got != want {
			t.Errorf("TimerLabel(%d) = %q, want %q", secs, got, want)
		}
	}
	m := testModel()
	m.msgs["a"] = append(m.msgs["a"], &model.Message{ID: 60, Thread: "a", Author: "alice", TS: 9500, Body: "⏱ disappearing messages: 1h0m0s"})
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "⌛ disappearing messages: 1 hour") || strings.Contains(view, "⏱") {
		t.Fatalf("old timer notice not modernised:\n%s", view)
	}
}

func TestTimerShownOnceInHeader(t *testing.T) {
	m := testModel()
	m.threads[0].ExpireTimer = 3600
	for _, x := range m.msgs["a"] {
		x.ExpiresIn = 3600
	}
	view := ansi.Strip(m.View().Content)
	if n := strings.Count(view, "⌛"); n != 1 {
		t.Fatalf("hourglass shown %d times, want once (header):\n%s", n, view)
	}
	if !strings.Contains(view, "⌛ 1 hour") {
		t.Fatalf("header should carry the timer:\n%s", view)
	}
}
