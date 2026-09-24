package tui

import (
	"context"
	"encoding/json"
	"time"

	tea "charm.land/bubbletea/v2"

	"signal-headless/internal/model"
	"signal-headless/internal/rpc"
)

// Messages produced by RPC commands and daemon notifications.
type (
	statusMsg   rpc.StatusResult
	threadsMsg  []model.Thread
	contactsMsg []model.Contact
	groupsMsg   []model.GroupInfo
	messagesMsg struct {
		thread model.ThreadID
		before int64
		msgs   []*model.Message
	}
	sentMsg struct {
		thread model.ThreadID
		err    error
	}
	resolvedMsg struct {
		res rpc.ResolveResult
		err error
	}
	searchMsg struct {
		thread model.ThreadID
		query  string
		msgs   []*model.Message
		err    error
	}
	errMsg          struct{ err error }
	flashMsg        string
	notificationMsg rpc.Notification
	disconnectedMsg struct{ err error }
	tickMsg         time.Time
	editorDoneMsg   struct {
		thread model.ThreadID
		path   string
		err    error
	}
)

const rpcTimeout = 2 * time.Minute

func (m *Model) call(method string, params, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	return m.cli.Call(ctx, method, params, out)
}

func (m *Model) subscribeCmd() tea.Cmd {
	return func() tea.Msg {
		var st rpc.StatusResult
		if err := m.call(rpc.MSubscribe, nil, &st); err != nil {
			return errMsg{err}
		}
		return statusMsg(st)
	}
}

func (m *Model) loadThreadsCmd() tea.Cmd {
	return func() tea.Msg {
		var ts []model.Thread
		if err := m.call(rpc.MListThreads, nil, &ts); err != nil {
			return errMsg{err}
		}
		return threadsMsg(ts)
	}
}

func (m *Model) loadContactsCmd() tea.Cmd {
	return tea.Batch(
		func() tea.Msg {
			var cs []model.Contact
			if err := m.call(rpc.MListContacts, nil, &cs); err != nil {
				return nil // completion only; not fatal
			}
			return contactsMsg(cs)
		},
		func() tea.Msg {
			var gs []model.GroupInfo
			if err := m.call(rpc.MListGroups, nil, &gs); err != nil {
				return nil
			}
			return groupsMsg(gs)
		},
	)
}

const pageSize = 200

func (m *Model) loadMessagesCmd(thread model.ThreadID, before int64) tea.Cmd {
	return func() tea.Msg {
		var msgs []*model.Message
		if err := m.call(rpc.MGetMessages, rpc.GetMessagesParams{Thread: thread, Before: before, Limit: pageSize}, &msgs); err != nil {
			return errMsg{err}
		}
		return messagesMsg{thread: thread, before: before, msgs: msgs}
	}
}

func (m *Model) markReadCmd(thread model.ThreadID) tea.Cmd {
	return func() tea.Msg {
		if err := m.call(rpc.MMarkRead, rpc.ThreadParams{Thread: thread}, nil); err != nil {
			return errMsg{err}
		}
		return nil
	}
}

func (m *Model) sendCmd(p rpc.SendParams) tea.Cmd {
	return func() tea.Msg {
		err := m.call(rpc.MSend, p, nil)
		return sentMsg{thread: p.Thread, err: err}
	}
}

func (m *Model) typingCmd(thread model.ThreadID, typing bool) tea.Cmd {
	return func() tea.Msg {
		_ = m.call(rpc.MSendTyping, rpc.TypingParams{Thread: thread, Typing: typing}, nil)
		return nil
	}
}

func (m *Model) reactCmd(thread model.ThreadID, target *model.Message, emoji string, remove bool) tea.Cmd {
	return func() tea.Msg {
		err := m.call(rpc.MSendReaction, rpc.ReactionParams{
			Thread: thread, Target: model.MessageRef{Author: target.Author, TS: target.TS}, Emoji: emoji, Remove: remove,
		}, nil)
		if err != nil {
			return errMsg{err}
		}
		return nil
	}
}

func (m *Model) deleteCmd(thread model.ThreadID, ts int64) tea.Cmd {
	return func() tea.Msg {
		if err := m.call(rpc.MRemoteDelete, rpc.DeleteParams{Thread: thread, TargetTS: ts}, nil); err != nil {
			return errMsg{err}
		}
		return flashMsg("deleted for everyone")
	}
}

func (m *Model) archiveCmd(thread model.ThreadID, archived bool) tea.Cmd {
	return func() tea.Msg {
		if err := m.call(rpc.MArchive, rpc.ArchiveParams{Thread: thread, Archived: archived}, nil); err != nil {
			return errMsg{err}
		}
		if archived {
			return flashMsg("archived (shown again when a message arrives; :archived to list)")
		}
		return flashMsg("unarchived")
	}
}

func (m *Model) retryCmd(id int64) tea.Cmd {
	return func() tea.Msg {
		if err := m.call(rpc.MRetry, rpc.RetryParams{MessageID: id}, nil); err != nil {
			return errMsg{err}
		}
		return flashMsg("retrying attachment download")
	}
}

func (m *Model) resolveCmd(recipient string) tea.Cmd {
	return func() tea.Msg {
		var res rpc.ResolveResult
		err := m.call(rpc.MResolve, rpc.ResolveParams{Recipient: recipient}, &res)
		return resolvedMsg{res: res, err: err}
	}
}

func (m *Model) searchCmd(thread model.ThreadID, query string) tea.Cmd {
	return func() tea.Msg {
		var msgs []*model.Message
		err := m.call(rpc.MSearch, rpc.SearchParams{Thread: thread, Query: query, Limit: 500}, &msgs)
		return searchMsg{thread: thread, query: query, msgs: msgs, err: err}
	}
}

// listen waits for the next daemon notification.
func (m *Model) listen() tea.Cmd {
	return func() tea.Msg {
		n, ok := <-m.cli.Notifications()
		if !ok {
			return disconnectedMsg{m.cli.Err()}
		}
		return notificationMsg(n)
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func decodeInto[T any](raw json.RawMessage) (T, bool) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, false
	}
	return v, true
}
