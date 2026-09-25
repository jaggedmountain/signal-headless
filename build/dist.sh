#!/bin/sh
# SPDX-FileCopyrightText: 2026 Jeff Mattson
# SPDX-License-Identifier: AGPL-3.0-or-later
# Builds a portable linux-x64 signal-headless in an Ubuntu 22.04 container:
#   build/dist.sh → dist/linux-x64/signal-headless
# The libsignal checkout (third_party/libsignal) is reused read-only; cargo
# and Go caches live in docker volumes so rebuilds are quick.
set -eu
cd "$(dirname "$0")/.."
LIBSIGNAL_REV=$(sed -n 's/^LIBSIGNAL_REV *:= *//p' Makefile)
GO_VERSION=$(sed -n 's/^go //p' go.mod)
RUST_VERSION=$(cat third_party/libsignal/rust-toolchain 2>/dev/null || echo stable)
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE=signal-headless-dist:go${GO_VERSION}-rust${RUST_VERSION}

[ -d third_party/libsignal/.git ] || make libsignal
git -C third_party/libsignal checkout -q "$LIBSIGNAL_REV"

docker build -q -t "$IMAGE" --build-arg GO_VERSION="$GO_VERSION" --build-arg RUST_VERSION="$RUST_VERSION" -f build/dist.Dockerfile build >/dev/null

mkdir -p dist/linux-x64
# Named volumes start out root-owned; hand them to the building user.
docker run --rm -v signal-headless-dist-target:/target -v signal-headless-dist-cache:/cache "$IMAGE" \
  chown "$(id -u):$(id -g)" /target /cache
docker run --rm \
  -u "$(id -u):$(id -g)" \
  -v "$PWD:/src:ro" \
  -v "$PWD/dist:/out" \
  -v signal-headless-dist-target:/target \
  -v signal-headless-dist-cache:/cache \
  -e HOME=/cache -e CARGO_HOME=/cache/cargo -e GOPATH=/cache/go -e GOCACHE=/cache/go-build \
  -e CARGO_TARGET_DIR=/target -e VERSION="$VERSION" \
  "$IMAGE" sh -euc '
    cd /src/third_party/libsignal
    RUSTFLAGS="-Ctarget-feature=-crt-static" cargo build -p libsignal-ffi --profile=release
    cd /src
    CGO_ENABLED=1 CGO_LDFLAGS=-L/target/release go build -buildvcs=false -trimpath \
      -ldflags "-s -w -X main.version=$VERSION" -o /out/linux-x64/signal-headless .
  '
objdump -T dist/linux-x64/signal-headless | grep -oE "GLIBC_[0-9.]+" | sort -Vu | tail -1 | sed 's/^/requires /'
ls -la dist/linux-x64/signal-headless
