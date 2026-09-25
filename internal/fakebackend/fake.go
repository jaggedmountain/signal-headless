// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package fakebackend is an in-memory backend.Backend for tests and for
// exercising clients without a Signal account (--daemon --fake).
package fakebackend

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"signal-headless/internal/backend"
	"signal-headless/internal/model"
)

const (
	SelfACI  = "00000000-0000-4000-8000-000000000001"
	AliceACI = "00000000-0000-4000-8000-00000000000a"
	BobACI   = "00000000-0000-4000-8000-00000000000b"
	EchoACI  = "00000000-0000-4000-8000-00000000000e"
	// DormouseACI's conversation arrives through the simulated history
	// transfer (as after linking with "Transfer message history").
	DormouseACI = "00000000-0000-4000-8000-00000000000d"
	GroupID     = "Z3JvdXBncm91cGdyb3VwZ3JvdXBncm91cGdyb3VwMDA="
)

type Sent struct {
	Kind string // "message", "reaction", "delete", "typing", "read"
	Out  model.Outgoing
	Ref  model.MessageRef
	Refs []model.MessageRef
	Text string
	// Deletes: a "delete for me" sync (Kind "deleteForMe").
	Deletes []backend.MessageDelete
}

type Fake struct {
	mu       sync.Mutex
	handler  backend.Handler
	ctx      context.Context
	ready    chan struct{}
	sent     []Sent
	contacts map[string]model.Contact
	// Echo makes the echo contact answer every message after EchoDelay.
	Echo      bool
	EchoDelay time.Duration
	// SendErr, when set, makes Send fail; DeleteErr makes DeleteForMe fail.
	SendErr   error
	DeleteErr error
	// Seed injects demo conversations on Run.
	Seed bool
}

var _ backend.Backend = (*Fake)(nil)

func New() *Fake {
	return &Fake{
		ready:     make(chan struct{}),
		EchoDelay: 300 * time.Millisecond,
		contacts: map[string]model.Contact{
			AliceACI:    {ID: AliceACI, Number: "+15550000001", Name: "Alice Liddell"},
			BobACI:      {ID: BobACI, Number: "+15550000002", Profile: "Bob"},
			EchoACI:     {ID: EchoACI, Number: "+15550000003", Name: "Echo Bot"},
			DormouseACI: {ID: DormouseACI, Number: "+15550000004", Name: "Dormouse"},
		},
	}
}

func (f *Fake) Account() model.Account {
	return model.Account{ACI: SelfACI, Number: "+15550000000", DeviceID: 2}
}

// Ready is closed once Run has registered its handler.
func (f *Fake) Ready() <-chan struct{} { return f.ready }

func (f *Fake) Run(ctx context.Context, h backend.Handler) error {
	f.mu.Lock()
	f.handler, f.ctx = h, ctx
	f.mu.Unlock()
	close(f.ready)
	_ = h(ctx, backend.ConnectionEvent{State: model.ConnConnected})
	if f.Seed {
		f.seed()
		go f.history(ctx)
	}
	_ = h(ctx, backend.QueueEmptyEvent{})
	<-ctx.Done()
	return nil
}

// Inject delivers an event as if it came from Signal.
func (f *Fake) Inject(e backend.Event) error {
	f.mu.Lock()
	h, ctx := f.handler, f.ctx
	f.mu.Unlock()
	if h == nil {
		return errors.New("fake backend not running")
	}
	return h(ctx, e)
}

func (f *Fake) Sent() []Sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Sent(nil), f.sent...)
}

func (f *Fake) record(s Sent) {
	f.mu.Lock()
	f.sent = append(f.sent, s)
	f.mu.Unlock()
}

func (f *Fake) Send(ctx context.Context, out model.Outgoing) error {
	f.mu.Lock()
	err := f.SendErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	for _, p := range out.Attachments {
		if _, err := os.Stat(p); err != nil {
			return err
		}
	}
	f.record(Sent{Kind: "message", Out: out})
	if out.Thread == EchoACI && f.Echo {
		go func() {
			time.Sleep(f.EchoDelay)
			_ = f.Inject(backend.ReceiptEvent{From: EchoACI, Kind: backend.ReceiptDelivered, Timestamps: []int64{out.TS}})
			_ = f.Inject(backend.TypingEvent{Thread: EchoACI, Sender: EchoACI, Typing: true})
			time.Sleep(f.EchoDelay)
			_ = f.Inject(backend.ReceiptEvent{From: EchoACI, Kind: backend.ReceiptRead, Timestamps: []int64{out.TS}})
			body := "echo: " + out.Body
			if len(out.Attachments) > 0 {
				body += fmt.Sprintf(" (+%d attachments)", len(out.Attachments))
			}
			_ = f.Inject(backend.MessageEvent{Message: model.Message{
				Thread: EchoACI, Author: EchoACI, TS: time.Now().UnixMilli(), Body: body,
				Quote: &model.Quote{Author: SelfACI, TS: out.TS, Text: out.Body},
			}})
		}()
	}
	return nil
}

func (f *Fake) SendReaction(ctx context.Context, thread model.ThreadID, target model.MessageRef, emoji string, remove bool) error {
	f.record(Sent{Kind: "reaction", Out: model.Outgoing{Thread: thread}, Ref: target, Text: emoji})
	return nil
}

func (f *Fake) SendDelete(ctx context.Context, thread model.ThreadID, targetTS int64) error {
	f.record(Sent{Kind: "delete", Out: model.Outgoing{Thread: thread}, Ref: model.MessageRef{Author: SelfACI, TS: targetTS}})
	return nil
}

func (f *Fake) SendTyping(ctx context.Context, thread model.ThreadID, typing bool) error {
	f.record(Sent{Kind: "typing", Out: model.Outgoing{Thread: thread}})
	return nil
}

func (f *Fake) MarkRead(ctx context.Context, thread model.ThreadID, refs []model.MessageRef) error {
	f.record(Sent{Kind: "read", Out: model.Outgoing{Thread: thread}, Refs: refs})
	return nil
}

func (f *Fake) Contacts(ctx context.Context) ([]model.Contact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.Contact, 0, len(f.contacts))
	for _, c := range f.contacts {
		out = append(out, c)
	}
	return out, nil
}

func (f *Fake) Groups(ctx context.Context) ([]model.GroupInfo, error) {
	return []model.GroupInfo{{ID: GroupID, Title: "Tea Party", Members: []string{SelfACI, AliceACI, BobACI}}}, nil
}

func (f *Fake) ThreadInfo(ctx context.Context, thread model.ThreadID) (model.ThreadKind, string, error) {
	switch string(thread) {
	case GroupID:
		return model.Group, "Tea Party", nil
	case SelfACI:
		return model.Direct, "Note to Self", nil
	}
	if c, ok := f.Contact(ctx, string(thread)); ok {
		return model.Direct, c.DisplayName(), nil
	}
	return model.Direct, "", nil
}

func (f *Fake) Contact(ctx context.Context, id string) (model.Contact, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == SelfACI {
		return model.Contact{ID: SelfACI, Number: "+15550000000", Name: "Me"}, true
	}
	c, ok := f.contacts[id]
	return c, ok
}

func (f *Fake) ResolveRecipient(ctx context.Context, s string) (model.ThreadID, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", errors.New("empty recipient")
	case strings.EqualFold(s, "self") || s == "+15550000000" || s == SelfACI:
		return SelfACI, nil
	case s == GroupID:
		return GroupID, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.contacts {
		if s == c.ID || s == c.Number || strings.EqualFold(s, c.DisplayName()) {
			return model.ThreadID(c.ID), nil
		}
	}
	return "", fmt.Errorf("unknown recipient %q", s)
}

func (f *Fake) Unlink(ctx context.Context) error {
	f.record(Sent{Kind: "unlink"})
	return nil
}

// DownloadAttachment writes the pointer bytes as the file content; a pointer
// of "fail" simulates a download error.
func (f *Fake) DownloadAttachment(ctx context.Context, pointer []byte, dest string) error {
	if string(pointer) == "fail" {
		return errors.New("simulated download failure")
	}
	return os.WriteFile(dest, pointer, 0o600)
}

func (f *Fake) seed() {
	now := time.Now().Add(-2 * time.Hour).UnixMilli()
	msgs := []model.Message{
		{Thread: AliceACI, Author: AliceACI, TS: now, Body: "Would you like some tea?"},
		{Thread: AliceACI, Author: SelfACI, TS: now + 60_000, Body: "I don't see any tea.", Outgoing: true, Status: model.StatusRead},
		{Thread: AliceACI, Author: AliceACI, TS: now + 120_000, Body: "There isn't any. Here's a picture of some.",
			Attachments: []model.Attachment{{ContentType: "image/png", Filename: "tea.png", Pointer: []byte("PNG…fake image bytes")}}},
		{Thread: GroupID, Author: BobACI, TS: now + 180_000, Body: "No room! No room!"},
		{Thread: GroupID, Author: AliceACI, TS: now + 240_000, Body: "There's plenty of room!"},
		{Thread: SelfACI, Author: SelfACI, TS: now + 300_000, Body: "remember: buy more tea", Outgoing: true, Status: model.StatusSent},
		{Thread: EchoACI, Author: EchoACI, TS: now + 360_000, Body: "Send me anything and I'll echo it back."},
	}
	for _, m := range msgs {
		_ = f.Inject(backend.MessageEvent{Message: m})
	}
	_ = f.Inject(backend.ReactionEvent{Thread: GroupID, Reactor: AliceACI, Target: model.MessageRef{Author: BobACI, TS: now + 180_000}, Emoji: "😤", TS: now + 250_000})
}

// history plays a message-history transfer: the phone "uploads" for a
// moment, then one old conversation is imported.
func (f *Fake) history(ctx context.Context) {
	step := func(d time.Duration, s model.HistoryStatus) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
		}
		_ = f.Inject(backend.HistoryStatusEvent{Status: s})
		return true
	}
	if !step(0, model.HistoryStatus{State: "waiting"}) ||
		!step(1500*time.Millisecond, model.HistoryStatus{State: "downloading"}) ||
		!step(500*time.Millisecond, model.HistoryStatus{State: "importing"}) {
		return
	}
	day := int64(24 * time.Hour / time.Millisecond)
	old := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	msgs := []model.Message{
		{Thread: DormouseACI, Author: DormouseACI, TS: old, Body: "Twinkle, twinkle, little bat!", Read: true},
		{Thread: DormouseACI, Author: SelfACI, TS: old + 60_000, Body: "How I wonder what you're at!", Outgoing: true, Status: model.StatusRead},
		{Thread: DormouseACI, Author: DormouseACI, TS: old + day, Body: "Up above the world you fly", Read: true,
			Reactions: []model.Reaction{{Reactor: SelfACI, Emoji: "😂", TS: old + day + 1}}},
	}
	if err := f.Inject(backend.HistoryEvent{Thread: DormouseACI, Kind: model.Direct, Messages: msgs}); err != nil {
		return
	}
	step(0, model.HistoryStatus{State: "done", Chats: 1, Messages: len(msgs)})
}

func (f *Fake) LinkPreviewsEnabled() bool { return true }

// LinkPreview returns canned previews without touching the network: a title
// naming the host and a small generated image saved in dir.
func LinkPreview(dir string) func(ctx context.Context, rawURL string) (*model.OutgoingPreview, error) {
	return func(ctx context.Context, rawURL string) (*model.OutgoingPreview, error) {
		u, err := url.Parse(rawURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, errors.New("only https links get previews")
		}
		if strings.Contains(u.Path, "nopreview") {
			return nil, errors.New("no preview for this page")
		}
		img := image.NewRGBA(image.Rect(0, 0, 120, 63))
		for y := 0; y < 63; y++ {
			for x := 0; x < 120; x++ {
				img.Set(x, y, color.RGBA{uint8(40 + x), uint8(90 + y), 160, 255})
			}
		}
		path := filepath.Join(dir, "preview-fake.png")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, err
		}
		err = png.Encode(f, img)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
		return &model.OutgoingPreview{URL: rawURL, Title: "Preview of " + u.Host, Description: "A canned preview (fake backend: no network).", Image: path}, nil
	}
}

func (f *Fake) DeleteForMe(ctx context.Context, deletes []backend.MessageDelete) error {
	f.mu.Lock()
	err := f.DeleteErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	f.record(Sent{Kind: "deleteForMe", Deletes: append([]backend.MessageDelete(nil), deletes...)})
	return nil
}
