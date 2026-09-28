# Developing signal-headless

The user-facing overview is in [README.md](README.md); the client protocol
is in [docs/protocol.md](docs/protocol.md).

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
protobuf`); Windows builds use MSYS2, see [Platforms](#platforms).
`LIBSIGNAL_REV` in the Makefile
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

## Platforms

Linux x86-64 and arm64, macOS arm64 and x86-64, and Windows x86-64 each
build natively on a GitHub-hosted runner; nothing is cross-compiled. Linking,
receiving and sending have been tried on real machines for Linux x86-64,
macOS arm64 and Windows; Linux arm64 and Intel Macs rely on CI so far.

**Linux:** Ubuntu 22.04 runners keep the glibc floor at 2.35 (checked with
`objdump -T`).

**macOS:** signalmeow's cgo flags ask for `-lstdc++`, which Xcode no longer
ships. The Makefile's Darwin branch puts an empty `libstdc++.a` next to
`libsignal_ffi.a` and links `libc++` and the Security, CoreFoundation and
SystemConfiguration frameworks instead. `MACOSX_DEPLOYMENT_TARGET` is 11.0;
the workflow checks the binary links only system libraries (`otool -L`).
Unix socket paths are limited to 104 bytes there, which test temp
directories can exceed, so tests put sockets in short directories.

**Windows:** MSYS2 MINGW64 gcc for cgo, and libsignal built for Rust's
`x86_64-pc-windows-gnu` target (`rustup set default-host`), so both sides use
the mingw ABI; MSVC-built Rust doesn't link with mingw Go. In the workflows:

- `CC=gcc`, `CXX=g++`: otherwise BoringSSL's CMake looks for MSVC's `cl`.
- `LIBCLANG_PATH` to MSYS2's clang, for bindgen in boring-sys.
- `CMAKE_TOOLCHAIN_FILE_x86_64_pc_windows_gnu=build/windows-boringssl.cmake`
  builds BoringSSL without assembly. A native mingw build otherwise uses
  BoringSSL's NASM sources, which lack the ADX routines its headers call
  under gcc (`fiat_p256_adx_*`). `-DOPENSSL_NO_ASM` as a C flag instead also
  reaches `ring`, which then fails to link.
- The Makefile links with `-static`, so the mingw runtime
  (`libstdc++-6.dll`, `libwinpthread-1.dll`) is inside the `.exe`; the
  workflows check it imports only Windows DLLs, and `build/smoke.ps1` runs it
  from plain PowerShell, outside MSYS2's `PATH`.
- `-ldl` in signalmeow's cgo flags comes from the `mingw-w64-dlfcn` package.
- AF_UNIX sockets need Windows 10 1803 or later.

**Per-OS code** lives in `platform_*.go` and `internal/paths`:

| Concern | Linux | macOS | Windows |
|---|---|---|---|
| One daemon per store | `flock` | `flock` | `LockFileEx` |
| Detached auto-start | `setsid` | `setsid` | `DETACHED_PROCESS` + new process group |
| Data dir | `~/.local/share/signal-headless` | `~/Library/Application Support/signal-headless` | `%LOCALAPPDATA%\signal-headless` |
| Socket | `$XDG_RUNTIME_DIR/…sock` | in the data dir | in the data dir |
| Refuse to share with signal-cli | `/proc` scan | not checked | not checked |
| TUI "open attachment" | `xdg-open` | `open` | `rundll32 url.dll,FileProtocolHandler` |
| Background service | systemd user unit | none | none |
| `--uninstall` | runs the script | runs the script | prints the command (a running `.exe` can't delete itself) |

**Known gaps:** a real-device session on Linux arm64 and an Intel Mac; a
launchd agent for macOS and a service for Windows; signal-cli detection off
Linux; the VS Code extension's CI on macOS and Windows. Binaries aren't
signed: `install.sh`, `install.ps1` and the extension's download don't set the
quarantine flag, so Gatekeeper and SmartScreen don't block them; a binary
downloaded by hand in a browser does (see the README's Troubleshooting).

## Code layout

```
main.go, *cmd.go, link.go, check.go   flags and commands
uninstall.go, platform_*.go           --update/--uninstall, per-OS process handling
watchcmd.go                           --watch (internal/watch does the work)
internal/signalbackend   signalmeow adapter: the only code that talks to Signal
internal/fakebackend     test and development backend
internal/daemon          event handling, RPC dispatch, signal-cli compatibility
internal/history         conversations, messages, attachments, reactions, previews (SQLite)
internal/linkpreview     outgoing link-preview fetcher
internal/importer        signal-cli device import
internal/rpc             JSON-RPC server and client, API types
internal/watch           following one conversation: reconnect, catch-up, attachment settling
internal/tui             the terminal UI (bubbletea v2)
internal/paths           data directory and socket locations
build/                   portable build (Docker), packaging, installers, smoke and install tests
.github/workflows/       CI (tests) and releases (on v* tags)
```

Source files carry SPDX headers (`AGPL-3.0-or-later`); see the License
section of the README.
