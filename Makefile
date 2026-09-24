# signal-headless build.
#
# The Signal protocol lives in Rust libsignal; signalmeow binds it via cgo and
# expects a static libsignal_ffi.a. LIBSIGNAL_REV must match the libsignal
# submodule commit of the go.mau.fi/mautrix-signal version in go.mod.

LIBSIGNAL_REPO := https://github.com/signalapp/libsignal.git
LIBSIGNAL_REV  := 8fc2113bda042fc972a166a24b974b0d34155c6c
LIBSIGNAL_DIR  := third_party/libsignal
LIBSIGNAL_LIB  := $(LIBSIGNAL_DIR)/target/release/libsignal_ffi.a

BIN     := bin/signal-headless
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX  ?= $(HOME)/.local

export PATH := $(HOME)/.cargo/bin:$(PATH)
export CGO_ENABLED := 1
export CGO_LDFLAGS := -L$(abspath $(dir $(LIBSIGNAL_LIB)))

GO_SRC := $(shell find . -name '*.go' -not -path './third_party/*') go.mod go.sum

.PHONY: all build libsignal test vet install clean

all: build

build: $(BIN)

$(BIN): $(LIBSIGNAL_LIB) $(GO_SRC)
	go build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' -o $@ .

libsignal: $(LIBSIGNAL_LIB)

$(LIBSIGNAL_LIB):
	@if [ ! -d $(LIBSIGNAL_DIR)/.git ]; then \
		git clone --filter=blob:none $(LIBSIGNAL_REPO) $(LIBSIGNAL_DIR); \
	fi
	cd $(LIBSIGNAL_DIR) && git checkout -q $(LIBSIGNAL_REV)
	cd $(LIBSIGNAL_DIR) && RUSTFLAGS="-Ctarget-feature=-crt-static" RUSTC_WRAPPER="" \
		cargo build -p libsignal-ffi --profile=release

test: $(LIBSIGNAL_LIB)
	go test ./...

vet: $(LIBSIGNAL_LIB)
	go vet ./...

install: $(BIN)
	install -Dm755 $(BIN) $(PREFIX)/bin/signal-headless

clean:
	rm -rf bin
