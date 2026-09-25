// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package signalbackend

// "Delete for me": deleting messages from all of the account's own devices
// (the other side keeps them), carried by a sync message to ourselves.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"go.mau.fi/mautrix-signal/pkg/libsignalgo"
	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/signalpb"
	"google.golang.org/protobuf/proto"

	"signal-headless/internal/backend"
	"signal-headless/internal/model"
)

// deleteBatch caps the messages per sync message.
const deleteBatch = 500

func (b *Backend) DeleteForMe(ctx context.Context, deletes []backend.MessageDelete) error {
	batches, err := packDeletes(deletes)
	if err != nil {
		return err
	}
	self := b.dev.ACIServiceID()
	for i, md := range batches {
		content := signalmeow.WrapSyncMessage(&signalpb.SyncMessage{Content: &signalpb.SyncMessage_DeleteForMe_{
			DeleteForMe: &signalpb.SyncMessage_DeleteForMe{MessageDeletes: md},
		}})
		res := b.cli.SendMessage(ctx, self, content)
		if !res.WasSuccessful {
			err := res.Error
			if err == nil {
				err = errors.New("sync message not sent")
			}
			return fmt.Errorf("delete-for-me sync %d of %d: %w", i+1, len(batches), err)
		}
	}
	return nil
}

// packDeletes groups deletes by conversation into sync-message batches of at
// most deleteBatch messages each.
func packDeletes(deletes []backend.MessageDelete) ([][]*signalpb.SyncMessage_DeleteForMe_MessageDeletes, error) {
	byThread := map[model.ThreadID][]*signalpb.AddressableMessage{}
	var order []model.ThreadID
	for _, d := range deletes {
		if _, seen := byThread[d.Thread]; !seen {
			order = append(order, d.Thread)
		}
		byThread[d.Thread] = append(byThread[d.Thread], &signalpb.AddressableMessage{
			Author:        &signalpb.AddressableMessage_AuthorServiceId{AuthorServiceId: d.Ref.Author},
			SentTimestamp: proto.Uint64(uint64(d.Ref.TS)),
		})
	}
	// Pack conversations into sync messages of at most deleteBatch messages.
	var batches [][]*signalpb.SyncMessage_DeleteForMe_MessageDeletes
	var cur []*signalpb.SyncMessage_DeleteForMe_MessageDeletes
	n := 0
	for _, t := range order {
		conv, err := conversationID(t)
		if err != nil {
			return nil, err
		}
		msgs := byThread[t]
		for len(msgs) > 0 {
			take := min(len(msgs), deleteBatch-n)
			cur = append(cur, &signalpb.SyncMessage_DeleteForMe_MessageDeletes{Conversation: conv, Messages: msgs[:take]})
			msgs, n = msgs[take:], n+take
			if n == deleteBatch {
				batches, cur, n = append(batches, cur), nil, 0
			}
		}
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	return batches, nil
}

// conversationID names a thread in sync messages.
func conversationID(t model.ThreadID) (*signalpb.ConversationIdentifier, error) {
	s := string(t)
	if _, err := libsignalgo.ServiceIDFromString(s); err == nil {
		return &signalpb.ConversationIdentifier{Identifier: &signalpb.ConversationIdentifier_ThreadServiceId{ThreadServiceId: s}}, nil
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("unknown conversation %q", s)
	}
	return &signalpb.ConversationIdentifier{Identifier: &signalpb.ConversationIdentifier_ThreadGroupId{ThreadGroupId: raw}}, nil
}

// threadOf maps a sync message's conversation back to our thread id.
func threadOf(c *signalpb.ConversationIdentifier) (model.ThreadID, bool) {
	switch id := c.GetIdentifier().(type) {
	case *signalpb.ConversationIdentifier_ThreadServiceId:
		if sid, err := libsignalgo.ServiceIDFromString(id.ThreadServiceId); err == nil {
			return model.ThreadID(sid.String()), true
		}
	case *signalpb.ConversationIdentifier_ThreadServiceIdBinary:
		if sid, err := libsignalgo.ServiceIDFromBytes(id.ThreadServiceIdBinary); err == nil {
			return model.ThreadID(sid.String()), true
		}
	case *signalpb.ConversationIdentifier_ThreadGroupId:
		if len(id.ThreadGroupId) == 32 {
			return model.ThreadID(base64.StdEncoding.EncodeToString(id.ThreadGroupId)), true
		}
	}
	return "", false
}

func authorOf(m *signalpb.AddressableMessage) (string, bool) {
	switch a := m.GetAuthor().(type) {
	case *signalpb.AddressableMessage_AuthorServiceId:
		if sid, err := libsignalgo.ServiceIDFromString(a.AuthorServiceId); err == nil {
			return sid.String(), true
		}
	case *signalpb.AddressableMessage_AuthorServiceIdBinary:
		if sid, err := libsignalgo.ServiceIDFromBytes(a.AuthorServiceIdBinary); err == nil {
			return sid.String(), true
		}
	}
	return "", false
}

func (b *Backend) translateDeleteForMe(d *signalpb.SyncMessage_DeleteForMe) backend.DeleteForMeEvent {
	var e backend.DeleteForMeEvent
	for _, md := range d.GetMessageDeletes() {
		t, ok := threadOf(md.GetConversation())
		if !ok {
			continue
		}
		for _, m := range md.GetMessages() {
			if a, ok := authorOf(m); ok {
				e.Messages = append(e.Messages, backend.MessageDelete{Thread: t, Ref: model.MessageRef{Author: a, TS: int64(m.GetSentTimestamp())}})
			}
		}
	}
	conv := func(c *signalpb.ConversationIdentifier, recent []*signalpb.AddressableMessage, full bool) {
		t, ok := threadOf(c)
		if !ok {
			return
		}
		var through int64
		for _, m := range recent {
			through = max(through, int64(m.GetSentTimestamp()))
		}
		e.Conversations = append(e.Conversations, backend.ConversationDelete{Thread: t, Through: through, Full: full})
	}
	for _, cd := range d.GetConversationDeletes() {
		conv(cd.GetConversation(), append(cd.GetMostRecentMessages(), cd.GetMostRecentNonExpiringMessages()...), cd.GetIsFullDelete())
	}
	for _, lc := range d.GetLocalOnlyConversationDeletes() {
		conv(lc.GetConversation(), nil, true)
	}
	if len(d.GetAttachmentDeletes()) > 0 {
		b.log.Debug().Int("count", len(d.GetAttachmentDeletes())).Msg("Ignoring attachment delete-for-me (not supported)")
	}
	return e
}
