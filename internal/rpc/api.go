// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"signal-headless/internal/history"
	"signal-headless/internal/model"
)

// ProtocolVersion is the native API's version. It increases whenever
// methods, events or their fields are added or changed, so clients can tell
// a daemon that is too old for them (a daemon keeps running across upgrades).
//
//	1  first versioned API: link previews, history transfer, stats/purge,
//	   delete-for-me, shutdown
const ProtocolVersion = 1

// Native methods. Parameter and result types follow each name.
const (
	MVersion      = "version"      // → VersionResult
	MStatus       = "status"       // → StatusResult
	MSubscribe    = "subscribe"    // switch this connection to native events
	MListThreads  = "listThreads"  // → []model.Thread
	MGetThread    = "getThread"    // ThreadParams → model.Thread
	MGetMessages  = "getMessages"  // GetMessagesParams → []model.Message
	MSearch       = "search"       // SearchParams → []model.Message
	MSend         = "send"         // SendParams → SendResult (also accepts signal-cli params)
	MSendReaction = "sendReaction" // ReactionParams (also signal-cli params)
	MRemoteDelete = "remoteDelete" // DeleteParams (also signal-cli params)
	MSendTyping   = "sendTyping"   // TypingParams
	MMarkRead     = "markRead"     // ThreadParams
	MArchive      = "archiveThread"
	MListContacts = "listContacts" // → []model.Contact
	MListGroups   = "listGroups"   // → []model.GroupInfo
	MResolve      = "resolve"      // ResolveParams → ResolveResult
	MRetry        = "retryAttachment"
	MUnlink       = "unlink"                 // UnlinkParams; removes this device from the account, daemon exits
	MLinkPreview  = "linkPreview"            // LinkPreviewParams → model.OutgoingPreview (fetched; pass it back in send)
	MStats        = "stats"                  // → StatsResult
	MPurge        = "purge"                  // PurgeParams → PurgeResult; deletes local history only
	MRetryFailed  = "retryFailedAttachments" // → RetryFailedResult
	MShutdown     = "shutdown"               // stop the daemon (clients restart it, e.g. after an upgrade)
)

// Native event notifications (after subscribe).
const (
	EvMessage       = "message"       // model.Message (new)
	EvMessageUpdate = "messageUpdate" // model.Message (edited/deleted/reaction/status/attachment)
	EvThread        = "thread"        // model.Thread
	EvTyping        = "typing"        // TypingEvent
	EvConnection    = "connection"    // StatusResult
	EvContacts      = "contacts"      // null; refetch contacts/threads
	// EvMessageRemoved: a message row is gone for good (a deleted message's
	// placeholder expired). Clients drop it from view.
	EvMessageRemoved = "messageRemoved" // MessageRemoved
	EvHistory        = "history"        // HistoryImported
)

// HistoryImported: a conversation's transferred history was stored;
// clients showing it reload.
type HistoryImported struct {
	Thread   model.ThreadID `json:"thread"`
	Messages int            `json:"messages"`
}

type MessageRemoved struct {
	ID     int64          `json:"id"`
	Thread model.ThreadID `json:"thread"`
}

// Compat notification (signal-cli jsonRpc), sent until a client subscribes.
const EvReceive = "receive"

type VersionResult struct {
	Version string `json:"version"`
}

type StatusResult struct {
	Account    model.Account   `json:"account"`
	Connection model.ConnState `json:"connection"`
	Error      string          `json:"error,omitempty"`
	QueueEmpty bool            `json:"queueEmpty"`
	Clients    int             `json:"clients"`
	Version    string          `json:"version"`
	Protocol   int             `json:"protocol"` // ProtocolVersion (0 from daemons before versioning)
	// LinkPreviews is the account's "Generate link previews" setting.
	LinkPreviews bool `json:"linkPreviews"`
	// History is the message-history transfer's progress after linking.
	History *model.HistoryStatus `json:"history,omitempty"`
}

type ThreadParams struct {
	Thread model.ThreadID `json:"thread"`
}

type ArchiveParams struct {
	Thread   model.ThreadID `json:"thread"`
	Archived bool           `json:"archived"`
}

type GetMessagesParams struct {
	Thread model.ThreadID `json:"thread"`
	Before int64          `json:"before,omitempty"` // sender timestamp, exclusive
	Limit  int            `json:"limit,omitempty"`
}

type SearchParams struct {
	Query  string         `json:"query"`
	Thread model.ThreadID `json:"thread,omitempty"`
	Limit  int            `json:"limit,omitempty"`
}

// SendParams is the native send request; signal-cli fields are accepted too
// (see daemon.parseSend).
type SendParams struct {
	Thread      model.ThreadID `json:"thread,omitempty"`
	To          string         `json:"to,omitempty"` // anything ResolveRecipient accepts
	Body        string         `json:"body,omitempty"`
	Attachments []string       `json:"attachments,omitempty"`
	Quote       *model.Quote   `json:"quote,omitempty"`
	// Previews to attach, as returned by linkPreview.
	Previews []model.OutgoingPreview `json:"previews,omitempty"`
}

type LinkPreviewParams struct {
	URL string `json:"url"`
}

type SendResult struct {
	Timestamp int64          `json:"timestamp"`
	Message   *model.Message `json:"message,omitempty"`
}

type ReactionParams struct {
	Thread model.ThreadID   `json:"thread"`
	Target model.MessageRef `json:"target"`
	Emoji  string           `json:"emoji"`
	Remove bool             `json:"remove,omitempty"`
}

type DeleteParams struct {
	Thread   model.ThreadID `json:"thread"`
	TargetTS int64          `json:"targetTs"`
}

type TypingParams struct {
	Thread model.ThreadID `json:"thread"`
	Typing bool           `json:"typing"`
}

type TypingEvent struct {
	Thread model.ThreadID `json:"thread"`
	Sender string         `json:"sender"`
	Name   string         `json:"name,omitempty"`
	Typing bool           `json:"typing"`
}

type ResolveParams struct {
	Recipient string `json:"recipient"`
}

type ResolveResult struct {
	Thread model.ThreadID `json:"thread"`
	Title  string         `json:"title"`
}

type RetryParams struct {
	MessageID int64 `json:"messageId"`
}

// UnlinkParams must carry the account's number as confirmation.
type UnlinkParams struct {
	Number string `json:"number"`
}

// StatsResult: storage and history counts (the diagnostics view).
type StatsResult struct {
	history.Stats
	DataDir         string `json:"dataDir"`
	DBBytes         int64  `json:"dbBytes"`         // database file incl. WAL
	AttachmentFiles int    `json:"attachmentFiles"` // files in the attachments dir
	AttachmentBytes int64  `json:"attachmentBytes"`
}

// PurgeParams: delete messages sent before Before (ms) — in Thread, or all
// conversations. DryRun only counts. Local only: the phone keeps them.
type PurgeParams struct {
	Before int64          `json:"before"`
	Thread model.ThreadID `json:"thread,omitempty"`
	DryRun bool           `json:"dryRun,omitempty"`
	// AllDevices also deletes them from the account's other devices (the
	// phone too) with a "delete for me" sync. Recipients keep their copies.
	AllDevices bool `json:"allDevices,omitempty"`
}

type PurgeResult struct {
	history.PurgeResult
	Bytes        int64 `json:"bytes"`        // attachment files removed (or to remove)
	DBBytesAfter int64 `json:"dbBytesAfter"` // after compacting (0 on a dry run)
}

type RetryFailedResult struct {
	Count int `json:"count"`
}
