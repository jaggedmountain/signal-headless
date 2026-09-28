// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"signal-headless/internal/model"
	"signal-headless/internal/paths"
	"signal-headless/internal/rpc"
	"signal-headless/internal/watch"
)

var errWatchTimeout = errors.New("no message before the timeout")

// runWatch is --watch CHANNEL: new messages there that didn't come from this
// computer, one per line (--json: the message as JSON), until interrupted,
// --once, or --timeout.
func runWatch(ctx context.Context, o *options, p paths.Paths) error {
	c, err := connect(ctx, o, p, true)
	if err != nil {
		return err
	}
	var r rpc.ResolveResult
	err = c.Call(ctx, rpc.MResolve, rpc.ResolveParams{Recipient: o.watch}, &r)
	c.Close()
	if err != nil {
		return err
	}
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
		defer cancel()
	}
	enc := json.NewEncoder(os.Stdout)
	err = watch.Run(ctx, watch.Options{
		Thread: r.Thread,
		Since:  o.since,
		Once:   o.once,
		Dial:   func(ctx context.Context) (*rpc.Client, error) { return connect(ctx, o, p, true) },
		Log:    func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
		Emit: func(m model.Message) error {
			if o.json {
				return enc.Encode(m)
			}
			_, err := fmt.Println(watchLine(m))
			return err
		},
	})
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return errWatchTimeout
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		return nil // interrupted
	}
	return err
}

// watchLine is the human form: "15:04 Name: text", continuation lines and
// attachments indented.
func watchLine(m model.Message) string {
	var b strings.Builder
	name := m.AuthorName
	if name == "" {
		name = m.Author
	}
	fmt.Fprintf(&b, "%s %s: %s", time.UnixMilli(m.TS).Format("15:04"), name, strings.ReplaceAll(m.Body, "\n", "\n  "))
	for _, a := range m.Files() {
		where := a.Path
		if a.State != model.AttachmentDone {
			where = fmt.Sprintf("%s (%s)", a.Filename, a.State)
		}
		fmt.Fprintf(&b, "\n  [attachment] %s", where)
	}
	return b.String()
}
