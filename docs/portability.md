# macOS and Windows

Status as of 2026-09-26: **Linux only in practice; the code is prepared for
macOS and Windows, but neither has been built into a binary or run.**

## Why it matters

The VS Code extension runs on the UI side (`extensionKind: ["ui"]`). A
macOS or Windows laptop using Remote-SSH into a Linux box therefore needs
the daemon **on the laptop**, not on the Linux box. So the laptop platforms
are the ones Remote-SSH users need.

## What is done

Platform-specific code is split into per-OS files. The Linux build behaves
exactly as before.

| Concern | Linux | macOS | Windows |
|---|---|---|---|
| One daemon per store | `flock` | `flock` | `LockFileEx` (`platform_windows.go`) |
| Detached auto-start | `setsid` | `setsid` | `DETACHED_PROCESS` + new process group |
| Data dir | `~/.local/share/signal-headless` | `~/Library/Application Support/signal-headless` | `%LOCALAPPDATA%\signal-headless` |
| Socket | `$XDG_RUNTIME_DIR/…sock` | in the data dir | in the data dir (AF_UNIX, Windows 10 1803+) |
| Refuse to share with signal-cli | `/proc` scan | not checked | not checked |
| TUI "open attachment" | `xdg-open` | `open` | `rundll32 url.dll,FileProtocolHandler` |
| Extension: socket/data defaults | mirrors daemon | mirrors daemon | mirrors daemon (`.exe` lookup) |
| Extension: desktop notifications | `notify-send`, click opens the chat | Notification Center via `osascript` (no click-through) | none yet — VS Code notifications only |

**Verified:** the whole tree, including libsignal's cgo bindings,
signalmeow and SQLite, type-checks for Windows. The check is
`GOOS=windows CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc go vet ./...` with
mingw-w64 on Linux. The same check for macOS needs the Apple SDK, which isn't
available on Linux without osxcross; the macOS paths share the Unix code
above, so there is little macOS-only code to get wrong.

## What is left

1. **Build libsignal for each target.**
   - **macOS** (`aarch64-apple-darwin`, `x86_64-apple-darwin`) wants a Mac.
     A GitHub Actions `macos-14` runner builds libsignal and the Go binary
     natively (Xcode clang for cgo).
   - **Windows:** use the `x86_64-pc-windows-gnu` Rust target, so the static
     library links with mingw gcc, which cgo needs. MSVC-built Rust and
     mingw-linked Go don't mix. This can be cross-built from Linux
     (`rustup target add x86_64-pc-windows-gnu`, mingw-w64, cmake for
     BoringSSL), but that path is untried; a Windows runner with MSYS2 is
     the fallback.
2. **Run it.** Link, receive, send, attachments, the TUI (Windows Terminal,
   macOS Terminal and iTerm2), and the extension against the local daemon.
3. **Windows specifics.**
   - **Stopping the daemon:** there's no SIGTERM, so stopping means
     `taskkill` or closing it. A `shutdown` RPC would make this clean for
     the extension and `--status` users.
   - **Notifications:** desktop notifications via PowerShell toast APIs,
     or leave them to VS Code.
   - **Socket paths:** keep AF_UNIX paths under ~100 characters. That's fine
     for `%LOCALAPPDATA%` with normal user names.
4. **Packaging.** Platform VSIX packages (`vsce package --target
   darwin-arm64 | darwin-x64 | win32-x64`) with the binary from CI in
   `bin/<platform>-<arch>/`, as for `linux-x64` now. The CI should also check
   what each binary needs, like the `GLIBC_2.34` check on Linux.
5. **signal-cli detection** off Linux, e.g. via `ps` on macOS, if importing
   from signal-cli matters there. Otherwise document "stop signal-cli
   first".

## Effort guess

- **macOS:** mostly the CI build plus a test pass on a Mac, about a day,
  given the groundwork above.
- **Windows:** the libsignal build and AF_UNIX/runtime testing are the
  unknowns, about 2–3 days, and the build could stall on BoringSSL
  cross-compilation.

Nothing about linked-device behaviour differs between platforms.
