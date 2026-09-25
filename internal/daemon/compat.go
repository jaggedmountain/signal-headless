// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"signal-headless/internal/backend"
	"signal-headless/internal/history"
	"signal-headless/internal/model"
	"signal-headless/internal/rpc"
)

// stringOrList accepts "x" or ["x", ...] (signal-cli allows both).
type stringOrList []string

func (s *stringOrList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*s = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*s = many
	return nil
}

// compatParams is the union of native and signal-cli parameters.
type compatParams struct {
	// native
	Thread      model.ThreadID          `json:"thread"`
	To          string                  `json:"to"`
	Body        string                  `json:"body"`
	Attachments stringOrList            `json:"attachments"`
	Quote       *model.Quote            `json:"quote"`
	Previews    []model.OutgoingPreview `json:"previews"`
	Target      *model.MessageRef       `json:"target"`
	TargetTS    int64                   `json:"targetTs"`
	Emoji       string                  `json:"emoji"`
	Remove      bool                    `json:"remove"`

	// signal-cli
	Recipient       stringOrList `json:"recipient"`
	GroupID         string       `json:"groupId"`
	NoteToSelf      bool         `json:"noteToSelf"`
	Message         string       `json:"message"`
	Attachment      stringOrList `json:"attachment"`
	QuoteTimestamp  int64        `json:"quoteTimestamp"`
	QuoteAuthor     string       `json:"quoteAuthor"`
	QuoteMessage    string       `json:"quoteMessage"`
	TargetAuthor    string       `json:"targetAuthor"`
	TargetTimestamp int64        `json:"targetTimestamp"`
}

// threads resolves the destination(s) named by p.
func (d *Daemon) threads(ctx context.Context, p *compatParams) ([]model.ThreadID, error) {
	var names []string
	switch {
	case p.Thread != "":
		return []model.ThreadID{p.Thread}, nil
	case p.GroupID != "":
		names = []string{p.GroupID}
	case p.To != "":
		names = []string{p.To}
	case p.NoteToSelf:
		names = []string{"self"}
	default:
		names = p.Recipient
	}
	if len(names) == 0 {
		return nil, rpc.Errorf(rpc.CodeInvalidParams, "no recipient (thread, to, recipient or groupId)")
	}
	out := make([]model.ThreadID, 0, len(names))
	for _, n := range names {
		id, err := d.resolve(ctx, n)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

// authorID turns a number or UUID into an ACI string.
func (d *Daemon) authorID(ctx context.Context, s string) (string, error) {
	if s == "" {
		return "", rpc.Errorf(rpc.CodeInvalidParams, "missing author")
	}
	id, err := d.resolve(ctx, s)
	if err != nil {
		return "", err
	}
	return string(id), nil
}

func (d *Daemon) rpcSend(ctx context.Context, params json.RawMessage) (any, error) {
	var p compatParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	threads, err := d.threads(ctx, &p)
	if err != nil {
		return nil, err
	}
	body := p.Body
	if body == "" {
		body = p.Message
	}
	atts := append([]string{}, p.Attachments...)
	atts = append(atts, p.Attachment...)
	quote := p.Quote
	if quote == nil && p.QuoteTimestamp != 0 {
		author, err := d.authorID(ctx, p.QuoteAuthor)
		if err != nil {
			return nil, err
		}
		quote = &model.Quote{Author: author, TS: p.QuoteTimestamp, Text: p.QuoteMessage}
	}
	if quote != nil && quote.Text == "" {
		if q, err := d.hist.MessageByRef(ctx, "", model.MessageRef{Author: quote.Author, TS: quote.TS}); err == nil {
			quote.Text = q.Body
		}
	}
	var res struct {
		rpc.SendResult
		Results []map[string]any `json:"results"`
	}
	for _, t := range threads {
		m, err := d.Send(ctx, model.Outgoing{Thread: t, Body: body, Attachments: append([]string{}, atts...), Quote: quote, Previews: p.Previews})
		if m != nil && res.Message == nil {
			res.Message = m
			res.Timestamp = m.TS
		}
		kind := "SUCCESS"
		if err != nil {
			if len(threads) == 1 {
				return nil, err
			}
			kind = "FAILURE"
		}
		res.Results = append(res.Results, map[string]any{"recipientAddress": d.address(ctx, string(t)), "type": kind})
	}
	return res, nil
}

func (d *Daemon) rpcReaction(ctx context.Context, params json.RawMessage) (any, error) {
	var p compatParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	threads, err := d.threads(ctx, &p)
	if err != nil {
		return nil, err
	}
	target := p.Target
	if target == nil {
		author, err := d.authorID(ctx, p.TargetAuthor)
		if err != nil {
			return nil, err
		}
		target = &model.MessageRef{Author: author, TS: p.TargetTimestamp}
	}
	if p.Emoji == "" {
		return nil, rpc.Errorf(rpc.CodeInvalidParams, "missing emoji")
	}
	for _, t := range threads {
		if err := d.be.SendReaction(ctx, t, *target, p.Emoji, p.Remove); err != nil {
			return nil, err
		}
		m, err := d.hist.ApplyReaction(ctx, t, *target, d.acct.ACI, p.Emoji, p.Remove, d.nextTS())
		if err == nil {
			d.broadcast(rpc.EvMessageUpdate, d.decorate(ctx, m))
		} else if !errors.Is(err, history.ErrNotFound) {
			return nil, err
		}
	}
	return map[string]any{"timestamp": d.nextTS()}, nil
}

func (d *Daemon) rpcDelete(ctx context.Context, params json.RawMessage) (any, error) {
	var p compatParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	threads, err := d.threads(ctx, &p)
	if err != nil {
		return nil, err
	}
	ts := p.TargetTS
	if ts == 0 {
		ts = p.TargetTimestamp
	}
	if ts == 0 {
		return nil, rpc.Errorf(rpc.CodeInvalidParams, "missing target timestamp")
	}
	for _, t := range threads {
		if err := d.be.SendDelete(ctx, t, ts); err != nil {
			return nil, err
		}
		if err := d.deleteMessage(ctx, t, d.acct.ACI, ts); err != nil {
			return nil, err
		}
	}
	return map[string]any{"timestamp": d.nextTS()}, nil
}

func contentTypeFor(path string) string {
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	return ct
}

// --- signal-cli envelopes ---

func isGroup(id model.ThreadID) bool {
	_, err := uuid.Parse(strings.TrimPrefix(string(id), "PNI:"))
	return err != nil
}

func (d *Daemon) number(ctx context.Context, id string) string {
	if id == d.acct.ACI {
		return d.acct.Number
	}
	if c, ok := d.be.Contact(ctx, id); ok {
		return c.Number
	}
	return ""
}

// address renders a recipient like signal-cli's RecipientAddress.
func (d *Daemon) address(ctx context.Context, id string) map[string]any {
	a := map[string]any{"uuid": id}
	if n := d.number(ctx, id); n != "" {
		a["number"] = n
	}
	return a
}

func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (d *Daemon) envelopeBase(ctx context.Context, author string, ts int64) map[string]any {
	num := d.number(ctx, author)
	source := num
	if source == "" {
		source = author
	}
	return map[string]any{
		"source":       source,
		"sourceNumber": orNil(num),
		"sourceUuid":   author,
		"sourceName":   d.name(ctx, author),
		"sourceDevice": 1,
		"timestamp":    ts,
	}
}

func (d *Daemon) compatEnvelope(ctx context.Context, m *model.Message) map[string]any {
	env := d.envelopeBase(ctx, m.Author, m.TS)
	if m.ServerTS != 0 {
		env["serverReceivedTimestamp"] = m.ServerTS
	}
	msg := map[string]any{
		"timestamp":        m.TS,
		"message":          orNil(m.Body),
		"expiresInSeconds": m.ExpiresIn,
		"viewOnce":         false,
	}
	if files := m.Files(); len(files) > 0 {
		var atts []map[string]any
		for _, a := range files {
			id := ""
			if a.State == model.AttachmentDone && a.Path != "" {
				id = filepath.Base(a.Path)
			}
			atts = append(atts, map[string]any{
				"contentType": a.ContentType, "filename": orNil(a.Filename), "id": id,
				"size": a.Size, "voiceNote": a.VoiceNote,
			})
		}
		msg["attachments"] = atts
	}
	if isGroup(m.Thread) {
		msg["groupInfo"] = map[string]any{"groupId": string(m.Thread), "type": "DELIVER"}
	}
	if q := m.Quote; q != nil {
		msg["quote"] = map[string]any{
			"id": q.TS, "author": orNil(d.number(ctx, q.Author)), "authorNumber": orNil(d.number(ctx, q.Author)),
			"authorUuid": q.Author, "text": q.Text,
		}
	}
	if !m.Outgoing {
		env["dataMessage"] = msg
		return env
	}
	if !isGroup(m.Thread) {
		dest := string(m.Thread)
		num := d.number(ctx, dest)
		msg["destination"] = orNil(num)
		if num == "" {
			msg["destination"] = dest
		}
		msg["destinationNumber"] = orNil(num)
		msg["destinationUuid"] = dest
	}
	env["syncMessage"] = map[string]any{"sentMessage": msg}
	return env
}

func (d *Daemon) compatReaction(ctx context.Context, e backend.ReactionEvent) map[string]any {
	env := d.envelopeBase(ctx, e.Reactor, e.TS)
	msg := map[string]any{
		"timestamp": e.TS,
		"reaction": map[string]any{
			"emoji": e.Emoji, "targetAuthor": orNil(d.number(ctx, e.Target.Author)),
			"targetAuthorNumber": orNil(d.number(ctx, e.Target.Author)), "targetAuthorUuid": e.Target.Author,
			"targetSentTimestamp": e.Target.TS, "isRemove": e.Remove,
		},
	}
	if isGroup(e.Thread) {
		msg["groupInfo"] = map[string]any{"groupId": string(e.Thread), "type": "DELIVER"}
	}
	if e.Reactor == d.acct.ACI {
		env["syncMessage"] = map[string]any{"sentMessage": msg}
	} else {
		env["dataMessage"] = msg
	}
	return env
}

func (d *Daemon) compatReceipt(ctx context.Context, e backend.ReceiptEvent) map[string]any {
	var when int64
	if len(e.Timestamps) > 0 {
		when = e.Timestamps[0]
	}
	env := d.envelopeBase(ctx, e.From, when)
	env["receiptMessage"] = map[string]any{
		"when": when, "isDelivery": e.Kind == backend.ReceiptDelivered,
		"isRead": e.Kind == backend.ReceiptRead, "isViewed": e.Kind == backend.ReceiptViewed,
		"timestamps": e.Timestamps,
	}
	return env
}
