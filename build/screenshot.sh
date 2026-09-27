#!/bin/sh
# SPDX-FileCopyrightText: 2026 Jeff Mattson
# SPDX-License-Identifier: AGPL-3.0-or-later
# The README's terminal UI screenshot: the demo data set (Ender's Game; see
# internal/fakebackend/demo.go) in a fake daemon with its own scratch data
# and socket, captured from tmux and rendered by build/screenshot.py.
# Needs tmux, python3 with Pillow, DejaVu Sans Mono and Noto Color Emoji.
#
#   build/screenshot.sh [OUT.png]     default docs/tui.png
set -eu
cd "$(dirname "$0")/.."
out=${1:-docs/tui.png}
bin=bin/signal-headless
[ -x "$bin" ] || { echo "screenshot.sh: build first (make)" >&2; exit 1; }
tmp=$(mktemp -d)
tmux="tmux -L signal-headless-screenshot -f /dev/null"
trap '$tmux kill-server 2>/dev/null; "$bin" --stop --fake --data "$tmp/data" --socket "$tmp/s.sock" >/dev/null 2>&1; rm -rf "$tmp"' EXIT
# Explicit --data and --socket: never the real daemon's.
$tmux new-session -d -s shot -x 110 -y 43 \
  "env SIGNAL_HEADLESS_DEMO=1 COLORTERM=truecolor TERM=xterm-256color $bin --shell --fake --data $tmp/data --socket $tmp/s.sock"
$tmux set -g default-terminal xterm-256color >/dev/null
$tmux set -ga terminal-overrides ',*:RGB' >/dev/null
sleep 12 # connect, load, and let the new-message notice clear
$tmux capture-pane -t shot -p >"$tmp/plain"
$tmux capture-pane -t shot -e -p >"$tmp/capture.ans"
if ! grep -q '+15550000000' "$tmp/plain" || ! grep -q 'Bean' "$tmp/plain"; then
  echo "screenshot.sh: the capture isn't the demo data set; not writing it" >&2
  cat "$tmp/plain" >&2
  exit 1
fi
python3 build/screenshot.py "$tmp/capture.ans" "$out"
echo "wrote $out"
