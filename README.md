# signal-headless

A headless Signal client in one Go binary — no Java, no Docker, no signal-cli.

```
signal-headless --link                 link this host as a Signal device (QR code)
signal-headless --import-signal-cli    adopt the device signal-cli already linked
signal-headless --daemon               run the device: receive, store, serve clients
signal-headless --shell                interactive TUI (default; starts the daemon if needed)
signal-headless --send TO -m TEXT [-a FILE]...
signal-headless --status
```

The Signal protocol comes from Signal's own Rust
[libsignal](https://github.com/signalapp/libsignal), statically linked through
[signalmeow](https://github.com/mautrix/signal/tree/main/pkg/signalmeow) (the
Go client library behind the mautrix Signal bridge). The toolchain is only
needed to build; the binary depends on nothing but libc/libstdc++.

## Architecture

```
Signal servers ⇄ daemon ── websocket, device keys, message history (SQLite)
                   │ unix socket: JSON-RPC 2.0, newline-delimited
      ┌────────────┼──────────────┬──────────────────┐
   --shell     --send/--status  signal_agent       anything else
   (TUI)                        (via `jsonRpc`)    (VS Code notifier, scripts)
```

A linked device has one set of keys and one server-side message queue, so
exactly one process may talk to Signal: the daemon. Everything else is a
client. Messages arrive and are stored whether or not a client is open;
Signal servers keep no history, so the daemon's database is the only record.

`--shell` starts the daemon in the background (tmux-style) when it isn't
running; for an always-on device use the systemd unit below.

## Build

Build dependencies (Debian/Ubuntu):

```bash
sudo apt install clang libclang-dev cmake make build-essential protobuf-compiler libprotobuf-dev
curl -sSf https://sh.rustup.rs | sh -s -- -y --profile minimal
```

```bash
make            # first run clones and builds libsignal (~5 min), then the binary
make test
make install    # → ~/.local/bin/signal-headless
```

`LIBSIGNAL_REV` in the Makefile must match the libsignal submodule commit of
the `go.mau.fi/mautrix-signal` version in `go.mod`; bump both together.

## Setup

Either link a new device:

```bash
signal-headless --link --name "my-host"
# Phone: Signal → Settings → Linked devices → Link new device → scan
```

…or take over the device signal-cli already linked (keeps the same device
slot; no QR scan):

```bash
signal-headless --import-signal-cli --dry-run   # validate, writes nothing
systemctl --user stop signal_agent.service      # nothing may use signal-cli now
signal-headless --import-signal-cli
```

The import only reads signal-cli's files. **Afterwards signal-cli must never run
for that account again** — two clients on one identity split the message
queue and corrupt each other's sessions. The daemon refuses to start while a
signal-cli process exists and shuts down if one appears. Disable anything that
would start it (e.g. `systemctl --user disable signal_agent.service` unless it
is switched over as described below).

On first start the daemon prunes session records that hold no current state
(signal-cli keeps these for reset/unlinked devices; they would otherwise block
sending, including the sync copies of our own messages to the phone).

## Running the daemon

```bash
cp signal-headless.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now signal-headless.service
loginctl enable-linger "$USER"      # keep running without a login session
journalctl --user -u signal-headless -f
```

State lives in `~/.local/share/signal-headless/` (database, `attachments/`,
`daemon.log` when auto-started). `XDG_DATA_HOME` is deliberately ignored:
confined terminals (the VS Code snap) point it at a private directory, which
would split the daemon and its clients across two stores. Override with
`--data DIR` / `SIGNAL_HEADLESS_DATA`. The socket is
`$XDG_RUNTIME_DIR/signal-headless.sock` (`--socket` / `SIGNAL_HEADLESS_SOCKET`).

The daemon honors the account's read-receipt and typing-indicator settings
(synced from Signal's storage service), follows each conversation's
disappearing-messages timer — outgoing messages carry it, and expired messages
and their files are deleted locally — and downloads attachments in the
background (three attempts; `R` in the shell retries).

## The shell

aerc-style: threads on the left, the conversation on the right, a compose box
at the bottom. Drafts, reply targets and attachments are kept per thread, so
switching conversations mid-sentence is free.

| Keys | |
|---|---|
| `J` `K`, `ctrl+n` `ctrl+p` | next / previous thread (also while composing) |
| `n`, `tab` | next thread with unread messages |
| `C`, `m`, `:to NAME` | new conversation (tab completes contacts and groups) |
| `j` `k`, `g` `G`, `ctrl+u` `ctrl+d` | select messages, oldest/newest, page |
| `enter`, `i` | write in this thread; `enter` sends, `alt+enter`/`ctrl+j` newline |
| `ctrl+e` | edit the draft in `$EDITOR` |
| `:joy:` … | shortcodes in sent messages and reactions become emoji (😂); unknown codes and `12:30:45` stay as typed |
| `a`, `ctrl+t` | attach a file (tab completes paths); `A` clears |
| `r` | reply to the selected message (quote) |
| `e`, `+` | react — `1`–`6` for 👍 ❤️ 😂 😮 😢 🙏, or type any emoji; empty removes |
| `D` | delete your selected message for everyone |
| `o` | open the selected message's attachments (`xdg-open`) |
| `y` | copy the message text (OSC 52 clipboard) |
| `/` | search this thread; `/` `enter` again for the next match |
| `:` | commands: `to`, `attach`, `detach`, `archive`, `unarchive`, `archived`, `read`, `search`, `react`, `retry`, `open`, `help`, `quit` |
| `?` | help |
| `q` | quit — confirm with `q` or `enter`, any other key cancels (`ctrl+c` quits at once) |

Opening a thread marks it read (and sends read receipts if enabled). The
terminal bell rings for messages in other threads; `SIGNAL_HEADLESS_BELL=0`
silences it.

## Client protocol

Newline-delimited JSON-RPC 2.0 on the unix socket, the same framing as
`signal-cli jsonRpc`.

**signal-cli compatibility.** A fresh connection behaves like signal-cli:
incoming and synced messages arrive as `receive` notifications with
signal-cli's envelope shape, sent only after attachments are downloaded, with
`attachments[].id` naming a file in `~/.local/share/signal-headless/attachments/`.
`send` (`recipient`, `groupId`, `message`, `attachments`, `quoteTimestamp`,
`quoteAuthor`), `sendReaction` and `remoteDelete` accept signal-cli's
parameters. Invoked as `signal-headless … jsonRpc` (signal-cli flags are
ignored) the binary bridges stdio to the daemon, starting it if needed.

That makes it a drop-in for signal_agent — in `~/.config/signal_agent.env`:

```bash
SIGNAL_CLI=/home/USER/.local/bin/signal-headless
SIGNAL_CONFIG=/home/USER/.local/share/signal-headless
```

**Native API.** Call `subscribe` to switch a connection to native events:
`message`, `messageUpdate` (edits, deletes, reactions, receipts, attachment
progress), `thread` (unread counts, titles), `typing`, `connection`,
`contacts`. Methods: `status`, `listThreads`, `getThread`, `getMessages`
(`thread`, `before`, `limit`), `search`, `send` (`thread` or `to`, `body`,
`attachments`, `quote`), `sendReaction`, `remoteDelete`, `sendTyping`,
`markRead`, `archiveThread`, `listContacts`, `listGroups`, `resolve`,
`retryAttachment`. Types are in `internal/model` and `internal/rpc/api.go`.

A notifier (e.g. a VS Code extension) needs only: connect, `subscribe`, show
`message` events where `outgoing` is false.

```bash
# quick look from a shell
echo '{"jsonrpc":"2.0","id":1,"method":"listThreads"}' | signal-headless jsonRpc
```

## Development

`signal-headless --daemon --fake` (or `--shell --fake`) runs against an
in-memory backend with seeded conversations and an echo contact, in a scratch
directory under `/tmp` — nothing reaches Signal. Tests use the same fake.

```
main.go, *cmd.go        flags and commands
internal/signalbackend  signalmeow adapter (the only code that talks to Signal)
internal/fakebackend    test/development backend
internal/daemon         event persistence, RPC dispatch, signal-cli compat
internal/history        threads/messages/attachments/reactions (SQLite)
internal/importer       signal-cli device import
internal/rpc            JSON-RPC server/client
internal/tui            the shell (bubbletea v2)
```

## Security notes

- The data directory holds the device's private keys and all message history
  (mode 0700; the socket is 0600). Treat it like `~/.ssh`.
- Any local process of the same user can use the socket to read and send
  messages as the account.
- If the host is compromised, unlink it from the phone (Settings → Linked
  devices).

## License

signalmeow and libsignal are AGPL-3.0; a binary built from this source is
therefore covered by the AGPL-3.0.
