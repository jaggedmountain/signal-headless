// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package backend defines the boundary between the daemon and Signal.
//
// The real implementation (package signalbackend) wraps signalmeow; package
// fakebackend provides an in-memory stand-in for tests and development.
package backend

import (
	"context"

	"signal-headless/internal/model"
)

// Handler receives events from the backend. It must persist whatever it needs
// before returning nil: a nil return acknowledges the message to the Signal
// server, after which it will never be delivered again.
type Handler func(ctx context.Context, evt Event) error

type Backend interface {
	// Run connects and processes events until ctx is cancelled or the device
	// is logged out.
	Run(ctx context.Context, h Handler) error
	Account() model.Account

	// Send delivers out using out.TS as the Signal message timestamp.
	Send(ctx context.Context, out model.Outgoing) error
	SendReaction(ctx context.Context, thread model.ThreadID, target model.MessageRef, emoji string, remove bool) error
	SendDelete(ctx context.Context, thread model.ThreadID, targetTS int64) error
	SendTyping(ctx context.Context, thread model.ThreadID, typing bool) error
	// MarkRead sends read receipts to the authors and a read sync to our other devices.
	MarkRead(ctx context.Context, thread model.ThreadID, refs []model.MessageRef) error

	Contacts(ctx context.Context) ([]model.Contact, error)
	Groups(ctx context.Context) ([]model.GroupInfo, error)
	// ThreadInfo returns the kind and display title of a thread.
	ThreadInfo(ctx context.Context, thread model.ThreadID) (model.ThreadKind, string, error)
	// Contact returns what the local store knows about a service ID.
	Contact(ctx context.Context, id string) (model.Contact, bool)
	// ResolveRecipient turns user input (+E164, UUID, group ID, 'self') into a thread ID.
	ResolveRecipient(ctx context.Context, s string) (model.ThreadID, error)

	// Unlink removes this device from the Signal account and deletes its
	// keys locally. Message history is not touched.
	Unlink(ctx context.Context) error

	// DownloadAttachment fetches and decrypts an attachment into dest.
	DownloadAttachment(ctx context.Context, pointer []byte, dest string) error
	// DeleteForMe asks the account's other devices (the phone too) to delete
	// these messages for us; the other side keeps them.
	DeleteForMe(ctx context.Context, deletes []MessageDelete) error
}

// Event is one of the *Event types below.
type Event interface{ isEvent() }

// MessageEvent is a new message, incoming or sent from one of our other devices.
type MessageEvent struct{ Message model.Message }

// EditEvent replaces the body of an earlier message.
type EditEvent struct {
	Thread   model.ThreadID
	Author   string
	TargetTS int64
	NewTS    int64
	Body     string
}

// DeleteEvent is a remote delete ("delete for everyone").
type DeleteEvent struct {
	Thread   model.ThreadID
	Author   string
	TargetTS int64
}

type ReactionEvent struct {
	Thread  model.ThreadID
	Reactor string
	Target  model.MessageRef
	Emoji   string
	Remove  bool
	TS      int64
}

type ReceiptKind string

const (
	ReceiptDelivered ReceiptKind = "delivered"
	ReceiptRead      ReceiptKind = "read"
	ReceiptViewed    ReceiptKind = "viewed"
)

// ReceiptEvent reports that From received/read our messages with these timestamps.
type ReceiptEvent struct {
	From       string
	Kind       ReceiptKind
	Timestamps []int64
}

// ReadSyncEvent reports messages read on another of our devices.
type ReadSyncEvent struct{ Refs []model.MessageRef }

type TypingEvent struct {
	Thread model.ThreadID
	Sender string
	Typing bool
}

type ConnectionEvent struct {
	State model.ConnState
	Err   string
}

// TimerEvent changes a thread's disappearing-messages timer.
type TimerEvent struct {
	Thread  model.ThreadID
	Seconds uint32
	Version uint32
}

// ContactsEvent signals that contact or group metadata changed.
type ContactsEvent struct{}

// QueueEmptyEvent is sent once the server has delivered all queued messages.
type QueueEmptyEvent struct{}

// HistoryEvent carries one conversation's transferred message history
// (after linking with "Transfer message history"). Messages are old: they
// are stored, not announced.
type HistoryEvent struct {
	Thread        model.ThreadID
	Kind          model.ThreadKind
	Archived      bool
	ExpireTimer   uint32
	ExpireVersion uint32
	Messages      []model.Message
}

// MessageDelete names one message in a conversation for "delete for me".
type MessageDelete struct {
	Thread model.ThreadID
	Ref    model.MessageRef
}

// ConversationDelete: another of our devices cleared a conversation. Through
// is the newest sent timestamp to delete (0: everything); Full also removes
// the conversation itself.
type ConversationDelete struct {
	Thread  model.ThreadID
	Through int64
	Full    bool
}

// DeleteForMeEvent: another of our devices deleted messages "for me" (only
// from the account's own devices, not for the other side).
type DeleteForMeEvent struct {
	Messages      []MessageDelete
	Conversations []ConversationDelete
}

// HistoryStatusEvent reports progress of the history transfer.
type HistoryStatusEvent struct{ Status model.HistoryStatus }

func (MessageEvent) isEvent()       {}
func (EditEvent) isEvent()          {}
func (DeleteEvent) isEvent()        {}
func (ReactionEvent) isEvent()      {}
func (ReceiptEvent) isEvent()       {}
func (ReadSyncEvent) isEvent()      {}
func (TypingEvent) isEvent()        {}
func (ConnectionEvent) isEvent()    {}
func (ContactsEvent) isEvent()      {}
func (TimerEvent) isEvent()         {}
func (QueueEmptyEvent) isEvent()    {}
func (HistoryEvent) isEvent()       {}
func (HistoryStatusEvent) isEvent() {}
func (DeleteForMeEvent) isEvent()   {}
