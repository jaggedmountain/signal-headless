// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package watch follows one conversation for new messages that didn't come
// from this computer: in Note to Self, what the user types on the phone. It
// is `signal-headless --watch`, for agents that take instructions over
// Signal. It survives daemon restarts: it resubscribes and fetches whatever
// arrived in between, using message IDs (which only grow) as the cursor.
package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"signal-headless/internal/model"
	"signal-headless/internal/rpc"
)

type Options struct {
	Thread model.ThreadID
	// Since: only messages with a larger ID; 0 means only messages that
	// arrive from now on.
	Since int64
	// Once stops after the first message.
	Once bool
	// Dial connects to the daemon (starting it if needed).
	Dial func(ctx context.Context) (*rpc.Client, error)
	// Emit receives each message, in order, with its attachments settled.
	Emit func(model.Message) error
	// Log gets reconnect notices (stderr); nil to discard.
	Log func(format string, args ...any)
	// Settle bounds the wait for a message's attachments to download
	// before it is emitted anyway (default 2 minutes).
	Settle time.Duration
}

const pageSize = 100

type pending struct {
	m        model.Message
	deadline time.Time
}

type watcher struct {
	o      Options
	lastID int64 // the last message emitted or skipped
	queue  []pending
	done   bool
}

// Run watches until ctx ends (nil for a deadline or cancel is up to the
// caller to interpret) or, with Once, the first message was emitted.
func Run(ctx context.Context, o Options) error {
	if o.Settle == 0 {
		o.Settle = 2 * time.Minute
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	w := &watcher{o: o, lastID: o.Since}
	started := o.Since > 0
	backoff := time.Second
	for {
		c, err := o.Dial(ctx)
		if err == nil {
			err = w.session(ctx, c, &started)
			c.Close()
			if w.done {
				return nil
			}
			if err == nil {
				backoff = time.Second
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		o.Log("watch: %v; reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 30*time.Second)
	}
}

// session catches up and follows events until the connection drops. The
// starting point is taken before subscribing and the catch-up runs after,
// so nothing falls between them; IDs weed out the overlap.
func (w *watcher) session(ctx context.Context, c *rpc.Client, started *bool) error {
	if !*started {
		newest, err := w.page(ctx, c, 0, 1)
		if err != nil {
			return err
		}
		if len(newest) > 0 {
			w.lastID = newest[0].ID
		}
		*started = true
	}
	if err := c.Call(ctx, rpc.MSubscribe, nil, nil); err != nil {
		return err
	}
	w.queue = nil // rebuilt from the catch-up
	missed, err := w.since(ctx, c, w.lastID)
	if err != nil {
		return err
	}
	for _, m := range missed {
		w.add(m)
	}
	if err := w.flush(); err != nil || w.done {
		return err
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if err := w.flush(); err != nil || w.done {
				return err
			}
		case n, ok := <-c.Notifications():
			if !ok {
				return c.Err()
			}
			var m model.Message
			switch n.Method {
			case rpc.EvMessage:
				if json.Unmarshal(n.Params, &m) == nil && m.Thread == w.o.Thread {
					w.add(m)
				}
			case rpc.EvMessageUpdate:
				if json.Unmarshal(n.Params, &m) == nil && m.Thread == w.o.Thread {
					w.update(m)
				}
			default:
				continue
			}
			if err := w.flush(); err != nil || w.done {
				return err
			}
		}
	}
}

// page returns up to limit messages before ts (0: the newest), newest first.
func (w *watcher) page(ctx context.Context, c *rpc.Client, before int64, limit int) ([]model.Message, error) {
	var msgs []model.Message
	if err := c.Call(ctx, rpc.MGetMessages, rpc.GetMessagesParams{Thread: w.o.Thread, Before: before, Limit: limit}, &msgs); err != nil {
		return nil, err
	}
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].TS > msgs[j].TS })
	return msgs, nil
}

// since returns the conversation's messages with an ID above id, oldest
// first, paging back until a page reaches it.
func (w *watcher) since(ctx context.Context, c *rpc.Client, id int64) ([]model.Message, error) {
	var out []model.Message
	var before int64
	for pages := 0; pages < 100; pages++ {
		msgs, err := w.page(ctx, c, before, pageSize)
		if err != nil {
			return nil, err
		}
		reached := len(msgs) < pageSize
		for _, m := range msgs {
			if m.ID > id {
				out = append(out, m)
			} else {
				reached = true
			}
		}
		if reached || len(msgs) == 0 {
			break
		}
		before = msgs[len(msgs)-1].TS
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (w *watcher) add(m model.Message) {
	if m.ID <= w.lastID {
		return
	}
	for _, p := range w.queue {
		if p.m.ID == m.ID {
			return
		}
	}
	w.queue = append(w.queue, pending{m: m, deadline: time.Now().Add(w.o.Settle)})
	sort.Slice(w.queue, func(i, j int) bool { return w.queue[i].m.ID < w.queue[j].m.ID })
}

// update refreshes a queued message (attachment progress, edits).
func (w *watcher) update(m model.Message) {
	for i := range w.queue {
		if w.queue[i].m.ID == m.ID {
			w.queue[i].m = m
		}
	}
}

// flush emits queued messages in ID order, stopping at one whose
// attachments are still downloading (until its deadline).
func (w *watcher) flush() error {
	for len(w.queue) > 0 {
		p := w.queue[0]
		if downloading(p.m) && time.Now().Before(p.deadline) {
			return nil
		}
		w.queue = w.queue[1:]
		w.lastID = p.m.ID
		if p.m.LocalOrigin || p.m.Deleted {
			continue
		}
		if err := w.o.Emit(p.m); err != nil {
			return fmt.Errorf("emit: %w", err)
		}
		if w.o.Once {
			w.done = true
			return nil
		}
	}
	return nil
}

func downloading(m model.Message) bool {
	for _, a := range m.Attachments {
		if a.State == model.AttachmentPending {
			return true
		}
	}
	return false
}
