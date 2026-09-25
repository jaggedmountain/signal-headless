// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// complete handles tab in a prompt: the first press computes candidates and
// fills the longest common prefix; further presses cycle through them.
func (m *Model) complete(backward bool) {
	if len(m.completions) > 0 {
		n := len(m.completions)
		if backward {
			m.compIdx = (m.compIdx - 1 + n) % n
		} else {
			m.compIdx = (m.compIdx + 1) % n
		}
		m.prompt.SetValue(m.completions[m.compIdx])
		m.prompt.CursorEnd()
		return
	}
	val := m.prompt.Value()
	var cands []string
	switch m.promptKind {
	case promptAttach:
		cands = completePath(val)
	case promptTo:
		cands = m.completeRecipient(val)
	case promptCommand:
		cmd, arg, hasArg := strings.Cut(val, " ")
		if !hasArg {
			for _, c := range commandHelp {
				name, _, _ := strings.Cut(c, " ")
				if strings.HasPrefix(name, cmd) {
					cands = append(cands, name+" ")
				}
			}
		} else if cmd == "attach" || cmd == "a" {
			for _, p := range completePath(arg) {
				cands = append(cands, cmd+" "+p)
			}
		} else if cmd == "to" {
			for _, r := range m.completeRecipient(arg) {
				cands = append(cands, "to "+r)
			}
		}
	default:
		return
	}
	switch len(cands) {
	case 0:
		m.setFlash("no completions")
		return
	case 1:
		m.prompt.SetValue(cands[0])
		m.prompt.CursorEnd()
		return
	}
	if p := commonPrefix(cands); len(p) > len(val) {
		m.prompt.SetValue(p)
		m.prompt.CursorEnd()
	}
	m.completions = cands
	m.compIdx = -1
}

func commonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	p := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, p) {
			p = p[:len(p)-1]
		}
	}
	return p
}

// completePath lists entries matching the partial path; directories get a
// trailing slash. Hidden files are offered only when the prefix starts with a dot.
func completePath(partial string) []string {
	expanded := expandHome(partial)
	dir, base := filepath.Split(expanded)
	readDir := dir
	if readDir == "" {
		readDir = "."
	}
	entries, err := os.ReadDir(readDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base) || (strings.HasPrefix(name, ".") && !strings.HasPrefix(base, ".")) {
			continue
		}
		full := dir + name
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			if st, err := os.Stat(full); err == nil && st.IsDir() {
				isDir = true
			}
		}
		if isDir {
			full += "/"
		}
		// Keep a "~/" prefix if the user typed one.
		if strings.HasPrefix(partial, "~/") {
			if h, err := os.UserHomeDir(); err == nil && strings.HasPrefix(full, h+"/") {
				full = "~/" + strings.TrimPrefix(full, h+"/")
			}
		}
		out = append(out, full)
	}
	sort.Strings(out)
	return out
}

// completeRecipient offers thread titles, contact names and group titles
// that contain the typed text (case-insensitive), prefix matches first.
func (m *Model) completeRecipient(partial string) []string {
	q := strings.ToLower(strings.TrimSpace(partial))
	seen := map[string]bool{}
	var prefix, contains []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		l := strings.ToLower(name)
		switch {
		case strings.HasPrefix(l, q):
			prefix = append(prefix, name)
		case strings.Contains(l, q):
			contains = append(contains, name)
		default:
			return
		}
		seen[name] = true
	}
	for _, t := range m.threads {
		add(t.Title)
	}
	for _, c := range m.contacts {
		if !c.Blocked {
			add(c.DisplayName())
		}
	}
	for _, g := range m.groups {
		add(g.Title)
	}
	sort.Strings(prefix)
	sort.Strings(contains)
	out := append(prefix, contains...)
	if len(out) > 50 {
		out = out[:50]
	}
	return out
}
