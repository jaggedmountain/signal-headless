# Platforms

Status as of 2026-09-27, after the first pre-release (`v0.1.0-rc.1`):

| Platform | Built by | Tested |
|---|---|---|
| Linux x86-64 | CI, releases (`ubuntu-22.04`), `make release-local` | daily use; CI tests + smoke test |
| Linux arm64 | releases (`ubuntu-22.04-arm`), *experimental* | release job: tests + smoke test |
| macOS arm64 | CI and releases (`macos-14`), *experimental* | CI: tests + smoke test; not on a real Mac yet |
| macOS x86-64 | releases (`macos-15-intel`), *experimental* | release job: tests + smoke test; not on a real Mac yet |
| Windows x86-64 | CI and releases (`windows-latest`, MSYS2), *experimental* | CI: tests + smoke test; not on a real PC yet |

"Smoke test" is `build/smoke.sh`: the built binary starts a `--fake` daemon,
answers `--status` over its unix socket, and stops on `--stop`. The Go tests
run against the fake backend. Nothing in CI talks to Signal, so linking,
receiving and sending still need one session on a real machine per platform.

*Experimental* platforms may fail in the release workflow without blocking
the release; that platform's tarball is then missing, and `install.sh` or
the VS Code extension say so. Drop `experimental: true` in
`.github/workflows/release.yml` once a platform has had a real-device check.

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
  SystemConfiguration frameworks instead (enough, per CI).
  `MACOSX_DEPLOYMENT_TARGET` is 11.0. The workflow checks that the binary
  links only system libraries (`otool -L`).
- **Windows:** MSYS2 MINGW64 gcc for cgo, and libsignal built for Rust's
  `x86_64-pc-windows-gnu` target (`rustup set default-host`), so both sides
  use the mingw ABI; MSVC-built Rust doesn't link with mingw Go. Settings,
  all in the workflows:
  - `CC=gcc`, `CXX=g++`: otherwise BoringSSL's CMake looks for MSVC's `cl`.
  - `LIBCLANG_PATH` to MSYS2's clang, for bindgen in boring-sys.
  - `CMAKE_TOOLCHAIN_FILE_x86_64_pc_windows_gnu=build/windows-boringssl.cmake`,
    which builds BoringSSL without assembly. A native mingw build otherwise
    uses BoringSSL's NASM sources, which lack the ADX routines its C headers
    call under gcc (`fiat_p256_adx_*`), and the final link fails. Setting
    `-DOPENSSL_NO_ASM` as a C flag instead also reaches `ring`, which then
    fails to link.
  - The Makefile links with `-static`, so the mingw runtime
    (`libstdc++-6.dll`, `libwinpthread-1.dll`) is inside the `.exe`; the
    workflows check that it imports only Windows DLLs. (rc.1 missed this:
    MSYS2 has those DLLs on `PATH`, so CI passed.)
  - `-ldl` in signalmeow's cgo flags comes from the `mingw-w64-dlfcn`
    package.
- **Unix socket paths** are limited to 104 bytes on macOS, which the test
  temp directories can exceed; the daemon tests put their socket in a short
  directory. AF_UNIX works on Windows 10 1803+ (the smoke test uses it).
- **Timing:** the slower macOS runners exposed a race in the daemon tests (a
  broadcast sent before the server registered the connection); the test
  helper now waits for a round trip.

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

1. **A real-device session per platform:** link, receive, send,
   attachments, the TUI (Terminal and iTerm2 on macOS, Windows Terminal),
   and the VS Code extension against the local daemon. Then drop
   `experimental`.
2. **Signing:** not needed for the binary as distributed: `install.sh`
   (curl) and the extension's download don't set the quarantine flag that
   makes Gatekeeper block unsigned binaries, and SmartScreen only checks
   browser downloads. A binary downloaded by hand in a browser needs
   `xattr -d com.apple.quarantine` on macOS, or signing and notarization
   (Apple developer account).
3. **Windows install:** `install.ps1` (`irm …/install.ps1 | iex`) is the
   counterpart of `install.sh`: checksum-verified, per user into
   `%LOCALAPPDATA%\Programs\signal-headless`, added to the user PATH. It
   replaces a running daemon's `.exe` by renaming it first (Windows allows
   that, not overwriting). CI runs it in Windows PowerShell 5.1 and
   PowerShell 7 against the job's own package. The VS Code extension looks
   there too, and downloads the daemon itself when nothing is installed.
   Updating: both installers also write an updater (`update.sh` /
   `update.ps1`, same places) that fetches the latest release's installer,
   checks it against `SHA256SUMS`, and runs it with this install's options;
   `signal-headless --update` runs it on every OS (on Windows the installer
   renames the running `.exe`, so it can replace it). A daemon under this
   install's systemd unit is restarted on the new version.
   Uninstalling: both installers write an uninstaller with the install's
   paths (`PREFIX/libexec/signal-headless/uninstall.sh`, not under `share/`,
   which on Linux is the data directory; `uninstall.ps1` next to the
   `.exe`). `signal-headless --uninstall` runs it (Unix) or prints the
   command (Windows). By default they unlink this computer and delete the
   message history and keys (plaintext on disk); `--retain`/`-Retain`
   keeps them. They delete only their own files, confirm first, and CI
   runs them on all three OSes.
4. **Windows specifics:** desktop notifications (PowerShell toast APIs, or
   leave them to VS Code); keep AF_UNIX paths under ~100 characters, fine
   for `%LOCALAPPDATA%` with normal user names; no background service yet.
5. **macOS background service:** a launchd agent, the counterpart of the
   systemd unit.
6. **signal-cli detection** off Linux (e.g. `ps` on macOS), if importing
   from signal-cli matters there; otherwise "stop signal-cli first" is
   enough.
7. **Extension tests on macOS and Windows:** `npm run test:vscode` needs no
   xvfb there; jobs in the extension's CI would cover its per-OS code
   (paths, `osascript` notifications).

Nothing about linked-device behaviour differs between platforms.
