#!/bin/sh
# SPDX-FileCopyrightText: 2026 Jeff Mattson
# SPDX-License-Identifier: AGPL-3.0-or-later
# Smoke test of a built binary on the platform it runs on: start a --fake
# daemon in a scratch directory, ask it for --status over its socket, then
# --stop it and wait for a clean exit. Nothing reaches Signal.
#
#   build/smoke.sh bin/signal-headless
set -eu
BIN=$1
dir=$(mktemp -d)
sock=$dir/s.sock
"$BIN" --version --json
"$BIN" --daemon --fake --data "$dir/data" --socket "$sock" >"$dir/log" 2>&1 &
pid=$!
up=0
for _ in $(seq 100); do
  if "$BIN" --status --fake --data "$dir/data" --socket "$sock" >"$dir/status" 2>&1; then up=1; break; fi
  sleep 0.2
done
if [ "$up" = 0 ]; then
  echo "smoke: daemon didn't answer --status" >&2
  cat "$dir/status" "$dir/log" >&2
  kill "$pid" 2>/dev/null || true
  exit 1
fi
cat "$dir/status"
"$BIN" --stop --fake --data "$dir/data" --socket "$sock"
wait "$pid" || { echo "smoke: daemon exited with $?" >&2; cat "$dir/log" >&2; exit 1; }
rm -rf "$dir"
echo "smoke: ok"
