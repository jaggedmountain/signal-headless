# Client protocol

JSON-RPC 2.0 on the daemon's unix socket, one JSON object per line: the same
framing as `signal-cli jsonRpc`. The socket is `$XDG_RUNTIME_DIR/signal-headless.sock`
on Linux (in the data directory elsewhere, or `--socket`/`SIGNAL_HEADLESS_SOCKET`).

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"listThreads"}' | signal-headless jsonRpc
```

## signal-cli compatibility

A new connection behaves like signal-cli: incoming and synced messages
arrive as `receive` notifications in signal-cli's envelope format, once
their attachments are downloaded, with `attachments[].id` naming a file in
the attachments directory. `send` (`recipient`, `groupId`, `message`,
`attachments`, `quoteTimestamp`, `quoteAuthor`), `sendReaction` and
`remoteDelete` accept signal-cli's parameters. Run as
`signal-headless … jsonRpc` (signal-cli's other flags are ignored), the
binary connects stdin/stdout to the daemon, starting it if needed. That
makes it a drop-in for tools like signal_agent:

```bash
SIGNAL_CLI=$HOME/.local/bin/signal-headless
SIGNAL_CONFIG=$HOME/.local/share/signal-headless
```

## Native API

After `subscribe`, a connection gets native events instead:

| Event | |
|---|---|
| `message` | a new message (incoming, or sent from any of our devices; `localOrigin` marks the ones this daemon sent) |
| `messageUpdate` | edits, deletions, reactions, receipts, attachment progress |
| `messageRemoved` | a message is gone for good (placeholder expired, delete-for-me) |
| `thread` | a conversation changed (unread count, title, timer) |
| `history` | a conversation's history was imported or purged: reload it |
| `typing`, `connection`, `contacts` | typing indicators; status changes; contact list changed |

| Method | Parameters → result |
|---|---|
| `status`, `version` | → account, connection, clients, history transfer, link-preview setting |
| `listThreads`, `getThread` | `thread` → conversations |
| `getMessages` | `thread`, `before`, `limit` → the newest `limit` messages before `before`, oldest first |
| `search` | `query`, `thread`?, `limit` → messages |
| `send` | `thread` or `to`, `body`, `attachments`, `quote`, `previews` → message |
| `linkPreview` | `url` → preview (fetched; pass it back in `send`) |
| `sendReaction`, `remoteDelete`, `sendTyping`, `markRead` | reactions, delete for everyone, typing, read |
| `archiveThread` | `thread`, `archived` |
| `listContacts`, `listGroups`, `resolve` | contacts; groups; name/number → conversation |
| `retryAttachment`, `retryFailedAttachments` | `messageId`; everything that failed |
| `stats` | → counts, database and attachment sizes |
| `purge` | `before`, `thread`?, `dryRun`, `allDevices` → what was (or would be) deleted |
| `unlink` | `number` (the account's, as confirmation) |
| `shutdown` | stop the daemon (e.g. after an upgrade; clients restart it) |

`status` includes `protocol`, the API version (`rpc.ProtocolVersion`, now
1; 0 or missing from older daemons). The daemon keeps running across
upgrades, so clients should check it and ask for a restart (`shutdown`) when
it is too old for them.

`purge` deletes this computer's copy only, unless `allDevices` is set. Then it
first sends the "delete for me" sync to the account's other devices, and
deletes nothing if that fails. Types are in `internal/model` and
`internal/rpc/api.go`.

## Messages

- `id` identifies a message on this computer. IDs only grow and are never
  reused, even after a message is deleted, so they work as a cursor.
- `ts` is the sender's timestamp (ms): Signal's identity for the message,
  used in quotes and reactions. It isn't ordered by arrival.
- `outgoing` means the account sent it, from any device; `localOrigin` means
  this daemon sent it (a client of it), not the phone or another linked
  device. In Note to Self, `outgoing && !localOrigin` is what was typed on
  another device.
- `attachments[].path` is the local file once `state` is `done`.

## Following a conversation

`signal-headless --watch CHANNEL` does this for scripts; a client of its own
can do the same without gaps:

1. Note the newest `id` in the conversation (`getMessages` with `limit: 1`).
2. `subscribe`.
3. Catch up: page back with `getMessages` until reaching that `id`, and
   handle the newer ones in `id` order.
4. Handle `message` events with a larger `id`; drop the ones already seen.
5. When the connection drops (the daemon restarted), reconnect and repeat
   from 2 with the last `id` handled.

Taking the starting point before subscribing, and catching up after it,
means nothing falls in between; comparing `id`s removes the overlap.
