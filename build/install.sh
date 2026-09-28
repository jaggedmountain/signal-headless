#!/bin/sh
# SPDX-FileCopyrightText: 2026 Jeff Mattson
# SPDX-License-Identifier: AGPL-3.0-or-later
# Installs signal-headless from its GitHub releases:
#
#   curl -fsSL https://github.com/jaggedmountain/signal-headless/releases/latest/download/install.sh | sh
#   curl -fsSL …/install.sh | sh -s -- --systemd            # also set up the systemd user service
#
# Options: --version vX.Y.Z (default: latest), --prefix DIR (default: ~/.local),
# --systemd. The same as environment variables: SIGNAL_HEADLESS_VERSION,
# PREFIX, SIGNAL_HEADLESS_SYSTEMD=1. SIGNAL_HEADLESS_BASE_URL points at a
# mirror (default https://github.com/jaggedmountain/signal-headless/releases).
set -eu

VERSION=${SIGNAL_HEADLESS_VERSION:-latest}
PREFIX=${PREFIX:-$HOME/.local}
SYSTEMD=${SIGNAL_HEADLESS_SYSTEMD:-0}
BASE=${SIGNAL_HEADLESS_BASE_URL:-https://github.com/jaggedmountain/signal-headless/releases}

while [ $# -gt 0 ]; do
  case $1 in
    --version) VERSION=$2; shift 2 ;;
    --prefix) PREFIX=$2; shift 2 ;;
    --systemd) SYSTEMD=1; shift ;;
    -h|--help) sed -n '4,14p' "$0" 2>/dev/null || true; exit 0 ;;
    *) echo "install.sh: unknown option $1" >&2; exit 2 ;;
  esac
done

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "no release for $(uname -s) here (on Windows, use install.ps1 from the same release)" ;;
esac
case $(uname -m) in
  x86_64|amd64) arch=x64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "no release for $(uname -m) yet (x86-64 and arm64 only)" ;;
esac
[ "$SYSTEMD" = 1 ] && [ "$os" != linux ] && die "--systemd is for Linux only"
asset=signal-headless-$os-$arch.tar.gz

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL --proto '=https,http' -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
else
  die "needs curl or wget"
fi
if command -v sha256sum >/dev/null 2>&1; then
  sha() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "needs sha256sum or shasum"
fi

if [ "$VERSION" = latest ]; then
  url=$BASE/latest/download
else
  case $VERSION in v*) ;; *) VERSION=v$VERSION ;; esac
  url=$BASE/download/$VERSION
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
say "Downloading $asset ($VERSION)…"
fetch "$url/$asset" "$tmp/$asset" || die "download failed: $url/$asset"
fetch "$url/SHA256SUMS" "$tmp/SHA256SUMS" || die "download failed: $url/SHA256SUMS"
want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")
[ -n "$want" ] || die "$asset is not listed in SHA256SUMS"
[ "$(sha "$tmp/$asset")" = "$want" ] || die "checksum mismatch for $asset — not installing"

tar -xzf "$tmp/$asset" -C "$tmp"
bin=$tmp/signal-headless/signal-headless
if ! "$bin" --version >/dev/null 2>&1; then
  case $os in
    linux) die "the binary doesn't run here (it needs glibc 2.34 or newer; this system: $(ldd --version 2>/dev/null | head -1))" ;;
    *) die "the binary doesn't run here (it needs macOS 11 or newer)" ;;
  esac
fi

mkdir -p "$PREFIX/bin"
# Replace atomically: a running daemon keeps its old copy until restarted.
cp "$bin" "$PREFIX/bin/.signal-headless.new"
chmod 755 "$PREFIX/bin/.signal-headless.new"
mv -f "$PREFIX/bin/.signal-headless.new" "$PREFIX/bin/signal-headless"
say "Installed $("$PREFIX/bin/signal-headless" --version) to $PREFIX/bin/signal-headless"

if [ "$SYSTEMD" = 1 ]; then
  unit_dir=${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user
  mkdir -p "$unit_dir"
  sed "s|%h/.local/bin/signal-headless|$PREFIX/bin/signal-headless|" "$tmp/signal-headless/signal-headless.service" >"$unit_dir/signal-headless.service"
  if command -v systemctl >/dev/null 2>&1; then
    systemctl --user daemon-reload
    systemctl --user enable signal-headless.service >/dev/null
    say "systemd user service installed and enabled ($unit_dir/signal-headless.service)."
  else
    say "Unit written to $unit_dir/signal-headless.service (systemctl not found)."
  fi
fi

# This install's systemd unit (now or from an earlier --systemd): one whose
# ExecStart is our binary. Enabled → updates keep it (update.sh passes --systemd).
unit=${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/signal-headless.service
our_unit=0 unit_enabled=0
if [ -f "$unit" ] && grep -qF "$PREFIX/bin/signal-headless" "$unit"; then
  our_unit=1
  if systemctl --user is-enabled --quiet signal-headless.service 2>/dev/null; then unit_enabled=1; fi
fi

# The uninstaller, with this install's paths: `signal-headless --uninstall`
# runs it (it looks for ../libexec/signal-headless/uninstall.sh next to
# itself). Not under share/: on Linux, ~/.local/share/signal-headless is the
# data directory.
q() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }
libexec=$PREFIX/libexec/signal-headless
mkdir -p "$libexec"
{
  echo '#!/bin/sh'
  echo "# Written by signal-headless's install.sh; removes that install."
  echo '#'
  echo '#   uninstall.sh            unlink this computer from the Signal account, delete its'
  echo '#                           message history and keys, and remove the program'
  echo '#   uninstall.sh --retain   remove only the program; history and keys stay'
  echo '#   -y                      no "are you sure" (unlinking still asks for the number)'
  echo "BIN=$(q "$PREFIX/bin/signal-headless")"
  echo "LIBEXEC=$(q "$libexec")"
  echo "UNIT=$(q "$unit")"
  cat <<'EOF'
set -eu
PURGE=1 YES=0
for a in "$@"; do
  case $a in
    --retain) PURGE=0 ;;
    -y|--yes) YES=1 ;;
    *) echo "uninstall.sh: unknown option $a" >&2; exit 2 ;;
  esac
done
ask() { # ask QUESTION → 0 for yes
  [ "$YES" = 1 ] && return 0
  printf '%s [y/N] ' "$1"
  read -r answer </dev/tty || return 1
  case $answer in y|Y|yes) return 0 ;; *) return 1 ;; esac
}

data=
linked=0
if [ -x "$BIN" ]; then
  info=$("$BIN" --check --json 2>/dev/null) && linked=1 || true
  data=$(printf '%s' "$info" | sed -n 's/.*"dataDir":"\([^"]*\)".*/\1/p')
fi
if [ "$PURGE" = 1 ]; then
  msg="This removes signal-headless ($BIN)"
  if [ "$linked" = 1 ]; then msg="$msg, unlinks this computer from the Signal account"; fi
  if [ -n "$data" ] && [ -d "$data" ]; then msg="$msg, and deletes its message history and keys ($data)"; fi
  echo "$msg."
  echo "To keep the history and keys instead, uninstall with --retain."
else
  echo "This removes signal-headless ($BIN); message history and keys stay in ${data:-its data directory}."
fi
ask "Continue?" || { echo "Nothing changed."; exit 1; }

if [ -f "$UNIT" ] && grep -qF "$BIN" "$UNIT"; then
  systemctl --user disable --now signal-headless.service 2>/dev/null || true
  rm -f "$UNIT"
  systemctl --user daemon-reload 2>/dev/null || true
  echo "Removed the systemd user service."
fi
if [ "$PURGE" = 1 ] && [ "$linked" = 1 ]; then
  if ! "$BIN" --unlink; then
    echo "Unlinking failed. Deleting the data anyway leaves this computer listed on the phone"
    echo "(remove it there: Settings > Linked devices)."
    ask "Delete the data anyway?" || { echo "Stopped; the program is still installed."; exit 1; }
  fi
fi
if [ -x "$BIN" ]; then
  "$BIN" --stop >/dev/null 2>&1 || true
  i=0
  while "$BIN" --status >/dev/null 2>&1 && [ $i -lt 50 ]; do sleep 0.2; i=$((i + 1)); done
fi
if [ "$PURGE" = 1 ] && [ -n "$data" ]; then
  rm -rf "$data"
  echo "Deleted $data."
fi
rm -f "$BIN"
# Only our own file, never rm -rf: this directory must not hold anything else.
rm -f "$LIBEXEC/uninstall.sh" "$LIBEXEC/update.sh"
rmdir "$LIBEXEC" 2>/dev/null || true
echo "Removed signal-headless."
if [ "$PURGE" = 0 ] && [ -n "$data" ] && [ -d "$data" ]; then
  echo "Kept $data."
  if [ "$linked" = 1 ]; then
    echo "This computer is still linked. To remove it from the account later: reinstall and run"
    echo "signal-headless --unlink, or remove it on the phone (Settings > Linked devices)."
  fi
fi
EOF
} >"$libexec/uninstall.sh"
chmod 755 "$libexec/uninstall.sh"

# The updater: the latest release's install.sh with this install's options.
# `signal-headless --update` runs it.
{
  echo '#!/bin/sh'
  echo "# Written by signal-headless's install.sh; updates that install with the same"
  echo '# options. History, keys and the link are kept.'
  echo '#'
  echo '#   update.sh                     the latest release'
  echo '#   update.sh --version vX.Y.Z    a specific one'
  echo "PREFIX=$(q "$PREFIX")"
  echo "BASE=$(q "$BASE")"
  echo "SYSTEMD=$unit_enabled"
  cat <<'EOF'
set -eu
VERSION=latest
while [ $# -gt 0 ]; do
  case $1 in
    --version) VERSION=$2; shift 2 ;;
    *) echo "update.sh: unknown option $1" >&2; exit 2 ;;
  esac
done
if [ "$VERSION" = latest ]; then
  url=$BASE/latest/download
else
  case $VERSION in v*) ;; *) VERSION=v$VERSION ;; esac
  url=$BASE/download/$VERSION
fi
if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL --proto '=https,http' -o "$2" "$1"; }
else
  fetch() { wget -q -O "$2" "$1"; }
fi
if command -v sha256sum >/dev/null 2>&1; then
  sha() { sha256sum "$1" | cut -d' ' -f1; }
else
  sha() { shasum -a 256 "$1" | cut -d' ' -f1; }
fi
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
fetch "$url/install.sh" "$tmp/install.sh" || { echo "update.sh: download failed: $url/install.sh" >&2; exit 1; }
fetch "$url/SHA256SUMS" "$tmp/SHA256SUMS" || { echo "update.sh: download failed: $url/SHA256SUMS" >&2; exit 1; }
want=$(awk '$2 == "install.sh" || $2 == "*install.sh" { print $1 }' "$tmp/SHA256SUMS")
if [ -z "$want" ] || [ "$(sha "$tmp/install.sh")" != "$want" ]; then
  echo "update.sh: install.sh doesn't match the release's SHA256SUMS; not running it" >&2
  exit 1
fi
set -- --prefix "$PREFIX" --version "$VERSION"
if [ "$SYSTEMD" = 1 ]; then set -- "$@" --systemd; fi
SIGNAL_HEADLESS_BASE_URL=$BASE sh "$tmp/install.sh" "$@"
EOF
} >"$libexec/update.sh"
chmod 755 "$libexec/update.sh"

case ":$PATH:" in
  *":$PREFIX/bin:"*) ;;
  *) say "Note: $PREFIX/bin is not on PATH; add it, e.g. export PATH=\"$PREFIX/bin:\$PATH\"" ;;
esac

new_version=$("$PREFIX/bin/signal-headless" --version | cut -d' ' -f2)
if running=$("$PREFIX/bin/signal-headless" --status 2>/dev/null | awk '$1 == "daemon:" { print $2 }') && [ -n "$running" ]; then
  if [ "$running" != "$new_version" ]; then
    if [ "$our_unit" = 1 ] && systemctl --user is-active --quiet signal-headless.service 2>/dev/null; then
      # Under systemd, --stop would leave the service stopped (it only restarts
      # on failure) and clients would start a daemon outside it.
      if systemctl --user restart signal-headless.service; then
        say "Restarted the systemd service: $running → $new_version."
      else
        say "The service still runs $running; restart it with: systemctl --user restart signal-headless"
      fi
    else
      say "The running daemon is $running; restart it on $new_version with: signal-headless --stop   (it starts again when needed; daemons before --stop: pkill -x signal-headless)"
    fi
  fi
elif "$PREFIX/bin/signal-headless" --check >/dev/null 2>&1; then
  say "Linked. Start with: signal-headless"
else
  say "Next: link this computer — signal-headless --link   (scan the QR code: phone → Settings → Linked devices)"
  if [ "$SYSTEMD" = 1 ]; then say "then start the service: systemctl --user start signal-headless"; fi
fi
