// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package signalbackend

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-signal/pkg/libsignalgo"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/backuppb"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/signalpb"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/store"
	"google.golang.org/protobuf/proto"

	"signal-headless/internal/backend"
	"signal-headless/internal/db"
	"signal-headless/internal/model"
)

// testBackend is a Backend over a scratch store with one device and no
// network client: enough for the history import.
func testBackend(t *testing.T) (*Backend, *[]backend.Event) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "t.db"), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	aciKP, _ := libsignalgo.GenerateIdentityKeyPair()
	pniKP, _ := libsignalgo.GenerateIdentityKeyPair()
	dd := store.DeviceData{ACI: uuid.New(), PNI: uuid.New(), ACIIdentityKeyPair: aciKP, PNIIdentityKeyPair: pniKP, DeviceID: 2, Number: "+15550000000", Password: "x"}
	if err := d.Signal.PutDevice(ctx, &dd); err != nil {
		t.Fatal(err)
	}
	dev, err := d.Signal.DeviceByACI(ctx, dd.ACI)
	if err != nil {
		t.Fatal(err)
	}
	var got []backend.Event
	b := &Backend{db: d, dev: dev, log: zerolog.Nop()}
	b.handler = func(ctx context.Context, e backend.Event) error {
		got = append(got, e)
		return nil
	}
	return b, &got
}

func u64(v uint64) *uint64 { return &v }
func str(s string) *string { return &s }

func TestHistoryImport(t *testing.T) {
	ctx := context.Background()
	b, got := testBackend(t)
	bs := b.dev.BackupStore
	alice := uuid.New()
	masterKey := make([]byte, libsignalgo.GroupMasterKeyLength)
	rand.Read(masterKey)
	gid, _ := libsignalgo.GroupMasterKey(masterKey).GroupIdentifier()
	groupID := base64.StdEncoding.EncodeToString(gid[:])

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(bs.AddBackupRecipient(ctx, &backuppb.Recipient{Id: 1, Destination: &backuppb.Recipient_Self{Self: &backuppb.Self{}}}))
	must(bs.AddBackupRecipient(ctx, &backuppb.Recipient{Id: 2, Destination: &backuppb.Recipient_Contact{Contact: &backuppb.Contact{
		Aci: alice[:], ProfileGivenName: str("Alice")}}}))
	must(bs.AddBackupRecipient(ctx, &backuppb.Recipient{Id: 3, Destination: &backuppb.Recipient_Group{Group: &backuppb.Group{MasterKey: masterKey}}}))
	must(bs.AddBackupChat(ctx, &backuppb.Chat{Id: 10, RecipientId: 2, ExpirationTimerMs: u64(3600_000), ExpireTimerVersion: 2}))
	must(bs.AddBackupChat(ctx, &backuppb.Chat{Id: 11, RecipientId: 3, Archived: true}))

	now := time.Now()
	recent := uint64(now.Add(-time.Hour).UnixMilli())
	ancient := uint64(now.Add(-90 * 24 * time.Hour).UnixMilli())
	incoming := func(read bool) *backuppb.ChatItem_Incoming {
		return &backuppb.ChatItem_Incoming{Incoming: &backuppb.ChatItem_IncomingMessageDetails{DateServerSent: u64(recent), DateReceived: recent, Read: read}}
	}
	text := func(s string) *backuppb.ChatItem_StandardMessage {
		return &backuppb.ChatItem_StandardMessage{StandardMessage: &backuppb.StandardMessage{Text: &backuppb.Text{Body: s}}}
	}
	hash := []byte("0123456789abcdef0123456789abcdef")
	items := []*backuppb.ChatItem{
		// Alice: text with a reaction, read on the phone.
		{ChatId: 10, AuthorId: 2, DateSent: recent, DirectionalDetails: incoming(true), Item: &backuppb.ChatItem_StandardMessage{StandardMessage: &backuppb.StandardMessage{
			Text:      &backuppb.Text{Body: "hello from the past"},
			Reactions: []*backuppb.Reaction{{AuthorId: 1, Emoji: "👍", SentTimestamp: recent + 5}},
		}}},
		// Our reply, quoting it, read by Alice.
		{ChatId: 10, AuthorId: 1, DateSent: recent + 10, DirectionalDetails: &backuppb.ChatItem_Outgoing{Outgoing: &backuppb.ChatItem_OutgoingMessageDetails{
			SendStatus: []*backuppb.SendStatus{{RecipientId: 2, DeliveryStatus: &backuppb.SendStatus_Read_{Read: &backuppb.SendStatus_Read{}}}},
		}}, Item: &backuppb.ChatItem_StandardMessage{StandardMessage: &backuppb.StandardMessage{
			Text:  &backuppb.Text{Body: "hi back"},
			Quote: &backuppb.Quote{TargetSentTimestamp: u64(recent), AuthorId: 2, Text: &backuppb.Text{Body: "hello from the past"}},
		}}},
		// Unread, with an attachment verified by plaintext hash.
		{ChatId: 10, AuthorId: 2, DateSent: recent + 20, DirectionalDetails: incoming(false), Item: &backuppb.ChatItem_StandardMessage{StandardMessage: &backuppb.StandardMessage{
			Attachments: []*backuppb.MessageAttachment{{Pointer: &backuppb.FilePointer{ContentType: str("image/png"), FileName: str("pic.png"),
				LocatorInfo: &backuppb.FilePointer_LocatorInfo{Key: []byte("k"), Size: 3, TransitCdnKey: str("cdnkey"), TransitCdnNumber: proto.Uint32(3),
					IntegrityCheck: &backuppb.FilePointer_LocatorInfo_PlaintextHash{PlaintextHash: hash}}}}},
		}}},
		// Deleted for everyone: dropped.
		{ChatId: 10, AuthorId: 2, DateSent: recent + 30, DirectionalDetails: incoming(true), Item: &backuppb.ChatItem_RemoteDeletedMessage{RemoteDeletedMessage: &backuppb.RemoteDeletedMessage{}}},
		// Disappearing, started on the phone an hour ago: imported with that
		// start, so it goes when the phone's copy does.
		{ChatId: 10, AuthorId: 2, DateSent: recent + 35, ExpireStartDate: u64(recent), ExpiresInMs: u64(2 * 3600_000), DirectionalDetails: incoming(true), Item: text("still counting")},
		// Disappeared already: dropped.
		{ChatId: 10, AuthorId: 2, DateSent: recent + 40, ExpireStartDate: u64(recent), ExpiresInMs: u64(1000), DirectionalDetails: incoming(true), Item: text("poof")},
		// Group: an old photo (not fetched automatically) and one the
		// transfer didn't include.
		{ChatId: 11, AuthorId: 2, DateSent: ancient, DirectionalDetails: incoming(true), Item: &backuppb.ChatItem_StandardMessage{StandardMessage: &backuppb.StandardMessage{
			Text: &backuppb.Text{Body: "old photo"},
			Attachments: []*backuppb.MessageAttachment{
				{Pointer: &backuppb.FilePointer{FileName: str("old.jpg"), LocatorInfo: &backuppb.FilePointer_LocatorInfo{Key: []byte("k"), TransitCdnKey: str("x"),
					IntegrityCheck: &backuppb.FilePointer_LocatorInfo_EncryptedDigest{EncryptedDigest: []byte("d")}}}},
				{Pointer: &backuppb.FilePointer{FileName: str("gone.jpg")}},
			},
		}}},
	}
	for _, it := range items {
		must(bs.AddBackupChatItem(ctx, it))
	}
	must(bs.RecalculateChatCounts(ctx))

	b.runHistory(ctx)

	var hist []backend.HistoryEvent
	var states []string
	for _, e := range *got {
		switch e := e.(type) {
		case backend.HistoryEvent:
			hist = append(hist, e)
		case backend.HistoryStatusEvent:
			states = append(states, e.Status.State)
		}
	}
	if len(states) < 2 || states[0] != "importing" || states[len(states)-1] != "done" {
		t.Fatalf("states = %v", states)
	}
	if len(hist) != 2 {
		t.Fatalf("history events = %d", len(hist))
	}
	byThread := map[model.ThreadID]backend.HistoryEvent{}
	for _, h := range hist {
		byThread[h.Thread] = h
	}
	a := byThread[model.ThreadID(alice.String())]
	if a.Kind != model.Direct || a.ExpireTimer != 3600 || a.ExpireVersion != 2 {
		t.Fatalf("alice chat = %+v", a)
	}
	if len(a.Messages) != 4 {
		t.Fatalf("alice messages = %+v", a.Messages)
	}
	msgs := map[string]model.Message{}
	for _, m := range a.Messages {
		msgs[m.Body] = m
	}
	first := msgs["hello from the past"]
	if first.Outgoing || !first.Read || len(first.Reactions) != 1 || first.Reactions[0].Reactor != b.self() {
		t.Fatalf("first = %+v", first)
	}
	reply := msgs["hi back"]
	if !reply.Outgoing || reply.Status != model.StatusRead || reply.Quote == nil || reply.Quote.Author != alice.String() || reply.Quote.TS != int64(recent) {
		t.Fatalf("reply = %+v", reply)
	}
	if c := msgs["still counting"]; c.ExpiresIn != 7200 || c.ExpireStart != int64(recent) {
		t.Fatalf("disappearing message: expiresIn %d, expireStart %d, want 7200, %d", c.ExpiresIn, c.ExpireStart, recent)
	}
	pic := msgs[""]
	if pic.Read || len(pic.Attachments) != 1 || pic.Attachments[0].State != model.AttachmentPending {
		t.Fatalf("pic = %+v", pic)
	}
	raw, ph, err := unwrapPointer(pic.Attachments[0].Pointer)
	var ap signalpb.AttachmentPointer
	if err != nil || string(ph) != string(hash) || proto.Unmarshal(raw, &ap) != nil || ap.GetCdnKey() != "cdnkey" || ap.GetCdnNumber() != 3 {
		t.Fatalf("pointer: hash=%q err=%v ptr=%v", ph, err, &ap)
	}

	g := byThread[model.ThreadID(groupID)]
	if g.Kind != model.Group || !g.Archived || len(g.Messages) != 1 {
		t.Fatalf("group chat = %+v", g)
	}
	atts := g.Messages[0].Attachments
	if len(atts) != 2 || atts[0].State != model.AttachmentFailed || atts[1].State != model.AttachmentFailed || atts[1].Error == atts[0].Error {
		t.Fatalf("group attachments = %+v", atts)
	}

	if chats, _ := bs.GetBackupChats(ctx); len(chats) != 0 {
		t.Fatalf("backup store should be empty after import, has %d chats", len(chats))
	}
}

func TestPointerWrapping(t *testing.T) {
	p := []byte{1, 2, 3}
	if got := wrapPointer(p, nil); string(got) != string(p) {
		t.Fatal("no hash: pointer unchanged")
	}
	raw, h, err := unwrapPointer(wrapPointer(p, []byte("hash")))
	if err != nil || string(raw) != string(p) || string(h) != "hash" {
		t.Fatalf("round trip: %v %q %v", raw, h, err)
	}
	if _, _, err := unwrapPointer(append([]byte("shph1:"), 200)); err == nil {
		t.Fatal("truncated wrapper must fail")
	}
}

func TestLinkPreviews(t *testing.T) {
	b, _ := testBackend(t)
	body := "look https://example.com/tea?x=1 now"
	url := "https://example.com/tea?x=1"
	img := &signalpb.AttachmentPointer{ContentType: proto.String("image/jpeg"), AttachmentIdentifier: &signalpb.AttachmentPointer_CdnKey{CdnKey: "k"}}
	dm := &signalpb.DataMessage{Body: &body, Timestamp: proto.Uint64(1000), Preview: []*signalpb.Preview{
		{Url: &url, Title: proto.String("Tea"), Description: proto.String("All about tea"), Image: img},
		{Url: proto.String("https://elsewhere.example/"), Title: proto.String("not in the text")},
		{Url: proto.String("javascript:alert(1)"), Title: proto.String("bad scheme")},
	}}
	evts := b.translateData(context.Background(), "thread", "00000000-0000-4000-8000-00000000000a", 0, dm)
	var m model.Message
	for _, e := range evts {
		if me, ok := e.(backend.MessageEvent); ok {
			m = me.Message
		}
	}
	if len(m.Previews) != 1 {
		t.Fatalf("previews = %+v", m.Previews)
	}
	p := m.Previews[0]
	if p.URL != url || p.Title != "Tea" || p.Description != "All about tea" || p.Image != 0 {
		t.Fatalf("preview = %+v", p)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].Kind != model.AttachmentPreview || len(m.Files()) != 0 {
		t.Fatalf("attachments = %+v", m.Attachments)
	}
	if got := m.Preview(); got != body {
		t.Fatalf("thread preview should ignore the preview image: %q", got)
	}
}

func TestDeleteForMeWire(t *testing.T) {
	alice := "00000000-0000-4000-8000-00000000000a"
	gid := make([]byte, 32)
	gid[0] = 7
	group := model.ThreadID(base64.StdEncoding.EncodeToString(gid))
	var deletes []backend.MessageDelete
	for i := 0; i < 700; i++ {
		deletes = append(deletes, backend.MessageDelete{Thread: model.ThreadID(alice), Ref: model.MessageRef{Author: alice, TS: int64(i + 1)}})
	}
	deletes = append(deletes, backend.MessageDelete{Thread: group, Ref: model.MessageRef{Author: alice, TS: 9}})
	batches, err := packDeletes(deletes)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 {
		t.Fatalf("batches = %d", len(batches))
	}
	count := func(b []*signalpb.SyncMessage_DeleteForMe_MessageDeletes) (n int) {
		for _, md := range b {
			n += len(md.Messages)
		}
		return
	}
	if count(batches[0]) != 500 || count(batches[1]) != 201 {
		t.Fatalf("batch sizes %d, %d", count(batches[0]), count(batches[1]))
	}
	// Round trip through what the phone would send back.
	b, _ := testBackend(t)
	e := b.translateDeleteForMe(&signalpb.SyncMessage_DeleteForMe{
		MessageDeletes: batches[1],
		ConversationDeletes: []*signalpb.SyncMessage_DeleteForMe_ConversationDelete{{
			Conversation:       batches[1][0].Conversation,
			MostRecentMessages: []*signalpb.AddressableMessage{{Author: &signalpb.AddressableMessage_AuthorServiceId{AuthorServiceId: alice}, SentTimestamp: proto.Uint64(650)}},
			IsFullDelete:       proto.Bool(false),
		}},
		LocalOnlyConversationDeletes: []*signalpb.SyncMessage_DeleteForMe_LocalOnlyConversationDelete{{Conversation: batches[1][len(batches[1])-1].Conversation}},
	})
	if len(e.Messages) != 201 || e.Messages[200].Thread != group || e.Messages[0].Ref.TS != 501 {
		t.Fatalf("messages: %d, last %+v", len(e.Messages), e.Messages[len(e.Messages)-1])
	}
	if len(e.Conversations) != 2 || e.Conversations[0] != (backend.ConversationDelete{Thread: model.ThreadID(alice), Through: 650}) ||
		e.Conversations[1] != (backend.ConversationDelete{Thread: group, Full: true}) {
		t.Fatalf("conversations = %+v", e.Conversations)
	}
	if _, err := packDeletes([]backend.MessageDelete{{Thread: "not-a-thread"}}); err == nil {
		t.Fatal("bad thread accepted")
	}
}
