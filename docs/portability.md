# Platforms

Status as of 2026-09-27:

| Platform | Built by | Tested |
|---|---|---|
| Linux x86-64 | release workflow (`ubuntu-22.04`), `make release-local` | yes: daily use, CI |
| Linux arm64 | release workflow (`ubuntu-22.04-arm`), *experimental* | not yet (the first release runs its tests) |
| macOS arm64 | release workflow (`macos-14`), CI (`macos-14`), *experimental* | not on a real Mac yet |
| macOS x86-64 | release workflow (`macos-15-intel`), *experimental* | not on a real Mac yet |
| Windows x86-64 | not built; CI type-checks it (`go vet` with mingw) | no |

*Experimental* platforms may fail in the release workflow without blocking
the release; that platform's tarball is then missing, and `install.sh` or
the VS Code extension say so. Drop `experimental: true` in
`.github/workflows/release.yml` once a platform has been through a release
and a real-device check.

## Why it matters

The VS Code extension runs on the UI side (`extensionKind: ["ui"]`). A Mac or
Windows laptop using Remote-SSH into a Linux box therefore needs the daemon
**on the laptop**. The extension downloads the daemon from the release it is
pinned to, so a platform only works there once that release has its tarball.

## How the platforms are built

Every platform builds natively on a GitHub-hosted runner; nothing is
cross-compiled. The slow step is libsignal (Rust, 10+ minutes cold), cached
per platform.

- **Linux:** Ubuntu 22.04 runners (x86-64 and arm64) keep the glibc floor at
  2.35; the workflow checks it with `objdump -T`. The arm64 runners are free
  for public repositories.
- **macOS:** signalmeow's cgo flags ask for `-lstdc++`, which Xcode no longer
  ships. The Makefile's Darwin branch puts an empty `libstdc++.a` next to
  `libsignal_ffi.a` and links `libc++` and the Security, CoreFoundation and
  SystemConfiguration frameworks instead. That framework list is an educated
  guess at what libsignal's Rust crates need; the first macOS CI run will
  confirm it or name what's missing. `MACOSX_DEPLOYMENT_TARGET` is 11.0. The
  workflow checks that the binary links only system libraries (`otool -L`).
- **Unix socket paths** are limited to 104 bytes on macOS, which the test
  temp directories can exceed; the daemon tests put their socket in a short
  directory.

## Per-OS code

Platform-specific code is split into per-OS files; Linux behaves exactly as
before.

| Concern | Linux | macOS | Windows |
|---|---|---|---|
| One daemon per store | `flock` | `flock` | `LockFileEx` (`platform_windows.go`) |
| Detached auto-start | `setsid` | `setsid` | `DETACHED_PROCESS` + new process group |
| Stopping the daemon | `--stop` (`shutdown` RPC), SIGTERM | same | `--stop` (no SIGTERM) |
| Data dir | `~/.local/share/signal-headless` | `~/Library/Application Support/signal-headless` | `%LOCALAPPDATA%\signal-headless` |
| Socket | `$XDG_RUNTIME_DIR/…sock` | in the data dir | in the data dir (AF_UNIX, Windows 10 1803+) |
| Refuse to share with signal-cli | `/proc` scan | not checked | not checked |
| TUI "open attachment" | `xdg-open` | `open` | `rundll32 url.dll,FileProtocolHandler` |
| Background service | systemd user unit (`install.sh --systemd`) | none yet (launchd agent possible) | none |
| Extension: desktop notifications | `notify-send`, click opens the chat | Notification Center via `osascript` (no click-through) | VS Code notifications only |

## What is left

1. **macOS:** run a release, then one session on a real Mac: link, receive,
   send, attachments, the TUI in Terminal and iTerm2, the extension. Code
   signing isn't needed for the binary as distributed: `install.sh` (curl)
   and the extension's download don't set the quarantine flag that makes
   Gatekeeper block unsigned binaries. Only a binary downloaded by hand in a
   browser would need `xattr -d com.apple.quarantine` (or signing and
   notarization, which needs an Apple developer account).
2. **Windows build:** a `windows-latest` job with MSYS2
   (`msys2/setup-msys2`) for mingw gcc, and libsignal for the
   `x86_64-pc-windows-gnu` Rust target so it links with mingw, which cgo
   needs (MSVC-built Rust and mingw-linked Go don't mix). Unknowns: building
   BoringSSL for that target, and AF_UNIX sockets under real use. Then the
   same real-device pass, and a `.zip` or keeping `.tar.gz` (Windows 10+ has
   `tar`; the extension has its own reader).
3. **Windows specifics:** desktop notifications (PowerShell toast APIs, or
   leave them to VS Code); keep AF_UNIX paths under ~100 characters, which is
   fine for `%LOCALAPPDATA%` with normal user names.
4. **signal-cli detection** off Linux (e.g. `ps` on macOS), if importing
   from signal-cli matters there; otherwise "stop signal-cli first" is
   enough.
5. **Extension tests on macOS:** `npm run test:vscode` needs no xvfb there;
   a `macos-14` job in the extension's CI would cover the macOS code paths
   (paths, notifications via `osascript`).

## Effort guess

- **Linux arm64:** done once a release passes; nothing platform-specific.
- **macOS:** fixing whatever the first CI run reports (likely the framework
  list), then half a day on a real Mac.
- **Windows:** 1–2 days. CI turns the libsignal/BoringSSL question into
  build logs instead of guesswork, but it may still stall there.

Nothing about linked-device behaviour differs between platforms.
