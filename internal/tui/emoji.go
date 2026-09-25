// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"regexp"
	"strings"
	"sync"

	"github.com/kyokomi/emoji/v2"
)

var (
	shortcodeRE   = regexp.MustCompile(`:[a-zA-Z0-9_+\-]+:`)
	shortcodeOnce sync.Once
	shortcodes    map[string]string
)

func isAlnum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// emojize replaces known :shortcodes: (e.g. :joy: → 😂). A code must not be
// glued to letters or digits on either side, so "a:b:c" and "12:30:45" stay
// as typed; unknown codes are left alone too.
func emojize(s string) string {
	if !strings.Contains(s, ":") {
		return s
	}
	shortcodeOnce.Do(func() { shortcodes = emoji.CodeMap() })
	var b strings.Builder
	last := 0
	for pos := 0; pos < len(s); {
		loc := shortcodeRE.FindStringIndex(s[pos:])
		if loc == nil {
			break
		}
		start, end := pos+loc[0], pos+loc[1]
		e, known := shortcodes[strings.ToLower(s[start:end])]
		if known && (start == 0 || !isAlnum(s[start-1])) && (end == len(s) || !isAlnum(s[end])) {
			b.WriteString(s[last:start])
			b.WriteString(e)
			last, pos = end, end
			continue
		}
		pos = start + 1 // the closing colon may open the next code
	}
	b.WriteString(s[last:])
	return b.String()
}
