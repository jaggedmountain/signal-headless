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
| `message` | a new message (incoming, or sent from any of our devices) |
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
