package rpc

import "signal-headless/internal/model"

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
)

// Native event notifications (after subscribe).
const (
	EvMessage       = "message"       // model.Message (new)
	EvMessageUpdate = "messageUpdate" // model.Message (edited/deleted/reaction/status/attachment)
	EvThread        = "thread"        // model.Thread
	EvTyping        = "typing"        // TypingEvent
	EvConnection    = "connection"    // StatusResult
	EvContacts      = "contacts"      // null; refetch contacts/threads
)

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
