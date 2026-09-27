#!/bin/sh
# SPDX-FileCopyrightText: 2026 Jeff Mattson
# SPDX-License-Identifier: AGPL-3.0-or-later
# install.sh and its uninstaller against a local mirror of a built binary:
# install into a scratch prefix, --update from the mirror's "latest", check
# `signal-headless --uninstall` finds the uninstaller, then uninstall (which deletes the data too) and check nothing
# is left. For CI
# runners: it uses the default data directory, and deletes it.
#
#   build/test-install.sh bin/signal-headless linux-x64
set -eu
if [ "${CI:-}" != true ]; then
  echo "test-install.sh: CI only (it stops the default daemon and deletes its data)" >&2
  exit 1
fi
BIN=$1 PLATFORM=$2
cd "$(dirname "$0")/.."
tmp=$(mktemp -d)
build/package.sh "$BIN" "$PLATFORM" "$tmp/mirror/download/v0.0.0" >/dev/null
mkdir -p "$tmp/mirror/latest" && cp -R "$tmp/mirror/download/v0.0.0" "$tmp/mirror/latest/download"
port=8769
python3 -m http.server "$port" --directory "$tmp/mirror" >/dev/null 2>&1 &
server=$!
trap 'kill $server 2>/dev/null; rm -rf "$tmp"' EXIT
sleep 1
prefix=$tmp/prefix
SIGNAL_HEADLESS_BASE_URL=http://127.0.0.1:$port sh build/install.sh --version v0.0.0 --prefix "$prefix"
"$prefix/bin/signal-headless" --version
test -x "$prefix/libexec/signal-headless/uninstall.sh"
# --update: the "latest" release, via the mirror update.sh remembers.
env -u SIGNAL_HEADLESS_BASE_URL "$prefix/bin/signal-headless" --update
"$prefix/bin/signal-headless" --version
# Without a terminal the uninstaller can't ask, so it changes nothing.
if out=$("$prefix/bin/signal-headless" --uninstall </dev/null 2>&1); then
  echo "--uninstall without a terminal should change nothing: $out" >&2; exit 1
fi
case $out in *"Nothing changed"*) ;; *) echo "unexpected: $out" >&2; exit 1 ;; esac
data=$("$prefix/bin/signal-headless" --check --json | sed -n 's/.*"dataDir":"\([^"]*\)".*/\1/p') || true
sh "$prefix/libexec/signal-headless/uninstall.sh" -y
test ! -e "$prefix/bin/signal-headless"
test ! -e "$prefix/libexec/signal-headless"
test -z "$data" || test ! -e "$data"
echo "test-install: ok"
