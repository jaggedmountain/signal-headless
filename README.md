<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/signal-headless-lockup-dark.svg">
    <img alt="signal-headless" src="docs/signal-headless-lockup-light.svg" width="480">
  </picture>
</p>

Signal on a computer without Signal Desktop: one Go binary that links as a
Signal device and keeps running in the background. A terminal chat client
and a VS Code extension connect to it. No Java, no Docker, no signal-cli.

- **A real linked device.** It links like Signal Desktop (scan a QR code),
  and can bring over the phone's message history.
- **Always receiving.** A small daemon holds the connection and stores
  everything in SQLite, whether or not a client is open.
- **A keyboard-driven terminal UI** (`--shell`): conversations on the left,
  per-conversation drafts, replies, reactions, attachments, search, link
  previews, emoji shortcodes.
- **A VS Code extension** ([signal-headless-vscode](https://github.com/jaggedmountain/signal-headless-vscode)): notifications that appear once across
  windows, a conversations view, chat tabs, search results, diagnostics.
  It also works in Remote-SSH windows.
- **Scriptable.** `--send`, `--status`, and a JSON-RPC socket that is also a
  drop-in replacement for `signal-cli jsonRpc`.

The Signal protocol is Signal's own
[libsignal](https://github.com/signalapp/libsignal) (Rust), statically
linked through [signalmeow](https://github.com/mautrix/signal/tree/main/pkg/signalmeow),
the Go library behind the mautrix Signal bridge.

**Contents:** [Quick start](#quick-start) · [How it works](#how-it-works) ·
[Building](#building) · [Linking](#linking) · [Running the daemon](#running-the-daemon) ·
[What the daemon does](#what-the-daemon-does) · [The shell](#the-shell) ·
[VS Code](#vs-code) · [Command line](#command-line) · [Client protocol](#client-protocol) ·
[Troubleshooting](#troubleshooting) · [Security and privacy](#security-and-privacy) ·
[Platforms](#platforms) · [Development](#development) · [License](#license)

## Quick start

```bash
curl -fsSL https://github.com/jaggedmountain/signal-headless/releases/latest/download/install.sh | sh
signal-headless --link               # scan the QR code: phone → Settings → Linked devices → Link new device
signal-headless                      # the terminal UI; starts the daemon in the background
```

On Windows (experimental), in PowerShell:

```powershell
irm https://github.com/jaggedmountain/signal-headless/releases/latest/download/install.ps1 | iex
```

It installs to `%LOCALAPPDATA%\Programs\signal-headless` and adds that to
the user PATH; no administrator rights needed.

The installer downloads the release for this platform (Linux x86-64; Linux
arm64 and macOS are experimental), checks it against the release's `SHA256SUMS`, and puts it in
`~/.local/bin`. `… | sh -s -- --systemd` also sets up the systemd user
service (Linux); `--version v0.1.0` pins a release. Or build from source (below).

With the [VS Code extension](https://github.com/jaggedmountain/signal-headless-vscode),
none of this is needed: it finds this install or downloads the matching
release itself, and links with a QR code panel.

## How it works

```
Signal servers ⇄ daemon ── device keys, message history, attachments (SQLite + files)
                   │ unix socket: JSON-RPC 2.0, one JSON object per line
     ┌─────────────┼───────────────┬───────────────┬──────────────┐
  --shell       VS Code         --send          signal_agent    scripts
  (terminal)    extension       --status        (jsonRpc bridge)
```

A linked device has one set of keys and one server-side message queue, so
exactly one process may talk to Signal: the daemon. Everything else is a
client, and any number can connect at once. Signal's servers keep no
message history; apart from the phone's one-time transfer at linking, the
daemon's database is the only record on this computer.

The daemon is started on demand (`--shell`, `--send` and the VS Code
extension start it in the background when it isn't running), or kept running
with the systemd unit below. A lock on the data directory ensures one daemon
per account, whoever starts it.

## Building

Build dependencies (Debian/Ubuntu):

```bash
sudo apt install clang libclang-dev cmake make build-essential protobuf-compiler libprotobuf-dev
curl -sSf https://sh.rustup.rs | sh -s -- -y --profile minimal
```

```bash
make            # first run clones and builds libsignal (~5 min), then bin/signal-headless
make test
make install    # → ~/.local/bin/signal-headless
make release-local   # portable build in an Ubuntu 22.04 container (glibc ≥ 2.34) + release assets → dist/release/
```

A binary built with `make` needs the build machine's glibc or newer.
Releases are built by GitHub Actions when a `v*` tag is pushed
(`.github/workflows/release.yml`), natively per platform:
`signal-headless-{linux,darwin}-{x64,arm64}.tar.gz`,
`signal-headless-windows-x64.tar.gz`, `install.sh`, `install.ps1` and
`SHA256SUMS`, with version-less names so `releases/latest/download/…` always
works. `make release-local` produces the Linux x86-64 assets with Docker.
`make` also builds on macOS (Xcode command line tools, Rust, Go, `brew
install protobuf`). `LIBSIGNAL_REV` in the Makefile must match the
libsignal version used by the `go.mau.fi/mautrix-signal` release in
`go.mod`; bump both together.

## Linking

```bash
signal-headless --link --name "my-laptop"
```

Scan the code on the phone (Settings → Linked devices → Link new device).
The phone then offers **Transfer message history**. If chosen, it uploads an
encrypted archive, which the daemon downloads and imports on its first start
(progress shows in `--status` clients, the VS Code status bar and the log).
Imported messages are stored quietly: no notifications, no signal-cli events.
Attachments from the last 45 days are fetched in the background while
Signal still holds them; older ones are listed with a note and can be
retried. Deleted, expired and view-once content isn't imported.

**Taking over a signal-cli device** (keeps its device slot, no QR code):

```bash
signal-headless --import-signal-cli --dry-run   # validate; writes nothing
systemctl --user stop signal_agent.service      # nothing may use signal-cli now
signal-headless --import-signal-cli
```

The import only reads signal-cli's files. Afterwards **signal-cli must never
run for that account again**: two clients on one identity split the message
queue and break each other's sessions. The daemon refuses to start while a
signal-cli process exists and stops if one appears. On first start it also
prunes stale session records signal-cli leaves behind; they would otherwise
block sending.

**Unlinking:** `signal-headless --unlink` removes the device from the account
(like Linked devices → Unlink on the phone), deletes its keys and stops the
daemon. It asks for the account number first. Message history is kept, so
linking again doesn't lose it. If the phone already removed the device,
`--unlink --force` just deletes the local keys (with the daemon stopped).

## Running the daemon

For an always-on device:

```bash
cp signal-headless.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now signal-headless.service
loginctl enable-linger "$USER"      # keep running without a login session
journalctl --user -u signal-headless -f
```

The VS Code extension starts the daemon through this unit when it's
installed.

| Where | What |
|---|---|
| `~/.local/share/signal-headless/signal-headless.db` | keys, sessions, contacts, message history |
| `~/.local/share/signal-headless/attachments/` | downloaded attachments, link-preview images |
| `~/.local/share/signal-headless/daemon.log` | log when started in the background |
| `$XDG_RUNTIME_DIR/signal-headless.sock` | the client socket (mode 0600) |

`XDG_DATA_HOME` is deliberately ignored. Confined terminals, like the VS Code
snap's, point it at a private directory, which would split the daemon and
its clients across two stores.

| Option | Environment | |
|---|---|---|
| `--data DIR` | `SIGNAL_HEADLESS_DATA` | data directory |
| `--socket PATH` | `SIGNAL_HEADLESS_SOCKET` | client socket |
| `--deleted-ttl 1h` | | how long "This message was deleted." placeholders stay |
| `--foreground=false` | | log to `daemon.log` instead of stderr |
| `-v` | | debug logging |
| | `SIGNAL_HEADLESS_BELL=0` | shell: no terminal bell for other conversations |
| | `SIGNAL_HEADLESS_LINK_PREVIEWS=on\|off` | shell: override the account's link-preview setting |

## What the daemon does

- **Receives and stores** messages, edits, reactions, receipts, typing,
  stickers, quotes and mentions. Calls, polls, payments and shared contacts
  appear as short notes ("answer on the phone").
- **Follows the account's settings** from Signal's storage service: read
  receipts, typing indicators, link previews.
- **Disappearing messages:** outgoing messages carry the conversation's
  timer. Expired messages and their files are deleted locally.
- **Attachments** download in the background, newest first, so live media
  never waits behind a history backlog. Three tries each; clients can retry.
- **Deletions:** "delete for everyone" leaves a placeholder for an hour, then
  it's removed, and replies quoting it lose the quoted text. "Delete for
  me" from the phone removes messages or whole conversations here too.
- **Link previews:** previews the sender's app attached are kept. Pages of
  incoming links are never fetched. For outgoing messages, clients can ask for a
  preview, which the daemon fetches like Signal's apps do (sender side):
  https only, public addresses only (checked when connecting), at most 3
  redirects, 512 KB of page and a 2 MB image.
- **Maintenance:** storage statistics, and purging history older than a date,
  on this computer only or (opt-in) on all the account's devices via
  Signal's "delete for me" sync.

## The shell

`signal-headless` (or `--shell`), aerc-style: conversations on the left, the
open one on the right, a compose line at the bottom. Drafts, reply targets and
attachments are kept per conversation, so switching mid-sentence is free.

| Keys | |
|---|---|
| `J` `K`, `ctrl+n` `ctrl+p` | next / previous conversation (also while writing) |
| `n`, `tab` | next conversation with unread messages |
| `C`, `m`, `:to NAME` | new conversation (tab completes contacts and groups) |
| `j` `k`, `g` `G`, `ctrl+u` `ctrl+d` | select messages, oldest / newest, page |
| `enter`, `i` | write here; `enter` sends, `alt+enter` / `ctrl+j` newline |
| `ctrl+e` | edit the draft in `$EDITOR` |
| `a`, `ctrl+t` | attach a file (tab completes paths); `A` clears attachments |
| `x`, `u` | clear the draft (text and reply target); `u` brings it back |
| `ctrl+x` | send without the link preview shown above the compose line |
| `r` | reply to the selected message (quote) |
| `e`, `+` | react — `1`–`6` for 👍 ❤️ 😂 😮 😢 🙏, or any emoji; empty removes |
| `D` | delete the selected own message for everyone |
| `o` | open the selected message's attachments |
| `y` | copy the message text (OSC 52 clipboard) |
| `/` | search this conversation; `/` `enter` again for the next match |
| `:` | commands: `to`, `attach`, `detach`, `clear`, `archive`, `unarchive`, `archived`, `read`, `search`, `react`, `retry`, `open`, `help`, `quit` |
| `?` | help |
| `q` | quit (confirm with `q` or `enter`; `ctrl+c` quits at once) |

`:joy:`-style shortcodes in messages and reactions become emoji (😂); unknown
codes and times like `12:30:45` stay as typed. A moment after an https link
is typed, `🔗 title — site` shows the preview that will be sent. Received
previews show the same way. Opening a conversation marks it read (sending
read receipts if enabled). The terminal bell rings for messages in other
conversations.

## VS Code

The [VS Code extension](https://github.com/jaggedmountain/signal-headless-vscode)
(`jaggedmountain.signal-headless`, its own repository) is a full client:
- notifications that appear once even with several windows open
  (desktop notifications when VS Code isn't focused)
- an unread count in the status bar
- a conversations view (active or all), chat tabs, and a Search Results panel
- a Diagnostics view with purge
- link previews in both directions
- linking from a QR code panel, with the history transfer shown as it runs

The extension runs on the local side of Remote-SSH windows. It uses an
installed `signal-headless` when it is new enough, else downloads the
release it was built for (checksum-verified) into its own storage.

## Command line

```
signal-headless                        the terminal UI (same as --shell)
signal-headless --link [--name N]      link this computer (QR code); --json for machine-readable steps
signal-headless --unlink [--force]     remove this device (history is kept)
signal-headless --import-signal-cli    adopt signal-cli's device [--dry-run] [--account N] [--signal-cli-dir D]
signal-headless --daemon               run the device in the foreground
signal-headless --send TO -m TEXT [-a FILE]...
                                       TO: +number, contact or group name, UUID, group id, or "self"
signal-headless --status               account, connection, clients, daemon version
signal-headless --check [--json]       linked? (exit status 3 if not)
signal-headless --stop                 stop the daemon (clients start it again when needed)
signal-headless --version [--json]     version (and, with --json, the protocol version)
signal-headless … jsonRpc              signal-cli-compatible stdio bridge (see below)
```

## Client protocol

JSON-RPC 2.0 on the unix socket, one JSON object per line: the same framing
as `signal-cli jsonRpc`.

**signal-cli compatibility.** A new connection behaves like signal-cli:
incoming and synced messages arrive as `receive` notifications in
signal-cli's envelope format, once their attachments are downloaded, with
`attachments[].id` naming a file in the attachments directory. `send`
(`recipient`, `groupId`, `message`, `attachments`, `quoteTimestamp`,
`quoteAuthor`), `sendReaction` and `remoteDelete` accept signal-cli's
parameters. Run as `signal-headless … jsonRpc` (signal-cli's other flags are
ignored), the binary connects stdin/stdout to the daemon, starting it if
needed. That makes it a drop-in for tools like signal_agent:

```bash
SIGNAL_CLI=$HOME/.local/bin/signal-headless
SIGNAL_CONFIG=$HOME/.local/share/signal-headless
```

**Native API.** After `subscribe`, a connection gets native events instead:

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

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"listThreads"}' | signal-headless jsonRpc
```

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `no linked Signal device` (exit 3) | Link with `--link` (or the VS Code extension). |
| `signal-cli is running …` | signal-cli uses the same identity; stop it (`systemctl --user stop signal_agent.service`) and keep it stopped. |
| `another daemon is using …` | A daemon is already running; clients connect to it. `signal-headless --stop` stops it. |
| Status shows `logged-out` | The phone removed this device. `signal-headless --unlink --force` (daemon stopped), then `--link`. |
| Attachments "not downloaded" | Old or expired media, or the transfer didn't include it. Retry (`R`, or Diagnostics in VS Code); what Signal no longer holds stays on the phone. |
| A new feature doesn't appear | The daemon keeps running across updates; `signal-headless --stop` once and it restarts on the new version (daemons older than `--stop`: `pkill -x signal-headless`). |

Logs: `daemon.log` in the data directory (background daemon), `journalctl --user
-u signal-headless` (systemd), or run `signal-headless --daemon -v` in a
terminal.

## Security and privacy

- The data directory holds the device's private keys and all message history
  (mode 0700; the socket is 0600). Treat it like `~/.ssh`.
- Any process running as the same user can use the socket to read and send
  messages as the account.
- The daemon fetches web pages only for links in messages *we* send, when a
  client asks for a preview (see [What the daemon does](#what-the-daemon-does)).
- If the computer is compromised, unlink it from the phone (Settings → Linked
  devices).

## Platforms

Linux x86-64 is what runs today. Releases also build Linux arm64, macOS
(Apple Silicon and Intel) and Windows x86-64. Those are experimental: each
is built, unit-tested and smoke-tested (a fake daemon started and stopped)
on its own CI runner, but not yet run against a real account. See
[docs/portability.md](docs/portability.md).

## Development

`--fake` runs everything against an in-memory backend with seeded
conversations, an echo contact and a simulated history transfer, in a
scratch directory under `/tmp`. Nothing reaches Signal: `--daemon --fake`,
`--shell --fake`, `--link --fake [--json]`. The Go tests use the same fake;
the extension's tests run against a `--fake`
daemon, including a real VS Code instance under xvfb (in the extension's repository).

```
main.go, *cmd.go, link.go, check.go   flags and commands
internal/signalbackend   signalmeow adapter: the only code that talks to Signal
internal/fakebackend     test and development backend
internal/daemon          event handling, RPC dispatch, signal-cli compatibility
internal/history         conversations, messages, attachments, reactions, previews (SQLite)
internal/linkpreview     outgoing link-preview fetcher
internal/importer        signal-cli device import
internal/rpc             JSON-RPC server and client, API types
internal/tui             the terminal UI (bubbletea v2)
internal/paths           data directory and socket locations
build/                   portable build (Docker), release packaging, install.sh, install.ps1
.github/workflows/       CI (tests) and releases (on v* tags)
```

## License

Copyright © 2026 Jeff Mattson

signal-headless, the daemon and terminal UI, is free software under the **GNU Affero General Public License,
version 3 or (at your option) any later version** (`AGPL-3.0-or-later`); see
[LICENSE](LICENSE). The binary statically links Signal's
[libsignal](https://github.com/signalapp/libsignal) and
[signalmeow](https://github.com/mautrix/signal), both AGPL-3.0, so the same
terms apply to it as a whole. Other Go dependencies keep their own
(BSD/MIT/Apache-2.0/MPL-2.0) licenses.

Under the AGPL, anyone who receives the binary is entitled to its source
(each release links its tag), and so is anyone who uses a modified version
over a network. The VS Code extension is licensed the same way.

The AGPL covers the code, not the signal-headless logo. The logo images
(`docs/signal-headless-lockup-*.svg`) are © 2026 Jeff Mattson, all rights
reserved, except that they may be shown unmodified to refer to this project.
A modified version or fork doesn't get to use them; please give it its own
name and logo.
