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
LIBSIGNAL_LIBDIR := $(abspath $(dir $(LIBSIGNAL_LIB)))
ifeq ($(OS),Windows_NT)
# MSYS2 (MINGW64): the native Go and gcc need D:/… paths.
LIBSIGNAL_LIBDIR := $(shell cygpath -m $(LIBSIGNAL_LIBDIR))
endif
export CGO_LDFLAGS := -L$(LIBSIGNAL_LIBDIR)

# macOS: signalmeow's cgo flags ask for -lstdc++, which Xcode no longer ships.
# An empty libstdc++.a next to libsignal_ffi.a satisfies the flag, and libc++
# plus the system frameworks Rust's TLS and networking crates use do the work.
# MACOSX_DEPLOYMENT_TARGET is honoured by both cargo and cgo's clang.
ifeq ($(shell uname -s),Darwin)
export MACOSX_DEPLOYMENT_TARGET ?= 11.0
CGO_LDFLAGS += -lc++ -framework Security -framework CoreFoundation -framework SystemConfiguration
STDCXX_STUB := $(dir $(LIBSIGNAL_LIB))libstdc++.a
endif

# Windows, built with MSYS2 MINGW64 gcc and Rust's x86_64-pc-windows-gnu
# target (see docs/portability.md): the system libraries Rust's std, tokio
# and BoringSSL use; -ldl comes from the mingw-w64 dlfcn package.
ifeq ($(OS),Windows_NT)
BIN := bin/signal-headless.exe
CGO_LDFLAGS += -lws2_32 -luserenv -lbcrypt -lntdll -ladvapi32 -lcrypt32 -lsecur32 -lncrypt -lole32 -loleaut32 -liphlpapi -lpsapi -lshell32 -luser32 -lsynchronization -lkernel32
endif

GO_SRC := $(shell find . -name '*.go' -not -path './third_party/*') go.mod go.sum

.PHONY: all build libsignal test vet install clean release-local

all: build

build: $(BIN)

$(BIN): $(LIBSIGNAL_LIB) $(STDCXX_STUB) $(GO_SRC)
	go build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' -o $@ .

libsignal: $(LIBSIGNAL_LIB)

$(LIBSIGNAL_LIB):
	@if [ ! -d $(LIBSIGNAL_DIR)/.git ]; then \
		git clone --filter=blob:none $(LIBSIGNAL_REPO) $(LIBSIGNAL_DIR); \
	fi
	cd $(LIBSIGNAL_DIR) && git checkout -q $(LIBSIGNAL_REV)
	cd $(LIBSIGNAL_DIR) && RUSTFLAGS="-Ctarget-feature=-crt-static" RUSTC_WRAPPER="" \
		cargo build -p libsignal-ffi --profile=release

$(STDCXX_STUB): $(LIBSIGNAL_LIB)
	printf '' | cc -x c -c -o $(@D)/stdcxx-stub.o -
	ar rcs $@ $(@D)/stdcxx-stub.o

test: $(LIBSIGNAL_LIB) $(STDCXX_STUB)
	go test ./...

vet: $(LIBSIGNAL_LIB) $(STDCXX_STUB)
	go vet ./...

install: $(BIN)
	install -Dm755 $(BIN) $(PREFIX)/bin/signal-headless

clean:
	rm -rf bin

# release-local: the release assets without CI (portable build in Docker).
release-local:
	build/dist.sh
	build/package.sh dist/linux-x64/signal-headless linux-x64 dist/release

