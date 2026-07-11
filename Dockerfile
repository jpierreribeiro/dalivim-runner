# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Reproducible build — base images are PINNED by digest and every apt toolchain by
# EXACT version. A rebuild months from now installs byte-identical toolchains, so a
# student's output cannot drift with a silent upstream bump and the CVE surface is
# controlled (the trivy CI gate flags a pinned version that becomes vulnerable —
# that is the signal to refresh, not a reason to float).
#
# REFRESH (deliberate — on a security advisory, or a build that fails because a
# Debian point release rotated a pinned version out of the mirror):
#   1. Base digests:  docker pull <img>:<tag> \
#        && docker inspect --format '{{index .RepoDigests 0}}' <img>:<tag>
#   2. apt versions:  docker run --rm <base> sh -c \
#        'apt-get update -qq >/dev/null; apt-cache policy <pkg>'   # Candidate: line
#   3. Update the pins, rebuild, and let CI (build + nsjail smoke + trivy) verify.
# A pinned apt version that no longer exists FAILS the build LOUDLY — never silently
# floats — so the break itself is the reminder to refresh. Digests captured on the
# bookworm point release current at 2026-07-09.
# ---------------------------------------------------------------------------

# ---- build stage: compile a static Go binary ----
FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
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
FROM debian:bookworm-slim@sha256:60eac759739651111db372c07be67863818726f754804b8707c90979bda511df AS nsjail-build
ARG NSJAIL_VERSION=3.4
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates=20230311+deb12u1 git=1:2.39.5-0+deb12u3 autoconf=2.71-3 bison=2:3.8.2+dfsg-1+b1 flex=2.6.4-8.2 gcc=4:12.2.0-3 g++=4:12.2.0-3 libtool=2.4.7-7~deb12u1 make=4.3-4.1 pkg-config=1.8.1-1 \
      libprotobuf-dev=3.21.12-3 libnl-route-3-dev=3.7.0-0.2+b1 protobuf-compiler=3.21.12-3 \
 && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 --branch "${NSJAIL_VERSION}" https://github.com/google/nsjail /nsjail \
 && make -C /nsjail \
 && test -x /nsjail/nsjail

# ---- runtime stage: interpreters + nsjail + non-root user ----
# Bookworm base so the nsjail runtime libs (copied from the build stage above)
# match ABI. Interpreted runtimes: python3 (in this base) + nodejs (F-C) + lua5.4.
# Compiled runtimes: gcc/g++ + libc6-dev for STATIC linking (F-D); Go (G2.1) via
# the toolchain copied from the build stage.
FROM python:3.12-slim-bookworm@sha256:8a7e7cc04fd3e2bd787f7f24e22d5d119aa590d429b50c95dfe12b3abe52f48b
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
      libprotobuf32=3.21.12-3 libnl-route-3-200=3.7.0-0.2+b1 \
      nodejs=18.20.4+dfsg-1~deb12u2 \
      lua5.4=5.4.4-3+deb12u1 \
      gcc=4:12.2.0-3 g++=4:12.2.0-3 libc6-dev=2.36-9+deb12u14 \
      openjdk-17-jdk-headless=17.0.19+10-1~deb12u2 \
 && rm -rf /var/lib/apt/lists/*
# pytest for the Python test-runner mode (G9): a mode=test request runs
# `python3 -m pytest` over the submission and returns the JUnit XML report raw.
# Pinned exactly (like every apt package) so a rebuild installs a byte-identical
# framework and a student's report cannot drift with a silent upstream bump; a pin
# that no longer exists fails the build LOUDLY, which is the signal to refresh.
# Installed system-wide (world-readable site-packages) so the jail-private uid can
# import it. --no-compile keeps the layer free of root-owned .pyc; PYTEST_DISABLE_
# PLUGIN_AUTOLOAD=1 in the run env keeps third-party plugins off the test tree.
# NOTE: transitive deps (pluggy, iniconfig, packaging) ride along unpinned; a
# future hardening can hash-pin them via a requirements lock (see G9 / S3).
RUN pip install --no-cache-dir --no-compile pytest==8.3.4
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
# NOTE: -trimpath below MUST match the real compiles (single-file goSpec.compile
# and multi-file compiled_multifile.go). It is part of Go's build-cache key, so a
# mismatch makes this warm cache useless → cold stdlib rebuild → compile timeout.
RUN set -eux; \
    mkdir -p /opt/gowarm; \
    printf 'package main\nimport (\n_ "bufio"\n_ "bytes"\n_ "container/heap"\n_ "container/list"\n_ "encoding/json"\n_ "errors"\n_ "fmt"\n_ "math"\n_ "math/rand"\n_ "net"\n_ "os"\n_ "regexp"\n_ "sort"\n_ "strconv"\n_ "strings"\n_ "sync"\n_ "time"\n)\nfunc main(){}\n' > /opt/gowarm/warm.go; \
    cd /opt/gowarm; \
    CGO_ENABLED=0 GOCACHE=/opt/gocache GOPATH=/opt/gopath GOTOOLCHAIN=local GOENV=off go build -trimpath -o /dev/null warm.go; \
    chmod -R a+rX /opt/gocache; \
    rm -rf /opt/gowarm /opt/gopath
USER runner
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/runner"]
