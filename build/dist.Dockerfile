# Toolchain for portable release builds: an older glibc (Ubuntu 22.04 →
# glibc 2.35) so the binary runs on more systems than the build host.
# Used only to build (make dist); nothing here is needed at runtime.
FROM ubuntu:22.04
ARG GO_VERSION
ARG RUST_VERSION
# 22.04's protoc (3.12) predates proto3 optional fields, which libsignal uses.
ARG PROTOC_VERSION=29.3
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git build-essential clang libclang-dev cmake make \
      pkg-config unzip zlib1g-dev \
    && rm -rf /var/lib/apt/lists/*
RUN curl -sSfL -o /tmp/protoc.zip https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-linux-x86_64.zip \
    && unzip -q /tmp/protoc.zip -d /usr/local bin/protoc 'include/*' && chmod -R a+rX /usr/local/include/google && rm /tmp/protoc.zip
RUN curl -sSfL https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz | tar -C /usr/local -xz
ENV RUSTUP_HOME=/opt/rustup CARGO_HOME=/opt/cargo
RUN curl -sSf https://sh.rustup.rs | sh -s -- -y --profile minimal --default-toolchain ${RUST_VERSION} \
    && chmod -R a+rwX /opt/rustup /opt/cargo
ENV PATH=/usr/local/go/bin:/opt/cargo/bin:$PATH
