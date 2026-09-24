// Package tui is the interactive client (--shell): an aerc-style layout with
// a thread sidebar, a message pane and a compose box. It talks to the daemon
// exclusively over RPC.
package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/bubbles/v2/key"

	"signal-headless/internal/model"
	"signal-headless/internal/rpc"
)

type mode int

const (
	modeNormal mode = iota
	modeCompose
	modePrompt
	modeHelp
)

type promptKind int

const (
	promptCommand promptKind = iota
	promptTo
	promptAttach
	promptReact
	promptSearch
	promptConfirmDelete
)

// quickReactions are selectable with 1-6 in the reaction prompt.
var quickReactions = []string{"👍", "❤️", "😂", "😮", "😢", "🙏"}

type Options struct {
	Bell bool // ring the terminal bell for messages in other threads
}

type Model struct {
	cli  *rpc.Client
	opts Options

	width, height int
	status        rpc.StatusResult
	connected     bool

	threads      []model.Thread
	showArchived bool
	cur          model.ThreadID
	msgs         map[model.ThreadID][]*model.Message
	loaded       map[model.ThreadID]bool
	exhausted    map[model.ThreadID]bool // no older messages to fetch
	sel          int                     // selected message index; -1 = follow newest
	scroll       int                     // extra lines scrolled up in the message pane

	mode    mode
	compose textarea.Model
	drafts  map[model.ThreadID]string
	replyTo map[model.ThreadID]*model.Message
	attach  map[model.ThreadID][]string
	typedAt time.Time // last local keystroke that announced typing

	prompt      textinput.Model
	promptKind  promptKind
	completions []string
	compIdx     int

	typing map[model.ThreadID]map[string]typingInfo

	contacts []model.Contact
	groups   []model.GroupInfo

	search struct {
		query string
		hits  []int64 // message IDs, newest first
		pos   int
	}

	flash   string
	flashAt time.Time
	errText string
	quit    bool
	// blurred is set while the terminal reports it lost focus; the open
	// thread is then not marked read (no read receipts for unseen messages).
	blurred bool
}

type typingInfo struct {
	name  string
	until time.Time
}

func New(cli *rpc.Client, opts Options) *Model {
	ta := textarea.New()
	ta.Placeholder = "message…  (enter send · alt+enter newline · ctrl+e $EDITOR · ctrl+t attach · esc done)"
	ta.ShowLineNumbers = false
	ta.Prompt = "│ "
	ta.CharLimit = 0
	ta.MaxHeight = 8
	ta.SetHeight(1)
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j", "shift+enter"))

	pi := textinput.New()
	pi.Prompt = ":"

	return &Model{
		cli: cli, opts: opts,
		msgs: map[model.ThreadID][]*model.Message{}, loaded: map[model.ThreadID]bool{}, exhausted: map[model.ThreadID]bool{},
		drafts: map[model.ThreadID]string{}, replyTo: map[model.ThreadID]*model.Message{}, attach: map[model.ThreadID][]string{},
		typing:  map[model.ThreadID]map[string]typingInfo{},
		compose: ta, prompt: pi, sel: -1,
	}
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.subscribeCmd(), m.loadThreadsCmd(), m.loadContactsCmd(), m.listen(), tick())
}

// --- helpers ---

func (m *Model) setFlash(s string) {
	m.flash, m.flashAt = s, time.Now()
}

func (m *Model) visibleThreads() []model.Thread {
	out := make([]model.Thread, 0, len(m.threads))
	for _, t := range m.threads {
		if !t.Archived || m.showArchived || t.ID == m.cur {
			out = append(out, t)
		}
	}
	return out
}

func (m *Model) threadIndex(id model.ThreadID) int {
	for i, t := range m.visibleThreads() {
		if t.ID == id {
			return i
		}
	}
	return -1
}

func (m *Model) thread(id model.ThreadID) *model.Thread {
	for i := range m.threads {
		if m.threads[i].ID == id {
			return &m.threads[i]
		}
	}
	return nil
}

func (m *Model) upsertThread(t model.Thread) {
	if p := m.thread(t.ID); p != nil {
		*p = t
	} else {
		m.threads = append(m.threads, t)
	}
	sort.SliceStable(m.threads, func(i, j int) bool { return m.threads[i].LastTS > m.threads[j].LastTS })
}

func (m *Model) curMsgs() []*model.Message { return m.msgs[m.cur] }

func (m *Model) selected() *model.Message {
	msgs := m.curMsgs()
	if len(msgs) == 0 {
		return nil
	}
	if m.sel < 0 || m.sel >= len(msgs) {
		return msgs[len(msgs)-1]
	}
	return msgs[m.sel]
}

func (m *Model) totalUnread() int {
	n := 0
	for _, t := range m.threads {
		n += t.Unread
	}
	return n
}

// open switches to a thread, stashing the current draft.
func (m *Model) open(id model.ThreadID) tea.Cmd {
	if id == "" {
		return nil
	}
	var cmds []tea.Cmd
	if id != m.cur {
		if m.cur != "" {
			m.drafts[m.cur] = m.compose.Value()
			if !m.typedAt.IsZero() {
				cmds = append(cmds, m.typingCmd(m.cur, false))
				m.typedAt = time.Time{}
			}
		}
		m.cur = id
		m.compose.SetValue(m.drafts[id])
		m.sel, m.scroll = -1, 0
		m.search.hits = nil
	}
	if !m.loaded[id] {
		cmds = append(cmds, m.loadMessagesCmd(id, 0))
	}
	if t := m.thread(id); t != nil && t.Unread > 0 && !m.blurred {
		cmds = append(cmds, m.markReadCmd(id))
	}
	m.layout()
	return tea.Batch(cmds...)
}

func (m *Model) moveThread(delta int) tea.Cmd {
	vt := m.visibleThreads()
	if len(vt) == 0 {
		return nil
	}
	i := m.threadIndex(m.cur) + delta
	i = max(0, min(i, len(vt)-1))
	return m.open(vt[i].ID)
}

func (m *Model) nextUnread() tea.Cmd {
	vt := m.visibleThreads()
	start := m.threadIndex(m.cur)
	for k := 1; k <= len(vt); k++ {
		t := vt[(start+k+len(vt))%len(vt)]
		if t.Unread > 0 {
			return m.open(t.ID)
		}
	}
	m.setFlash("no unread threads")
	return nil
}

func (m *Model) moveSel(delta int) tea.Cmd {
	msgs := m.curMsgs()
	if len(msgs) == 0 {
		return nil
	}
	i := m.sel
	if i < 0 {
		if delta > 0 {
			return nil // already following the newest message
		}
		// First step up highlights the newest message.
		i = len(msgs) - 1
		delta++
	}
	i += delta
	var cmd tea.Cmd
	if i < 0 {
		i = 0
		cmd = m.loadOlder()
	}
	if i >= len(msgs)-1 && delta > 0 {
		m.sel = -1 // back to following the newest message
	} else {
		m.sel = min(i, len(msgs)-1)
	}
	m.scroll = 0
	return cmd
}

func (m *Model) loadOlder() tea.Cmd {
	msgs := m.curMsgs()
	if len(msgs) == 0 || m.exhausted[m.cur] {
		return nil
	}
	return m.loadMessagesCmd(m.cur, msgs[0].TS)
}

func (m *Model) layout() {
	w := m.mainWidth()
	m.compose.SetWidth(max(10, w))
	lines := strings.Count(m.compose.Value(), "\n") + 1
	m.compose.SetHeight(max(1, min(lines, 8)))
	m.prompt.SetWidth(max(10, m.width-4))
}

func (m *Model) sidebarWidth() int {
	return max(16, min(34, m.width/3))
}

func (m *Model) mainWidth() int {
	return max(20, m.width-m.sidebarWidth()-1)
}

// --- update ---

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case tea.BlurMsg:
		m.blurred = true
		return m, nil

	case tea.FocusMsg:
		m.blurred = false
		if t := m.thread(m.cur); t != nil && t.Unread > 0 {
			return m, m.markReadCmd(m.cur)
		}
		return m, nil

	case statusMsg:
		m.status = rpc.StatusResult(msg)
		m.connected = true
		return m, nil

	case threadsMsg:
		m.threads = []model.Thread(msg)
		sort.SliceStable(m.threads, func(i, j int) bool { return m.threads[i].LastTS > m.threads[j].LastTS })
		if m.cur == "" {
			if vt := m.visibleThreads(); len(vt) > 0 {
				return m, m.open(vt[0].ID)
			}
		}
		return m, nil

	case contactsMsg:
		m.contacts = msg
		return m, nil

	case groupsMsg:
		m.groups = msg
		return m, nil

	case messagesMsg:
		m.mergeMessages(msg)
		return m, nil

	case sentMsg:
		if msg.err != nil {
			m.errText = "send failed: " + msg.err.Error()
		}
		return m, nil

	case resolvedMsg:
		if msg.err != nil {
			m.errText = msg.err.Error()
			return m, nil
		}
		cmd := m.open(msg.res.Thread)
		if m.thread(msg.res.Thread) == nil {
			m.upsertThread(model.Thread{ID: msg.res.Thread, Title: msg.res.Title, LastTS: time.Now().UnixMilli()})
		}
		m.mode = modeCompose
		return m, tea.Batch(cmd, m.compose.Focus())

	case searchMsg:
		return m, m.applySearch(msg)

	case retryHitMsg:
		return m, m.nextHit()

	case errMsg:
		m.errText = msg.err.Error()
		return m, nil

	case flashMsg:
		m.setFlash(string(msg))
		return m, nil

	case notificationMsg:
		cmd := m.handleNotification(rpc.Notification(msg))
		return m, tea.Batch(cmd, m.listen())

	case disconnectedMsg:
		m.connected = false
		m.errText = "lost connection to daemon"
		if msg.err != nil {
			m.errText += ": " + msg.err.Error()
		}
		return m, nil

	case tickMsg:
		m.expireTyping()
		var cmds []tea.Cmd
		// Stop announcing typing after a pause.
		if !m.typedAt.IsZero() && time.Since(m.typedAt) > 5*time.Second {
			cmds = append(cmds, m.typingCmd(m.cur, false))
			m.typedAt = time.Time{}
		}
		cmds = append(cmds, tick())
		return m, tea.Batch(cmds...)

	case editorDoneMsg:
		defer os.Remove(msg.path)
		if msg.err != nil {
			m.errText = "editor: " + msg.err.Error()
			return m, nil
		}
		b, err := os.ReadFile(msg.path)
		if err != nil {
			m.errText = err.Error()
			return m, nil
		}
		text := strings.TrimRight(string(b), "\n")
		if msg.thread == m.cur {
			m.compose.SetValue(text)
			m.mode = modeCompose
			m.layout()
			return m, m.compose.Focus()
		}
		m.drafts[msg.thread] = text
		return m, nil

	case tea.PasteMsg:
		if m.mode == modeCompose {
			var cmd tea.Cmd
			m.compose, cmd = m.compose.Update(msg)
			m.layout()
			return m, cmd
		}
		if m.mode == modePrompt {
			var cmd tea.Cmd
			m.prompt, cmd = m.prompt.Update(msg)
			return m, cmd
		}
		return m, nil

	case tea.KeyPressMsg:
		m.errText = ""
		return m.handleKey(msg)
	}

	// Cursor blink and other component messages.
	var cmds []tea.Cmd
	var cmd tea.Cmd
	m.compose, cmd = m.compose.Update(msg)
	cmds = append(cmds, cmd)
	m.prompt, cmd = m.prompt.Update(msg)
	cmds = append(cmds, cmd)
	return m, tea.Batch(cmds...)
}

func (m *Model) mergeMessages(msg messagesMsg) {
	have := map[int64]bool{}
	for _, x := range m.msgs[msg.thread] {
		have[x.ID] = true
	}
	var add []*model.Message
	for _, x := range msg.msgs {
		if !have[x.ID] {
			add = append(add, x)
		}
	}
	all := append(add, m.msgs[msg.thread]...)
	sort.SliceStable(all, func(i, j int) bool { return all[i].TS < all[j].TS })
	if msg.before != 0 && m.sel >= 0 && msg.thread == m.cur {
		m.sel += len(add) // keep the same message selected
	}
	m.msgs[msg.thread] = all
	m.loaded[msg.thread] = true
	if len(msg.msgs) < pageSize {
		m.exhausted[msg.thread] = true
	}
}

func (m *Model) upsertMessage(x *model.Message) (isNew bool) {
	list := m.msgs[x.Thread]
	for i, old := range list {
		if old.ID == x.ID {
			list[i] = x
			return false
		}
	}
	if !m.loaded[x.Thread] {
		return true // will be fetched when the thread is opened
	}
	i := sort.Search(len(list), func(i int) bool { return list[i].TS > x.TS })
	list = append(list, nil)
	copy(list[i+1:], list[i:])
	list[i] = x
	if m.sel >= i && x.Thread == m.cur {
		m.sel++
	}
	m.msgs[x.Thread] = list
	return true
}

func (m *Model) handleNotification(n rpc.Notification) tea.Cmd {
	switch n.Method {
	case rpc.EvMessage:
		x, ok := decodeInto[model.Message](n.Params)
		if !ok {
			return nil
		}
		m.upsertMessage(&x)
		if !x.Outgoing {
			delete(m.typing[x.Thread], x.Author)
		}
		if x.Thread == m.cur && !x.Outgoing && !m.blurred {
			return m.markReadCmd(x.Thread)
		}
		if !x.Outgoing {
			who := x.AuthorName
			if who == "" {
				who = "someone"
			}
			where := ""
			if t := m.thread(x.Thread); t != nil && t.Kind == model.Group {
				where = " in " + t.Title
			}
			m.setFlash(fmt.Sprintf("✉ %s%s: %s", who, where, truncate(x.Preview(), 60)))
			if m.opts.Bell {
				return tea.Raw("\a")
			}
		}
	case rpc.EvMessageUpdate:
		if x, ok := decodeInto[model.Message](n.Params); ok {
			m.upsertMessage(&x)
		}
	case rpc.EvThread:
		if t, ok := decodeInto[model.Thread](n.Params); ok {
			m.upsertThread(t)
			if m.cur == "" {
				return m.open(t.ID)
			}
		}
	case rpc.EvTyping:
		if e, ok := decodeInto[rpc.TypingEvent](n.Params); ok {
			if m.typing[e.Thread] == nil {
				m.typing[e.Thread] = map[string]typingInfo{}
			}
			if e.Typing {
				m.typing[e.Thread][e.Sender] = typingInfo{name: e.Name, until: time.Now().Add(15 * time.Second)}
			} else {
				delete(m.typing[e.Thread], e.Sender)
			}
		}
	case rpc.EvConnection:
		if st, ok := decodeInto[rpc.StatusResult](n.Params); ok {
			m.status = st
		}
	case rpc.EvContacts:
		return tea.Batch(m.loadContactsCmd(), m.loadThreadsCmd())
	}
	return nil
}

func (m *Model) expireTyping() {
	now := time.Now()
	for th, who := range m.typing {
		for id, ti := range who {
			if now.After(ti.until) {
				delete(who, id)
			}
		}
		if len(who) == 0 {
			delete(m.typing, th)
		}
	}
}

// --- keys ---

func (m *Model) handleKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := k.String()
	if s == "ctrl+c" {
		return m, m.quitCmd()
	}
	// Thread switching works everywhere except prompts.
	if m.mode != modePrompt {
		switch s {
		case "ctrl+n":
			return m, m.moveThread(1)
		case "ctrl+p":
			return m, m.moveThread(-1)
		}
	}
	switch m.mode {
	case modeHelp:
		m.mode = modeNormal
		return m, nil
	case modePrompt:
		return m.handlePromptKey(k)
	case modeCompose:
		return m.handleComposeKey(k)
	}
	return m.handleNormalKey(k)
}

func (m *Model) quitCmd() tea.Cmd {
	m.quit = true
	var cmds []tea.Cmd
	if !m.typedAt.IsZero() {
		cmds = append(cmds, m.typingCmd(m.cur, false))
	}
	return tea.Sequence(tea.Batch(cmds...), tea.Quit)
}

func (m *Model) handleNormalKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "q":
		return m, m.quitCmd()
	case "?":
		m.mode = modeHelp
	case "J":
		return m, m.moveThread(1)
	case "K":
		return m, m.moveThread(-1)
	case "j", "down":
		return m, m.moveSel(1)
	case "k", "up":
		return m, m.moveSel(-1)
	case "n", "tab":
		return m, m.nextUnread()
	case "g", "home":
		m.sel, m.scroll = 0, 0
		return m, m.loadOlder()
	case "G", "end", "esc":
		m.sel, m.scroll = -1, 0
		if k.String() == "esc" {
			delete(m.replyTo, m.cur)
		}
	case "ctrl+u", "pgup":
		return m, m.moveSel(-max(3, m.paneHeight()/6))
	case "ctrl+d", "pgdown":
		return m, m.moveSel(max(3, m.paneHeight()/6))
	case "ctrl+y":
		m.scroll++
	case "ctrl+e":
		m.scroll = max(0, m.scroll-1)
	case "enter", "i", "c":
		if m.cur == "" {
			return m, m.startPrompt(promptTo, "to: ", "")
		}
		m.mode = modeCompose
		m.layout()
		return m, m.compose.Focus()
	case "r":
		if sel := m.selected(); sel != nil && m.cur != "" {
			m.replyTo[m.cur] = sel
			m.mode = modeCompose
			m.layout()
			return m, m.compose.Focus()
		}
	case "e", "+":
		if m.selected() != nil {
			return m, m.startPrompt(promptReact, "react (1-6 or emoji, empty removes): ", "")
		}
	case "D":
		if sel := m.selected(); sel != nil && sel.Outgoing && !sel.Deleted {
			return m, m.startPrompt(promptConfirmDelete, "delete this message for everyone? (y/N) ", "")
		}
		m.setFlash("only your own messages can be deleted")
	case "o":
		return m, m.openAttachments()
	case "R":
		if sel := m.selected(); sel != nil {
			return m, m.retryCmd(sel.ID)
		}
	case "y":
		if sel := m.selected(); sel != nil {
			m.setFlash("copied message text")
			return m, tea.SetClipboard(sel.Body)
		}
	case "a":
		return m, m.startPrompt(promptAttach, "attach: ", m.defaultAttachDir())
	case "A":
		delete(m.attach, m.cur)
		m.setFlash("attachments cleared")
	case "C", "m":
		return m, m.startPrompt(promptTo, "to: ", "")
	case "/":
		return m, m.startPrompt(promptSearch, "search: ", "")
	case ":":
		return m, m.startPrompt(promptCommand, ":", "")
	}
	return m, nil
}

func (m *Model) handleComposeKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.mode = modeNormal
		m.compose.Blur()
		m.drafts[m.cur] = m.compose.Value()
		return m, nil
	case "enter":
		return m, m.sendDraft()
	case "ctrl+e":
		return m, m.openEditor()
	case "ctrl+t":
		return m, m.startPrompt(promptAttach, "attach: ", m.defaultAttachDir())
	case "ctrl+r":
		delete(m.replyTo, m.cur)
		return m, nil
	case "up":
		if m.compose.Line() == 0 && m.compose.Value() == "" {
			m.mode = modeNormal
			m.compose.Blur()
			return m, m.moveSel(-1)
		}
	}
	before := m.compose.Value()
	var cmd tea.Cmd
	m.compose, cmd = m.compose.Update(k)
	m.layout()
	var cmds = []tea.Cmd{cmd}
	if m.compose.Value() != before && m.compose.Value() != "" && time.Since(m.typedAt) > 4*time.Second {
		m.typedAt = time.Now()
		cmds = append(cmds, m.typingCmd(m.cur, true))
	}
	return m, tea.Batch(cmds...)
}

func (m *Model) sendDraft() tea.Cmd {
	body := strings.TrimSpace(m.compose.Value())
	atts := m.attach[m.cur]
	if body == "" && len(atts) == 0 {
		return nil
	}
	p := rpc.SendParams{Thread: m.cur, Body: body, Attachments: atts}
	if r := m.replyTo[m.cur]; r != nil {
		p.Quote = &model.Quote{Author: r.Author, TS: r.TS, Text: r.Body}
	}
	m.compose.Reset()
	m.drafts[m.cur] = ""
	delete(m.replyTo, m.cur)
	delete(m.attach, m.cur)
	m.typedAt = time.Time{}
	m.sel, m.scroll = -1, 0
	m.layout()
	return m.sendCmd(p)
}

func (m *Model) openEditor() tea.Cmd {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	f, err := os.CreateTemp("", "signal-headless-*.txt")
	if err != nil {
		m.errText = err.Error()
		return nil
	}
	_, _ = f.WriteString(m.compose.Value())
	f.Close()
	thread := m.cur
	parts := strings.Fields(editor)
	cmd := exec.Command(parts[0], append(parts[1:], f.Name())...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return editorDoneMsg{thread: thread, path: f.Name(), err: err}
	})
}

func (m *Model) openAttachments() tea.Cmd {
	sel := m.selected()
	if sel == nil {
		return nil
	}
	var paths []string
	for _, a := range sel.Attachments {
		if a.State == model.AttachmentDone && a.Path != "" {
			paths = append(paths, a.Path)
		}
	}
	if len(paths) == 0 {
		m.setFlash("no downloaded attachments on this message")
		return nil
	}
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	for _, p := range paths {
		c := exec.Command(opener, p)
		c.Stdout, c.Stderr = nil, nil
		if err := c.Start(); err != nil {
			m.errText = err.Error()
			return nil
		}
		go c.Wait()
	}
	m.setFlash(fmt.Sprintf("opened %d attachment(s)", len(paths)))
	return nil
}

func (m *Model) defaultAttachDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd + string(filepath.Separator)
}

// --- prompts ---

func (m *Model) startPrompt(kind promptKind, label, initial string) tea.Cmd {
	if kind != promptCommand && kind != promptTo && m.cur == "" && kind != promptSearch {
		m.setFlash("open a conversation first")
		return nil
	}
	m.promptKind = kind
	m.prompt.Prompt = label
	m.prompt.SetValue(initial)
	m.prompt.CursorEnd()
	m.completions, m.compIdx = nil, 0
	m.mode = modePrompt
	m.compose.Blur()
	return m.prompt.Focus()
}

func (m *Model) endPrompt() {
	m.prompt.Blur()
	m.completions = nil
	if m.promptKind == promptAttach && m.compose.Value() != "" {
		m.mode = modeCompose
		m.compose.Focus()
		return
	}
	m.mode = modeNormal
}

func (m *Model) handlePromptKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc", "ctrl+g":
		m.endPrompt()
		return m, nil
	case "tab", "shift+tab":
		m.complete(k.String() == "shift+tab")
		return m, nil
	case "enter":
		val := m.prompt.Value()
		kind := m.promptKind
		m.endPrompt()
		return m, m.submitPrompt(kind, val)
	}
	if m.promptKind == promptConfirmDelete {
		m.endPrompt()
		if k.String() == "y" || k.String() == "Y" {
			if sel := m.selected(); sel != nil {
				return m, m.deleteCmd(m.cur, sel.TS)
			}
		}
		return m, nil
	}
	if m.promptKind == promptReact && m.prompt.Value() == "" {
		if n := k.String(); len(n) == 1 && n[0] >= '1' && n[0] <= '6' {
			m.endPrompt()
			return m, m.react(quickReactions[n[0]-'1'])
		}
	}
	m.completions = nil
	var cmd tea.Cmd
	m.prompt, cmd = m.prompt.Update(k)
	return m, cmd
}

func (m *Model) react(emoji string) tea.Cmd {
	sel := m.selected()
	if sel == nil {
		return nil
	}
	if emoji == "" {
		for _, r := range sel.Reactions {
			if r.Reactor == m.status.Account.ACI {
				return m.reactCmd(m.cur, sel, r.Emoji, true)
			}
		}
		return nil
	}
	return m.reactCmd(m.cur, sel, emoji, false)
}

func (m *Model) submitPrompt(kind promptKind, val string) tea.Cmd {
	val = strings.TrimSpace(val)
	switch kind {
	case promptTo:
		if val == "" {
			return nil
		}
		return m.resolveCmd(val)
	case promptAttach:
		return m.addAttachment(val)
	case promptReact:
		return m.react(val)
	case promptSearch:
		if val == "" {
			val = m.search.query
		}
		if val == "" {
			return nil
		}
		if val == m.search.query && len(m.search.hits) > 0 {
			return m.nextHit()
		}
		return m.searchCmd(m.cur, val)
	case promptCommand:
		return m.runCommand(val)
	}
	return nil
}

func (m *Model) addAttachment(path string) tea.Cmd {
	path = expandHome(path)
	if path == "" {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		m.errText = err.Error()
		return nil
	}
	if st.IsDir() {
		m.errText = path + " is a directory"
		return nil
	}
	abs, _ := filepath.Abs(path)
	m.attach[m.cur] = append(m.attach[m.cur], abs)
	m.mode = modeCompose
	m.layout()
	return m.compose.Focus()
}

func expandHome(p string) string {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func (m *Model) applySearch(msg searchMsg) tea.Cmd {
	if msg.err != nil {
		m.errText = msg.err.Error()
		return nil
	}
	m.search.query = msg.query
	m.search.hits = nil
	m.search.pos = -1
	for _, x := range msg.msgs {
		m.search.hits = append(m.search.hits, x.ID)
	}
	if len(m.search.hits) == 0 {
		m.setFlash(fmt.Sprintf("no matches for %q", msg.query))
		return nil
	}
	return m.nextHit()
}

// nextHit selects the next (older) search match, loading history as needed.
func (m *Model) nextHit() tea.Cmd {
	if len(m.search.hits) == 0 {
		return nil
	}
	m.search.pos = (m.search.pos + 1) % len(m.search.hits)
	id := m.search.hits[m.search.pos]
	for i, x := range m.curMsgs() {
		if x.ID == id {
			m.sel, m.scroll = i, 0
			m.setFlash(fmt.Sprintf("match %d/%d for %q (/ enter for next)", m.search.pos+1, len(m.search.hits), m.search.query))
			return nil
		}
	}
	// Not loaded yet: fetch older pages, then retry.
	m.search.pos--
	if m.exhausted[m.cur] {
		return nil
	}
	return tea.Sequence(m.loadOlder(), func() tea.Msg { return retryHitMsg{} })
}

type retryHitMsg struct{}

var commandHelp = []string{
	"to NAME", "attach PATH", "detach", "archive", "unarchive", "archived", "read", "search TEXT", "react EMOJI",
	"retry", "open", "help", "quit",
}

func (m *Model) runCommand(line string) tea.Cmd {
	cmd, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
	arg = strings.TrimSpace(arg)
	switch cmd {
	case "":
		return nil
	case "q", "quit", "exit":
		return m.quitCmd()
	case "to", "compose", "new":
		if arg == "" {
			return m.startPrompt(promptTo, "to: ", "")
		}
		return m.resolveCmd(arg)
	case "attach", "a":
		if arg == "" {
			return m.startPrompt(promptAttach, "attach: ", m.defaultAttachDir())
		}
		return m.addAttachment(arg)
	case "detach":
		delete(m.attach, m.cur)
	case "archive":
		return m.archiveCmd(m.cur, true)
	case "unarchive":
		return m.archiveCmd(m.cur, false)
	case "archived":
		m.showArchived = !m.showArchived
		m.setFlash(fmt.Sprintf("show archived: %v", m.showArchived))
	case "read":
		return m.markReadCmd(m.cur)
	case "search", "s":
		return m.searchCmd(m.cur, arg)
	case "react":
		return m.react(arg)
	case "retry":
		if sel := m.selected(); sel != nil {
			return m.retryCmd(sel.ID)
		}
	case "open":
		return m.openAttachments()
	case "help", "h":
		m.mode = modeHelp
	default:
		m.errText = fmt.Sprintf("unknown command %q (try :help)", cmd)
	}
	return nil
}
