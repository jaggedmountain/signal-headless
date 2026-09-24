package signalbackend

import (
	"sort"
	"unicode/utf16"

	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/signalpb"
)

// renderBody replaces mention placeholders with "@Name". Signal body range
// offsets count UTF-16 code units; styles (bold etc.) are dropped.
func renderBody(body string, ranges []*signalpb.BodyRange, name func(aci string) string) string {
	type mention struct {
		start, end int
		aci        string
	}
	var ms []mention
	for _, r := range ranges {
		if r.GetMentionAci() == "" && len(r.GetMentionAciBinary()) == 0 {
			continue
		}
		aci, err := signalmeow.ParseStringOrBinaryUUID(r.GetMentionAci(), r.GetMentionAciBinary())
		if err != nil {
			continue
		}
		ms = append(ms, mention{int(r.GetStart()), int(r.GetStart() + r.GetLength()), aci.String()})
	}
	if len(ms) == 0 {
		return body
	}
	u := utf16.Encode([]rune(body))
	sort.Slice(ms, func(i, j int) bool { return ms[i].start > ms[j].start })
	for _, m := range ms {
		if m.start < 0 || m.end > len(u) || m.start > m.end {
			continue
		}
		repl := utf16.Encode([]rune("@" + name(m.aci)))
		u = append(u[:m.start], append(repl, u[m.end:]...)...)
	}
	return string(utf16.Decode(u))
}
