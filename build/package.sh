#!/bin/sh
# SPDX-FileCopyrightText: 2026 Jeff Mattson
# SPDX-License-Identifier: AGPL-3.0-or-later
# Packages a built binary as release assets (names carry no version, so
# https://github.com/<repo>/releases/latest/download/<asset> always works):
#
#   build/package.sh BINARY PLATFORM OUTDIR    e.g. dist/linux-x64/signal-headless linux-x64 dist/release
#
# → OUTDIR/signal-headless-PLATFORM.tar.gz  (signal-headless/{signal-headless,LICENSE,README.md,signal-headless.service})
#   OUTDIR/install.sh
#   OUTDIR/SHA256SUMS                       (every asset in OUTDIR)
set -eu
BIN=$1 PLATFORM=$2 OUT=$3
cd "$(dirname "$0")/.."
mkdir -p "$OUT"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
mkdir "$stage/signal-headless"
install -m755 "$BIN" "$stage/signal-headless/signal-headless"
install -m644 LICENSE README.md signal-headless.service "$stage/signal-headless/"
# Reproducible-ish: fixed owner and order, times from the last commit.
mtime=$(git log -1 --format=%ct 2>/dev/null || date +%s)
tar -C "$stage" --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$mtime" -cf - signal-headless | gzip -9n >"$OUT/signal-headless-$PLATFORM.tar.gz"
install -m755 build/install.sh "$OUT/install.sh"
(cd "$OUT" && rm -f SHA256SUMS && sha256sum -- * >SHA256SUMS.tmp && mv SHA256SUMS.tmp SHA256SUMS)
"$BIN" --version
ls -l "$OUT"
