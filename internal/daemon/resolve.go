package daemon

import (
	"context"
	"sort"
	"strings"

	"signal-headless/internal/model"
	"signal-headless/internal/rpc"
)

// resolve turns user input into a thread: whatever the backend accepts
// (+E164, UUID, group ID, 'self'), else a contact, group or thread name.
// Exact (case-insensitive) name matches win over prefix, then substring.
func (d *Daemon) resolve(ctx context.Context, s string) (model.ThreadID, error) {
	id, beErr := d.be.ResolveRecipient(ctx, s)
	if beErr == nil {
		return id, nil
	}
	q := strings.ToLower(strings.TrimSpace(s))
	if q == "" || strings.HasPrefix(q, "+") {
		return "", rpc.Errorf(rpc.CodeInvalidParams, "%v", beErr)
	}
	type cand struct {
		id    model.ThreadID
		label string
	}
	var cands []cand
	seen := map[model.ThreadID]bool{}
	add := func(id model.ThreadID, labels ...string) {
		for _, l := range labels {
			if l != "" && !seen[id] {
				cands = append(cands, cand{id, l})
			}
		}
	}
	if ts, err := d.hist.Threads(ctx); err == nil {
		for i := range ts {
			d.fillThread(ctx, &ts[i])
			add(ts[i].ID, ts[i].Title)
		}
	}
	if cs, err := d.be.Contacts(ctx); err == nil {
		for _, c := range cs {
			if !c.Blocked {
				add(model.ThreadID(c.ID), c.Nickname, c.Name, c.Profile)
			}
		}
	}
	if gs, err := d.be.Groups(ctx); err == nil {
		for _, g := range gs {
			add(g.ID, g.Title)
		}
	}
	for _, match := range []func(string) bool{
		func(l string) bool { return l == q },
		func(l string) bool { return strings.HasPrefix(l, q) },
		func(l string) bool { return strings.Contains(l, q) },
	} {
		hits := map[model.ThreadID]string{}
		for _, c := range cands {
			if match(strings.ToLower(c.label)) {
				hits[c.id] = c.label
			}
		}
		switch len(hits) {
		case 0:
			continue
		case 1:
			for id := range hits {
				return id, nil
			}
		default:
			var names []string
			for _, l := range hits {
				names = append(names, l)
			}
			sort.Strings(names)
			if len(names) > 8 {
				names = append(names[:8], "…")
			}
			return "", rpc.Errorf(rpc.CodeInvalidParams, "%q is ambiguous: %s", s, strings.Join(names, ", "))
		}
	}
	return "", rpc.Errorf(rpc.CodeInvalidParams, "no contact, group or number matches %q", s)
}
