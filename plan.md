# signal-cli

Goal: a go binary for headless Signal interaction

--link <number> # run device linking protocol
--daemon # run linked device proxy
--shell # interactive tui (like aerc)


Related reference ~/projects/signal-agent; but don't want the java/docker dependency

The aerc command line mail client is decent analogy.
Easy keyboard transition between threads, with seamless response/attach etc.
---

## Decisions (2026-09-24)

- **Stack:** Go + `go.mau.fi/mautrix-signal/pkg/signalmeow` (v0.2609.0), Rust
  `libsignal` (pinned 8fc2113b, built as `libsignal_ffi.a`) statically linked via
  cgo. AGPL-3.0. Build needs Rust/clang/cmake/protoc; runtime needs none of it.
- **Binary:** `signal-headless` (avoid clashing with `signal-cli` on PATH).
- **Architecture:** the daemon is the *only* process that talks to Signal. It owns
  the websocket, device keys (signalmeow store) and message history (own SQLite).
  Clients attach over a unix socket (`$XDG_RUNTIME_DIR/signal-headless.sock`),
  JSON-RPC with pushed events, shaped like signal-cli's `jsonRpc` so
  `signal_agent` can later become a client. `--shell` auto-starts the daemon if
  it isn't running (tmux-style); a systemd user unit is optional.
- **Device:** reuse the device already linked for signal-cli (deviceId 3) by a
  one-way, read-only import of `~/.local/share/signal-cli/data/<acct>` +
  `account.db` (identity keys, password, registration ids, sessions, prekeys,
  sender keys, profile key, AEP). signal-cli data is never modified. `--link`
  is still implemented (fresh QR link) as the fallback.
- **Exclusivity:** signal-cli and our daemon must never run concurrently on the
  same identity. `signal_agent.service` is stopped for live tests and **left
  stopped**.
- **Live-test boundary:** unattended sends go only to Note to Self.
- **VCS:** local git, checkpoint commits at milestones, no remote.

## Milestones

1. Build plumbing: Makefile builds libsignal_ffi.a + cgo binary; `version` works.
2. `--link`: provisioning QR (terminal render), store device in signalmeow DB.
3. `--import-signal-cli`: read-only import of the existing linked device.
4. `--daemon`: connect, receive/decrypt, persist messages/attachments/receipts,
   send text + attachments, contact/group sync; unix-socket JSON-RPC + events.
5. `--shell`: aerc-like TUI client — thread list, message view, compose, reply,
   attach (file picker / path completion), keyboard thread switching, unread
   counts, notifications of new messages.
6. Polish: systemd unit, README, signal-cli-compatible RPC subset for
   signal_agent, tests (fake transport for daemon/shell).

## Status (2026-09-24)

All six milestones are implemented and committed; see README.md for usage.

- [x] 1 Build plumbing (Makefile builds libsignal + binary)
- [x] 2 `--link` — verified against Signal's provisioning server (QR shown; not scanned)
- [x] 3 `--import-signal-cli` — real import done (device 3); 35 archived-only
      sessions pruned on first start; send path verified live (Note to Self)
- [x] 4 `--daemon` — connects live, storage sync, prekeys consistent with server;
      signal-cli compat verified by running signal_agent unmodified against it (fake backend)
- [x] 5 `--shell` — exercised in tmux against fake and live daemons
- [x] 6 systemd unit, README, tests (history, importer, daemon, tui)

Open items:
- Live *receive* path not yet observed (no inbound traffic during the run).
- signal_agent.service is stopped but still **enabled**: it would start
  signal-cli at next login/boot. The daemon guards against this by shutting
  down, but better: disable it, or switch it to signal-headless (README).
- signal-headless.service is provided but not installed/enabled.
- Not supported: calls, stories, polls/payments (shown as placeholders),
  group management, editing sent messages, sticker packs, link previews.
