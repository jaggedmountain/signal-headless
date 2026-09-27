# Developing signal-headless

The user-facing overview is in [README.md](README.md); the client protocol
is in [docs/protocol.md](docs/protocol.md), platform notes in
[docs/portability.md](docs/portability.md).

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
by the systemd unit. A lock on the data directory ensures one daemon per
account, whoever starts it.

The Signal protocol is Signal's own
[libsignal](https://github.com/signalapp/libsignal) (Rust), statically
linked through [signalmeow](https://github.com/mautrix/signal/tree/main/pkg/signalmeow),
the Go library behind the mautrix Signal bridge.

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

A binary built with `make` needs the build machine's glibc or newer. `make`
also builds on macOS (Xcode command line tools, Rust, Go, `brew install
protobuf`); Windows builds use MSYS2, see
[docs/portability.md](docs/portability.md). `LIBSIGNAL_REV` in the Makefile
must match the libsignal version used by the `go.mau.fi/mautrix-signal`
release in `go.mod`; bump both together.

## Testing

`--fake` runs everything against an in-memory backend with seeded
conversations, an echo contact and a simulated history transfer, in a
scratch directory under `/tmp`. Nothing reaches Signal: `--daemon --fake`,
`--shell --fake`, `--link --fake [--json]`. The Go tests use the same fake;
the VS Code extension's tests run against a `--fake` daemon, including a
real VS Code instance under xvfb (in the extension's repository).

CI (`.github/workflows/ci.yml`) builds and tests on Linux, macOS and Windows,
and on each runs `build/smoke.sh` (a fake daemon started, queried over its
socket and stopped) and the installers against a local mirror
(`build/test-install.sh`, `install.ps1` in both PowerShell versions).
`build/test-install.sh` uninstalls with the default data directory, so it
refuses to run outside CI.

## Screenshot

`docs/tui.png`, in the README, comes from `build/screenshot.sh`: the terminal
UI on a fake daemon (own scratch data and socket) with the demo data set
(`SIGNAL_HEADLESS_DEMO=1`, `internal/fakebackend/demo.go`), captured from
tmux and rendered by `build/screenshot.py` (Pillow, DejaVu Sans Mono, Noto
Color Emoji). It refuses to write the image unless the capture shows the
fake account.

## Releases

Pushing a `v*` tag runs `.github/workflows/release.yml`: each platform builds
natively on its own runner, tests, smoke-tests and packages, then one job
publishes `signal-headless-{linux,darwin}-{x64,arm64}.tar.gz`,
`signal-headless-windows-x64.tar.gz`, `install.sh`, `install.ps1` and
`SHA256SUMS`. Names carry no version, so `releases/latest/download/…` always
works. Tags with a `-` (e.g. `v0.2.0-rc.1`) become pre-releases and never
"latest". A failure on any platform stops the release.

The installers write `update.sh`/`update.ps1` and `uninstall.sh`/`uninstall.ps1`
with the install's options; `--update` and `--uninstall` find them next to
the binary (`uninstall.go`, `platform_*.go`).

## Code layout

```
main.go, *cmd.go, link.go, check.go   flags and commands
uninstall.go, platform_*.go           --update/--uninstall, per-OS process handling
internal/signalbackend   signalmeow adapter: the only code that talks to Signal
internal/fakebackend     test and development backend
internal/daemon          event handling, RPC dispatch, signal-cli compatibility
internal/history         conversations, messages, attachments, reactions, previews (SQLite)
internal/linkpreview     outgoing link-preview fetcher
internal/importer        signal-cli device import
internal/rpc             JSON-RPC server and client, API types
internal/tui             the terminal UI (bubbletea v2)
internal/paths           data directory and socket locations
build/                   portable build (Docker), packaging, installers, smoke and install tests
.github/workflows/       CI (tests) and releases (on v* tags)
```

Source files carry SPDX headers (`AGPL-3.0-or-later`); see the License
section of the README.
