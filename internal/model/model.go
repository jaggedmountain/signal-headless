// SPDX-FileCopyrightText: 2026 Jeff Mattson
// SPDX-License-Identifier: AGPL-3.0-or-later

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
	ExpireTimer uint32     `json:"expireTimer,omitempty"` // disappearing messages, seconds
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
	// Kind is "" for files sent with the message, AttachmentPreview for a
	// link preview's image (shown with the preview, not as a file).
	Kind string `json:"kind,omitempty"`
	// Pointer is the backend's opaque download handle; never sent to clients.
	Pointer []byte `json:"-"`
}

const AttachmentPreview = "preview"

// OutgoingPreview is a link preview to send; Image is a local file ("" for none).
type OutgoingPreview struct {
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Date        int64  `json:"date,omitempty"`
	Image       string `json:"image,omitempty"`
}

// LinkPreview is the card a sender's app attached to a link in the message
// (Signal clients fetch it when sending; receivers never fetch the page).
type LinkPreview struct {
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Date        int64  `json:"date,omitempty"` // ms, publication date if given
	// Image indexes Attachments (an AttachmentPreview entry); -1 for none.
	Image int `json:"image"`
}

type Reaction struct {
	Reactor string `json:"reactor"`
	Emoji   string `json:"emoji"`
	TS      int64  `json:"ts,omitempty"`
}

type Message struct {
	ID          int64         `json:"id"`
	Thread      ThreadID      `json:"thread"`
	Author      string        `json:"author"`
	AuthorName  string        `json:"authorName,omitempty"`
	TS          int64         `json:"ts"`
	ServerTS    int64         `json:"serverTs,omitempty"`
	ReceivedAt  int64         `json:"receivedAt,omitempty"`
	Outgoing    bool          `json:"outgoing,omitempty"`
	Read        bool          `json:"read,omitempty"` // incoming only
	Status      Status        `json:"status,omitempty"`
	Body        string        `json:"body"`
	Quote       *Quote        `json:"quote,omitempty"`
	Attachments []Attachment  `json:"attachments,omitempty"`
	Previews    []LinkPreview `json:"previews,omitempty"`
	Reactions   []Reaction    `json:"reactions,omitempty"`
	EditedAt    int64         `json:"editedAt,omitempty"`
	Deleted     bool          `json:"deleted,omitempty"`
	ExpiresIn   int64         `json:"expiresIn,omitempty"`
	Sticker     string        `json:"sticker,omitempty"`
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
	if files := m.Files(); len(files) > 0 {
		label := "[attachment]"
		if len(files) > 1 {
			label = "[" + strconv.Itoa(len(files)) + " attachments]"
		} else if files[0].VoiceNote {
			label = "[voice note]"
		} else if files[0].Filename != "" {
			label = "[" + files[0].Filename + "]"
		}
		if s == "" {
			s = label
		} else {
			s = label + " " + s
		}
	}
	return s
}

// Files returns the attachments sent as files (not link-preview images).
func (m *Message) Files() []Attachment {
	var out []Attachment
	for _, a := range m.Attachments {
		if a.Kind != AttachmentPreview {
			out = append(out, a)
		}
	}
	return out
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

// HistoryStatus is the state of the message-history transfer that follows
// linking: "waiting" for the phone to upload, "downloading", "importing",
// then "done", "declined" (the phone chose not to transfer) or "failed".
type HistoryStatus struct {
	State    string `json:"state"`
	Chats    int    `json:"chats,omitempty"`
	Messages int    `json:"messages,omitempty"`
	Error    string `json:"error,omitempty"`
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
	// Previews: link previews to attach (the page was fetched by us, the
	// sender, as Signal's apps do). Image is a local file.
	Previews []OutgoingPreview `json:"previews,omitempty"`
	// TS is the message timestamp, chosen by the daemon.
	TS int64 `json:"-"`
	// Set by the daemon from the thread's disappearing-messages setting.
	ExpireTimer   uint32 `json:"-"`
	ExpireVersion uint32 `json:"-"`
}

// TimerLabel describes a disappearing-messages timer ("off", "30 seconds",
// "1 hour", "4 weeks") the way Signal's apps do.
func TimerLabel(seconds uint32) string {
	if seconds == 0 {
		return "off"
	}
	units := []struct {
		name string
		secs uint32
	}{{"week", 7 * 86400}, {"day", 86400}, {"hour", 3600}, {"minute", 60}, {"second", 1}}
	for _, u := range units {
		if seconds%u.secs == 0 {
			n := seconds / u.secs
			if n == 1 {
				return "1 " + u.name
			}
			return strconv.FormatUint(uint64(n), 10) + " " + u.name + "s"
		}
	}
	return strconv.FormatUint(uint64(seconds), 10) + " seconds"
}
