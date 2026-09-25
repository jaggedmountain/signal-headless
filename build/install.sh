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
  *) die "no release for $(uname -s) yet (Linux only for now; macOS/Windows: see docs/portability.md)" ;;
esac
case $(uname -m) in
  x86_64|amd64) arch=x64 ;;
  *) die "no release for $(uname -m) yet (x86-64 only for now)" ;;
esac
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
"$bin" --version >/dev/null 2>&1 || die "the binary doesn't run here (it needs glibc 2.34 or newer; this system: $(ldd --version 2>/dev/null | head -1))"

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

case ":$PATH:" in
  *":$PREFIX/bin:"*) ;;
  *) say "Note: $PREFIX/bin is not on PATH; add it, e.g. export PATH=\"$PREFIX/bin:\$PATH\"" ;;
esac

new_version=$("$PREFIX/bin/signal-headless" --version | cut -d' ' -f2)
if running=$("$PREFIX/bin/signal-headless" --status 2>/dev/null | awk '$1 == "daemon:" { print $2 }') && [ -n "$running" ]; then
  if [ "$running" != "$new_version" ]; then
    say "The running daemon is $running; restart it on $new_version with: signal-headless --stop   (it starts again when needed; daemons before --stop: pkill -x signal-headless)"
  fi
elif "$PREFIX/bin/signal-headless" --check >/dev/null 2>&1; then
  say "Linked. Start with: signal-headless"
else
  say "Next: link this computer — signal-headless --link   (scan the QR code: phone → Settings → Linked devices)"
  [ "$SYSTEMD" = 1 ] && say "then start the service: systemctl --user start signal-headless"
fi
