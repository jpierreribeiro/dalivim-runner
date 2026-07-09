# syntax=docker/dockerfile:1

# ---- build stage: compile a static Go binary ----
FROM golang:1.26-alpine AS build
WORKDIR /app
# No external dependencies yet. When go.sum appears, add it here and run
# `go mod download` before copying source to keep the module cache layer cached.
COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY pkg/ ./pkg/
# CGO off + trimpath + stripped symbols → a small, portable, static binary.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /runner ./cmd/runner

# ---- nsjail stage: build the sandbox from source ----
# nsjail is not a reliable apt package on Debian, so we compile it from a pinned
# tag and copy only the binary + its runtime libs into the final image. Pinning
# the build to bookworm keeps the libprotobuf / libnl ABI matching the runtime.
FROM debian:bookworm-slim AS nsjail-build
ARG NSJAIL_VERSION=3.4
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git autoconf bison flex gcc g++ libtool make pkg-config \
      libprotobuf-dev libnl-route-3-dev protobuf-compiler \
 && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 --branch "${NSJAIL_VERSION}" https://github.com/google/nsjail /nsjail \
 && make -C /nsjail \
 && test -x /nsjail/nsjail

# ---- runtime stage: interpreters + nsjail + non-root user ----
# Bookworm base so the nsjail runtime libs (copied from the build stage above)
# match ABI. Interpreted runtimes: python3 (in this base) + nodejs (F-C).
# Compiled runtimes: gcc/g++ + libc6-dev for STATIC linking (F-D); Go (G2.1) via
# the toolchain copied from the build stage.
FROM python:3.12-slim-bookworm
# nsjail's runtime shared libraries (protobuf + libnl-route); the Node interpreter
# for the JavaScript runtime (bookworm's v18 supports --disable-proto=throw); the
# C/C++ toolchain; and a headless JDK for Java (G2.2 — javac + the JVM). libc6-dev
# provides libc.a + crt objects so `gcc -static` links a self-contained binary —
# which lets the run jail drop the whole rootfs (F-D). The C/C++/Go toolchains are
# bound into the COMPILE jail only (minimal-rootfs run jail). Java is the exception:
# the JVM is dynamically linked, so its RUN jail keeps the full rootfs (like the
# interpreters). openjdk-17 comes from bookworm main so its native libs are ABI-
# matched to this base. The jail execve's resolved abs paths.
RUN apt-get update && apt-get install -y --no-install-recommends \
      libprotobuf32 libnl-route-3-200 \
      nodejs \
      gcc g++ libc6-dev \
      openjdk-17-jdk-headless \
 && rm -rf /var/lib/apt/lists/*
# Non-root, no interactive login shell: the runner never needs a session, and
# dropping privileges shrinks the blast radius of any escape from a run. nsjail
# runs rootless (unprivileged user namespaces), so no elevated caps are needed.
RUN useradd --create-home --shell /usr/sbin/nologin runner
# Pre-create the jail's bind-mount target. nsjail bind-mounts the host rootfs
# READ-ONLY as the jail root (see nsjail.go: `--bindmount_ro /`), then bind-mounts
# each run's WorkDir onto /sandbox. The mountpoint must already exist on that
# read-only rootfs: nsjail cannot mkdir it (the root is RO), so a missing
# /sandbox fails every jail launch with "Couldn't mount '/sandbox'" — on the
# target as well as in CI. An empty dir is all a bind-mount target needs.
RUN mkdir /sandbox
COPY --from=build       /runner         /usr/local/bin/runner
COPY --from=nsjail-build /nsjail/nsjail /usr/local/bin/nsjail
# Go toolchain (G2.1) for the `go` compile jail. The go command and its sub-tools
# are statically-linked Go programs, portable from the alpine build stage to this
# bookworm base; CGO_ENABLED=0 builds never touch the host libc. The toolchain is
# bound into the COMPILE jail only (read-only rootfs); the minimal-rootfs run jail
# runs the resulting static artifact and never sees it.
COPY --from=build       /usr/local/go   /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"
# Determinism pin (G7), belt-and-suspenders: the jail sets these explicitly in
# every run env (an explicit minimal cmd.Env means this ENV does NOT reach the
# child), so this line only pins the daemon itself and any image-level tooling.
# C.UTF-8 is built into glibc — no locale package needed; the base image has no
# /etc/localtime, so TZ=UTC turns the accidental UTC default into policy.
ENV LANG=C.UTF-8 LC_ALL=C.UTF-8 TZ=UTC
# Pre-warm a read-only Go build cache (G2.1) covering the common stdlib a judged
# program imports. Each run gets a FRESH tmpfs GOCACHE, so without this every Go
# compile would pay a ~14 s cold stdlib rebuild (over the compile budget). The Go
# compile jail seeds its writable /tmp/gocache from this (see goSpec); a warm build
# is ~0.3 s. World-readable so the jail-private uid can copy it.
RUN set -eux; \
    mkdir -p /opt/gowarm; \
    printf 'package main\nimport (\n_ "bufio"\n_ "bytes"\n_ "container/heap"\n_ "container/list"\n_ "encoding/json"\n_ "errors"\n_ "fmt"\n_ "math"\n_ "math/rand"\n_ "net"\n_ "os"\n_ "regexp"\n_ "sort"\n_ "strconv"\n_ "strings"\n_ "sync"\n_ "time"\n)\nfunc main(){}\n' > /opt/gowarm/warm.go; \
    cd /opt/gowarm; \
    CGO_ENABLED=0 GOCACHE=/opt/gocache GOPATH=/opt/gopath GOTOOLCHAIN=local GOENV=off go build -o /dev/null warm.go; \
    chmod -R a+rX /opt/gocache; \
    rm -rf /opt/gowarm /opt/gopath
USER runner
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/runner"]
