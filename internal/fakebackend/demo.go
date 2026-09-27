// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package fakebackend

import (
	"fmt"
	"time"

	"signal-headless/internal/backend"
	"signal-headless/internal/model"
)

// The demo cast: our account is Ender's. For screenshots
// (SIGNAL_HEADLESS_DEMO=1 with --fake); the dialogue is original.
const (
	BeanACI      = "00000000-0000-4000-8000-000000000b00"
	PetraACI     = "00000000-0000-4000-8000-000000000b01"
	AlaiACI      = "00000000-0000-4000-8000-000000000b02"
	ValentineACI = "00000000-0000-4000-8000-000000000b03"
	PeterACI     = "00000000-0000-4000-8000-000000000b04"
	GraffACI     = "00000000-0000-4000-8000-000000000b05"
	MazerACI     = "00000000-0000-4000-8000-000000000b06"
	CrazyTomACI  = "00000000-0000-4000-8000-000000000b07"
	HotSoupACI   = "00000000-0000-4000-8000-000000000b08"
	FlyMoloACI   = "00000000-0000-4000-8000-000000000b09"
)

// NewDemo is a fake with the demo cast and conversations instead of the
// test seed; no echo contact, no history transfer.
func NewDemo() *Fake {
	f := New()
	f.demo = true
	f.groupTitle = "Dragon Army"
	f.groupMembers = []string{SelfACI, BeanACI, AlaiACI, CrazyTomACI, HotSoupACI, FlyMoloACI}
	f.contacts = map[string]model.Contact{}
	for i, c := range []struct{ id, name string }{
		{BeanACI, "Bean"}, {PetraACI, "Petra Arkanian"}, {AlaiACI, "Alai"},
		{ValentineACI, "Valentine Wiggin"}, {PeterACI, "Peter Wiggin"},
		{GraffACI, "Colonel Graff"}, {MazerACI, "Mazer Rackham"},
		{CrazyTomACI, "Crazy Tom"}, {HotSoupACI, "Hot Soup"}, {FlyMoloACI, "Fly Molo"},
	} {
		f.contacts[c.id] = model.Contact{ID: c.id, Number: fmt.Sprintf("+1555010%02d", i), Name: c.name}
	}
	return f
}

func (f *Fake) seedDemo() {
	const min = int64(time.Minute / time.Millisecond)
	day := 24 * 60 * min
	now := time.Now().UnixMilli()
	today := now - 62*min // Bean's thread this morning: the newest, so it opens first
	y, mo, dd := time.Now().Date()
	yday := time.Date(y, mo, dd-1, 21, 40, 0, 0, time.Local).UnixMilli() // last evening

	in := func(thread, author string, ts int64, body string) model.Message {
		return model.Message{Thread: model.ThreadID(thread), Author: author, TS: ts, Body: body, Read: true}
	}
	out := func(thread string, ts int64, body string) model.Message {
		return model.Message{Thread: model.ThreadID(thread), Author: SelfACI, TS: ts, Body: body, Outgoing: true, Status: model.StatusRead}
	}
	unread := func(m model.Message) model.Message { m.Read = false; return m }

	formation := in(BeanACI, BeanACI, today, "Couldn't sleep. Sketched something for the star field.")
	formation.Attachments = []model.Attachment{{ContentType: "image/png", Filename: "formation.png", Pointer: []byte("PNG…demo image")}}
	reply := out(BeanACI, today+8*min, "This is good. Too good for a first try.")
	reply.Quote = &model.Quote{Author: BeanACI, TS: today, Text: formation.Body}
	always := in(BeanACI, BeanACI, today+11*min, "Every time. Feet toward their gate before the lights even come up.")
	standings := in(BeanACI, BeanACI, today+28*min, "Standings are up: https://battleschool.example/standings")
	standings.Attachments = []model.Attachment{{ContentType: "image/png", Kind: model.AttachmentPreview, Pointer: []byte("PNG…demo preview")}}
	standings.Previews = []model.LinkPreview{{URL: "https://battleschool.example/standings", Title: "Army standings", Description: "Dragon Army: 7 wins, 0 losses", Image: 0}}

	msgs := []model.Message{
		// Older conversations, oldest first.
		in(PeterACI, PeterACI, now-6*day, "Heard you're winning up there. Don't let it go to your head."),
		out(PeterACI, now-6*day+30*min, "Noted."),
		in(MazerACI, MazerACI, now-3*day, "Again."),
		in(AlaiACI, AlaiACI, now-2*day, "Salaam, Ender."),
		out(AlaiACI, now-2*day+5*min, "Salaam."),
		out(SelfACI, yday-60*min, "ask Bean about the star formation"),
		in(ValentineACI, ValentineACI, yday-30*min, "Write when you can. The lake is still there, and so am I."),
		out(ValentineACI, yday-20*min, "Soon. I promise."),
		in(GraffACI, GraffACI, yday-10*min, "Your schedule has changed. Check the board."),

		// Ender and Bean: last evening, then this morning.
		in(BeanACI, BeanACI, yday, "Saw the new schedule. Two battles tomorrow?"),
		out(BeanACI, yday+1*min, "Two. Back to back. Graff is pushing us."),
		in(BeanACI, BeanACI, yday+3*min, "Then we stop playing their game and start playing ours."),
		out(BeanACI, yday+4*min, "Meaning?"),
		in(BeanACI, BeanACI, yday+7*min, "Toon leaders call their own moves once we're through the door. You set the goal, we find the way."),
		out(BeanACI, yday+12*min, "Do it. Drill it with your toon tonight. Quietly."),
		formation,
		reply,
		in(BeanACI, BeanACI, today+9*min, "I've been working on it for three weeks."),
		out(BeanACI, today+10*min, "Remember which way is down."),
		always,
		standings,
		out(BeanACI, today+29*min, "Don't read those. They're for the teachers."),
		in(BeanACI, BeanACI, today+30*min, "Everyone reads them. That's why they post them."),
		out(BeanACI, today+58*min, "Battle room at seven thirty. Bring the new kids."),
		in(BeanACI, BeanACI, today+60*min, "Already there."),

		// Waiting to be read.
		unread(in(PetraACI, PetraACI, now-50*min, "Extra practice after dinner? I want another go at the freeze drill.")),
		unread(in(GroupID, CrazyTomACI, now-35*min, "Who moved practice to 0630?")),
		unread(in(GroupID, HotSoupACI, now-34*min, "Ender. Obviously.")),
		unread(in(GroupID, FlyMoloACI, now-20*min, "Somebody tell Bean to stop being right about everything.")),
	}
	for _, m := range msgs {
		_ = f.Inject(backend.MessageEvent{Message: m})
	}
	_ = f.Inject(backend.ReactionEvent{Thread: BeanACI, Reactor: SelfACI, Target: model.MessageRef{Author: BeanACI, TS: always.TS}, Emoji: "👍", TS: always.TS + min})
}
