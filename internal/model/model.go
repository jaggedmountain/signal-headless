// Package model holds the types shared by the daemon, its backends, the RPC
// protocol and clients. They are JSON-encoded on the wire as-is.
package model

import (
	"strconv"
	"strings"
)

type ThreadKind string

const (
	Direct ThreadKind = "direct"
	Group  ThreadKind = "group"
)

// ThreadID identifies a conversation: a service ID string ("<uuid>" for an
// ACI, "PNI:<uuid>") for direct chats, or a base64 group identifier.
type ThreadID string

type Thread struct {
	ID          ThreadID   `json:"id"`
	Kind        ThreadKind `json:"kind"`
	Title       string     `json:"title"`
	LastTS      int64      `json:"lastTs"`
	Unread      int        `json:"unread"`
	Archived    bool       `json:"archived,omitempty"`
	LastPreview string     `json:"lastPreview,omitempty"`
	LastAuthor  string     `json:"lastAuthor,omitempty"`
	NoteToSelf  bool       `json:"noteToSelf,omitempty"`
}

type Status string

const (
	StatusSending   Status = "sending"
	StatusSent      Status = "sent"
	StatusDelivered Status = "delivered"
	StatusRead      Status = "read"
	StatusFailed    Status = "failed"
)

// statusRank orders outgoing statuses so receipts only move them forward.
func (s Status) Rank() int {
	switch s {
	case StatusSending:
		return 1
	case StatusSent:
		return 2
	case StatusDelivered:
		return 3
	case StatusRead:
		return 4
	}
	return 0
}

type Quote struct {
	Author string `json:"author"`
	TS     int64  `json:"ts"`
	Text   string `json:"text,omitempty"`
}

type AttachmentState string

const (
	AttachmentPending AttachmentState = "pending"
	AttachmentDone    AttachmentState = "done"
	AttachmentFailed  AttachmentState = "failed"
)

type Attachment struct {
	Index       int             `json:"index"`
	ContentType string          `json:"contentType,omitempty"`
	Filename    string          `json:"filename,omitempty"`
	Size        int64           `json:"size,omitempty"`
	Path        string          `json:"path,omitempty"`
	State       AttachmentState `json:"state"`
	Error       string          `json:"error,omitempty"`
	VoiceNote   bool            `json:"voiceNote,omitempty"`
	// Pointer is the backend's opaque download handle; never sent to clients.
	Pointer []byte `json:"-"`
}

type Reaction struct {
	Reactor string `json:"reactor"`
	Emoji   string `json:"emoji"`
	TS      int64  `json:"ts,omitempty"`
}

type Message struct {
	ID          int64        `json:"id"`
	Thread      ThreadID     `json:"thread"`
	Author      string       `json:"author"`
	AuthorName  string       `json:"authorName,omitempty"`
	TS          int64        `json:"ts"`
	ServerTS    int64        `json:"serverTs,omitempty"`
	ReceivedAt  int64        `json:"receivedAt,omitempty"`
	Outgoing    bool         `json:"outgoing,omitempty"`
	Read        bool         `json:"read,omitempty"` // incoming only
	Status      Status       `json:"status,omitempty"`
	Body        string       `json:"body"`
	Quote       *Quote       `json:"quote,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Reactions   []Reaction   `json:"reactions,omitempty"`
	EditedAt    int64        `json:"editedAt,omitempty"`
	Deleted     bool         `json:"deleted,omitempty"`
	ExpiresIn   int64        `json:"expiresIn,omitempty"`
	Sticker     string       `json:"sticker,omitempty"`
}

// Preview is a one-line summary used in thread lists and notifications.
func (m *Message) Preview() string {
	if m.Deleted {
		return "(deleted)"
	}
	s := strings.Join(strings.Fields(m.Body), " ")
	if s == "" && m.Sticker != "" {
		s = "[sticker " + m.Sticker + "]"
	}
	if len(m.Attachments) > 0 {
		label := "[attachment]"
		if len(m.Attachments) > 1 {
			label = "[" + strconv.Itoa(len(m.Attachments)) + " attachments]"
		} else if m.Attachments[0].VoiceNote {
			label = "[voice note]"
		} else if m.Attachments[0].Filename != "" {
			label = "[" + m.Attachments[0].Filename + "]"
		}
		if s == "" {
			s = label
		} else {
			s = label + " " + s
		}
	}
	return s
}

type Contact struct {
	ID       string `json:"id"` // ACI (or PNI:uuid when no ACI is known)
	Number   string `json:"number,omitempty"`
	Name     string `json:"name"`
	Nickname string `json:"nickname,omitempty"`
	Profile  string `json:"profileName,omitempty"`
	Blocked  bool   `json:"blocked,omitempty"`
}

// DisplayName picks the best human label, falling back to the number or ID.
func (c Contact) DisplayName() string {
	for _, s := range []string{c.Nickname, c.Name, c.Profile, c.Number} {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return c.ID
}

type GroupInfo struct {
	ID      ThreadID `json:"id"`
	Title   string   `json:"title"`
	Members []string `json:"members,omitempty"`
}

type Account struct {
	ACI      string `json:"aci"`
	PNI      string `json:"pni,omitempty"`
	Number   string `json:"number"`
	DeviceID int    `json:"deviceId"`
}

type ConnState string

const (
	ConnConnecting   ConnState = "connecting"
	ConnConnected    ConnState = "connected"
	ConnDisconnected ConnState = "disconnected"
	ConnLoggedOut    ConnState = "logged-out"
	ConnError        ConnState = "error"
)

// MessageRef names a message by its Signal identity (author + timestamp).
type MessageRef struct {
	Author string `json:"author"`
	TS     int64  `json:"ts"`
}

// Outgoing is a message a client asks the daemon to send.
type Outgoing struct {
	Thread      ThreadID `json:"thread"`
	Body        string   `json:"body"`
	Attachments []string `json:"attachments,omitempty"` // absolute paths on the daemon host
	Quote       *Quote   `json:"quote,omitempty"`
}
