package tui

import (
	"fmt"
	"hash/fnv"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"signal-headless/internal/model"
)

var (
	colAccent = lipgloss.Color("12")
	colDim    = lipgloss.Color("8")
	colErr    = lipgloss.Color("9")
	colOK     = lipgloss.Color("10")
	colWarn   = lipgloss.Color("11")

	stDim      = lipgloss.NewStyle().Foreground(colDim)
	stBold     = lipgloss.NewStyle().Bold(true)
	stSel      = lipgloss.NewStyle().Reverse(true)
	stHeader   = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	stErr      = lipgloss.NewStyle().Foreground(colErr)
	stSelMark  = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
	stUnread   = lipgloss.NewStyle().Bold(true)
	stSep      = lipgloss.NewStyle().Foreground(colDim)
	stQuote    = lipgloss.NewStyle().Foreground(colDim).Italic(true)
	stReaction = lipgloss.NewStyle().Foreground(colDim)

	namePalette = []string{"1", "2", "3", "4", "5", "6", "9", "10", "11", "12", "13", "14"}
)

func nameStyle(id string) lipgloss.Style {
	h := fnv.New32a()
	h.Write([]byte(id))
	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(namePalette[h.Sum32()%uint32(len(namePalette))]))
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if n <= 0 {
		return ""
	}
	return ansi.Truncate(s, n, "…")
}

func padRight(s string, w int) string {
	if d := w - ansi.StringWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return ansi.Truncate(s, w, "")
}

func (m *Model) View() tea.View {
	var v tea.View
	v.AltScreen = true
	v.WindowTitle = m.windowTitle()
	if m.width == 0 || m.height == 0 {
		v.SetContent("starting…")
		return v
	}
	if m.mode == modeHelp {
		v.SetContent(m.renderHelp())
		return v
	}
	bottom := m.renderBottom()
	bodyH := max(1, m.height-lipgloss.Height(bottom))
	side := m.renderSidebar(bodyH)
	main := m.renderMain(bodyH)
	sep := stDim.Render(strings.Repeat("│\n", bodyH-1) + "│")
	body := lipgloss.JoinHorizontal(lipgloss.Top, side, sep, main)
	v.SetContent(lipgloss.JoinVertical(lipgloss.Left, body, bottom))
	return v
}

func (m *Model) windowTitle() string {
	n := m.totalUnread()
	if n > 0 {
		return fmt.Sprintf("signal (%d)", n)
	}
	return "signal"
}

// --- sidebar ---

func (m *Model) renderSidebar(h int) string {
	w := m.sidebarWidth()
	vt := m.visibleThreads()
	lines := make([]string, 0, h)
	title := " Threads"
	if m.showArchived {
		title += " (+archived)"
	}
	lines = append(lines, stHeader.Render(padRight(title, w)))
	// Keep the current thread in view.
	rows := h - 1
	start := 0
	if i := m.threadIndex(m.cur); i >= rows {
		start = i - rows + 1
	}
	for i := start; i < len(vt) && len(lines) < h; i++ {
		t := vt[i]
		mark := "  "
		if t.Unread > 0 {
			mark = "● "
		}
		name := t.Title
		if t.Kind == model.Group {
			name = "#" + name
		}
		if t.Archived {
			name += " (archived)"
		}
		count := ""
		if t.Unread > 0 {
			count = fmt.Sprintf(" %d", t.Unread)
		}
		if len(m.typing[t.ID]) > 0 {
			count = " …" + count
		}
		nameW := w - 2 - ansi.StringWidth(count) - 1
		row := mark + padRight(truncate(name, nameW), nameW) + count + " "
		row = padRight(row, w)
		switch {
		case t.ID == m.cur:
			row = stSel.Render(row)
		case t.Unread > 0:
			row = stUnread.Render(row)
		}
		lines = append(lines, row)
	}
	for len(lines) < h {
		lines = append(lines, strings.Repeat(" ", w))
	}
	return strings.Join(lines, "\n")
}

// --- message pane ---

func (m *Model) paneHeight() int {
	return max(3, m.height-6)
}

type block struct {
	lines []string
	msgIx int // index in curMsgs, -1 for separators
}

func (m *Model) renderMain(h int) string {
	w := m.mainWidth()
	header := m.renderHeader(w)
	paneH := h - 1
	if m.cur == "" {
		empty := stDim.Render("  No conversation selected. Press C to start one, ? for help.")
		return lipgloss.JoinVertical(lipgloss.Left, header, lipgloss.Place(w, paneH, lipgloss.Left, lipgloss.Top, empty))
	}
	msgs := m.curMsgs()
	if len(msgs) == 0 {
		note := "  No messages yet."
		if !m.loaded[m.cur] {
			note = "  Loading…"
		}
		return lipgloss.JoinVertical(lipgloss.Left, header, lipgloss.Place(w, paneH, lipgloss.Left, lipgloss.Top, stDim.Render(note)))
	}

	sel := m.sel
	if sel < 0 || sel >= len(msgs) {
		sel = len(msgs) - 1
	}
	blocks := m.buildBlocks(msgs, w, sel)
	// Find the selected block and lay out lines so it is visible, anchored
	// to the bottom when following the newest message.
	var all []string
	selStart, selEnd := 0, 0
	for _, b := range blocks {
		if b.msgIx == sel {
			selStart = len(all)
			selEnd = len(all) + len(b.lines)
		}
		all = append(all, b.lines...)
	}
	end := len(all)
	if m.sel >= 0 {
		// Center-ish: show the selected message with context below it.
		end = min(len(all), max(selEnd+paneH/3, paneH))
		if selStart < end-paneH {
			end = selStart + paneH
		}
	}
	end = max(0, end-m.scroll)
	start := max(0, end-paneH)
	view := all[start:min(end, len(all))]
	for len(view) < paneH {
		view = append([]string{""}, view...)
	}
	for i, l := range view {
		if ansi.StringWidth(l) > w {
			view[i] = ansi.Truncate(l, w, "")
		}
	}
	if start == 0 && !m.exhausted[m.cur] && len(view) > 0 {
		view[0] = stDim.Render(padRight("  ↑ older messages (g / k to load)", w))
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, strings.Join(view, "\n"))
}

func (m *Model) renderHeader(w int) string {
	t := m.thread(m.cur)
	title := " signal-headless"
	if t != nil {
		title = " " + t.Title
		if t.Kind == model.Group {
			title = " #" + t.Title
		}
		if t.ExpireTimer > 0 {
			title += stDim.Render(" ⏱ " + (time.Duration(t.ExpireTimer) * time.Second).String())
		}
	}
	if who := m.typingNames(m.cur); who != "" {
		title += stDim.Render("  " + who + " typing…")
	}
	return stHeader.Render(padRight(title, w))
}

func (m *Model) typingNames(id model.ThreadID) string {
	var names []string
	for _, ti := range m.typing[id] {
		n := ti.name
		if n == "" {
			n = "someone"
		}
		names = append(names, n)
	}
	return strings.Join(names, ", ")
}

func dayLabel(t time.Time) string {
	now := time.Now()
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return "Today"
	case y1 == y2 && m1 == m2 && d1 == d2-1:
		return "Yesterday"
	case y1 == y2:
		return t.Format("Mon 2 Jan")
	}
	return t.Format("Mon 2 Jan 2006")
}

func (m *Model) buildBlocks(msgs []*model.Message, w, sel int) []block {
	t := m.thread(m.cur)
	isGroup := t != nil && t.Kind == model.Group
	nameW := 4
	for _, x := range msgs {
		nameW = max(nameW, min(16, ansi.StringWidth(m.authorLabel(x))))
	}
	nameW = min(nameW, max(4, w/5)) // leave room for text in narrow panes
	var out []block
	var lastDay string
	for i, x := range msgs {
		ts := time.UnixMilli(x.TS)
		if d := dayLabel(ts); d != lastDay {
			lastDay = d
			label := "── " + d + " "
			out = append(out, block{msgIx: -1, lines: []string{stSep.Render(label + strings.Repeat("─", max(0, w-ansi.StringWidth(label))))}})
		}
		out = append(out, block{msgIx: i, lines: m.renderMessage(x, w, nameW, i == sel && m.sel >= 0, isGroup)})
	}
	return out
}

func (m *Model) authorLabel(x *model.Message) string {
	if x.Outgoing || x.Author == m.status.Account.ACI {
		return "me"
	}
	if x.AuthorName != "" {
		return x.AuthorName
	}
	return shortID(x.Author)
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "PNI:")
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func statusGlyph(s model.Status) string {
	switch s {
	case model.StatusSending:
		return stDim.Render("…")
	case model.StatusSent:
		return stDim.Render("✓")
	case model.StatusDelivered:
		return stDim.Render("✓✓")
	case model.StatusRead:
		return lipgloss.NewStyle().Foreground(colAccent).Render("✓✓")
	case model.StatusFailed:
		return stErr.Render("✗ failed")
	}
	return ""
}

func humanSize(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.0f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
}

// renderMessage lays out one message: "HH:MM name  text", wrapped under the
// text column, with quote, attachment and reaction lines.
func (m *Model) renderMessage(x *model.Message, w, nameW int, selected, isGroup bool) []string {
	clock := time.UnixMilli(x.TS).Format("15:04")
	name := m.authorLabel(x)
	ns := nameStyle(x.Author)
	if name == "me" {
		ns = lipgloss.NewStyle().Bold(true).Foreground(colDim)
	}
	prefixW := 2 + 5 + 1 + nameW + 2 // mark, clock, space, name, gap
	textW := max(8, w-prefixW)
	indent := strings.Repeat(" ", prefixW)

	var body []string
	if q := x.Quote; q != nil {
		qn := "me"
		if q.Author != m.status.Account.ACI {
			qn = shortID(q.Author)
			for _, y := range m.curMsgs() {
				if y.Author == q.Author && y.AuthorName != "" {
					qn = y.AuthorName
					break
				}
			}
		}
		body = append(body, stQuote.Render(truncate("↱ "+qn+": "+q.Text, textW)))
	}
	text := x.Body
	switch {
	case x.Deleted:
		text = stDim.Render("(message deleted)")
	case text == "" && x.Sticker != "":
		text = "[sticker " + x.Sticker + "]"
	}
	if text != "" {
		for _, para := range strings.Split(text, "\n") {
			wrapped := ansi.Wrap(para, textW, " -")
			body = append(body, strings.Split(wrapped, "\n")...)
		}
	}
	for _, a := range x.Attachments {
		label := a.Filename
		if label == "" && a.Path != "" {
			label = filepath.Base(a.Path)
		}
		if label == "" {
			label = a.ContentType
		}
		if a.VoiceNote {
			label = "voice note"
		}
		info := humanSize(a.Size)
		switch a.State {
		case model.AttachmentPending:
			info = strings.TrimSpace(info + " downloading…")
		case model.AttachmentFailed:
			info = stErr.Render("download failed (R to retry)")
		}
		line := "📎 " + label
		if info != "" {
			line += " " + stDim.Render("("+info+")")
		}
		body = append(body, truncate(line, textW))
	}
	if len(body) == 0 {
		body = []string{""}
	}
	suffix := ""
	if x.EditedAt != 0 && !x.Deleted {
		suffix += stDim.Render(" (edited)")
	}
	if x.Outgoing {
		suffix += " " + statusGlyph(x.Status)
	}
	if x.ExpiresIn > 0 && !x.Deleted {
		suffix += stDim.Render(" ⏱")
	}
	last := len(body) - 1
	if ansi.StringWidth(body[last])+ansi.StringWidth(suffix) <= textW {
		body[last] += suffix
	} else if suffix != "" {
		body = append(body, strings.TrimLeft(suffix, " "))
	}
	if len(x.Reactions) > 0 {
		var parts []string
		for _, r := range x.Reactions {
			who := "me"
			if r.Reactor != m.status.Account.ACI {
				who = shortID(r.Reactor)
				for _, y := range m.curMsgs() {
					if y.Author == r.Reactor && y.AuthorName != "" {
						who = y.AuthorName
						break
					}
				}
			}
			parts = append(parts, r.Emoji+" "+who)
		}
		body = append(body, stReaction.Render(truncate(strings.Join(parts, "  "), textW)))
	}

	mark := "  "
	if selected {
		mark = stSelMark.Render("▌ ")
	}
	lines := make([]string, len(body))
	for i, b := range body {
		if i == 0 {
			lines[i] = mark + stDim.Render(clock) + " " + ns.Render(padRight(truncate(name, nameW), nameW)) + "  " + b
		} else {
			if selected {
				lines[i] = stSelMark.Render("▌ ") + indent[2:] + b
			} else {
				lines[i] = indent + b
			}
		}
	}
	return lines
}

// --- bottom: compose / prompt / status ---

func (m *Model) renderBottom() string {
	var parts []string
	w := m.width
	if m.cur != "" && m.mode != modePrompt {
		if r := m.replyTo[m.cur]; r != nil {
			parts = append(parts, stQuote.Render(truncate("replying to "+m.authorLabel(r)+": "+r.Body, w-20)+"  (ctrl+r cancel)"))
		}
		if atts := m.attach[m.cur]; len(atts) > 0 {
			var names []string
			for _, a := range atts {
				names = append(names, filepath.Base(a))
			}
			parts = append(parts, stDim.Render(truncate("📎 "+strings.Join(names, ", ")+"  (A clears)", w)))
		}
	}
	switch m.mode {
	case modePrompt:
		parts = append(parts, m.prompt.View())
		if len(m.completions) > 1 {
			parts = append(parts, stDim.Render(truncate(strings.Join(m.completionLabels(), "  "), w)))
		}
	case modeCompose:
		parts = append(parts, m.compose.View())
	default:
		if m.cur != "" {
			draft := m.compose.Value()
			hint := "enter/i to write · r reply · e react · a attach · J/K switch · ? help"
			if draft != "" {
				hint = "draft: " + strings.ReplaceAll(draft, "\n", " ⏎ ")
			}
			parts = append(parts, stDim.Render(truncate("│ "+hint, w)))
		}
	}
	parts = append(parts, m.renderStatus(w))
	return strings.Join(parts, "\n")
}

func (m *Model) completionLabels() []string {
	out := make([]string, len(m.completions))
	for i, c := range m.completions {
		label := c
		if m.promptKind == promptAttach {
			label = filepath.Base(strings.TrimSuffix(c, "/"))
			if strings.HasSuffix(c, "/") {
				label += "/"
			}
		}
		if i == m.compIdx {
			label = stSel.Render(label)
		}
		out[i] = label
	}
	return out
}

func (m *Model) renderStatus(w int) string {
	var left string
	switch {
	case !m.connected:
		left = stErr.Render("● daemon disconnected")
	case m.status.Connection == model.ConnConnected:
		left = lipgloss.NewStyle().Foreground(colOK).Render("●") + " " + m.status.Account.Number
		if !m.status.QueueEmpty {
			left += stDim.Render(" (syncing)")
		}
	default:
		left = lipgloss.NewStyle().Foreground(colWarn).Render("● "+string(m.status.Connection)) + " " + m.status.Account.Number
	}
	if n := m.totalUnread(); n > 0 {
		left += stBold.Render(fmt.Sprintf("  %d unread", n))
	}
	msg := ""
	switch {
	case m.errText != "":
		msg = stErr.Render(m.errText)
	case m.flash != "" && time.Since(m.flashAt) < 8*time.Second:
		msg = m.flash
	}
	right := stDim.Render("? help")
	space := w - ansi.StringWidth(left) - ansi.StringWidth(right) - 4
	if msg != "" {
		msg = truncate(msg, max(0, space))
	}
	mid := "  " + msg
	pad := w - ansi.StringWidth(left) - ansi.StringWidth(mid) - ansi.StringWidth(right)
	return left + mid + strings.Repeat(" ", max(1, pad)) + right
}

var helpText = `signal-headless — keys

 Threads                          Messages
   J / K, ctrl+n / ctrl+p  next / previous thread   j / k        select message
   n, tab                  next unread thread       g / G        oldest / newest
   C, m                    new conversation (to:)   ctrl+u/d     page up / down
   :archive / :archived    hide / show archived     /            search (enter again: next)

 Writing                                           Acting on the selected message
   enter, i, c   compose in this thread            r     reply (quote)
   enter         send                              e, +  react (1-6 quick picks)
   alt+enter     newline (also ctrl+j)             D     delete for everyone (own)
   ctrl+e        edit draft in $EDITOR             o     open attachments
   ctrl+t, a     attach a file (tab completes)     R     retry failed download
   A             clear attachments                 y     copy text
   esc           leave compose (draft is kept)     esc   back to newest

 Commands (:)   to NAME · attach PATH · detach · archive · unarchive · archived
                read · search TEXT · react EMOJI · retry · open · help · quit

 q quit · ? close this help`

func (m *Model) renderHelp() string {
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colAccent).Padding(0, 1).Render(helpText)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}
