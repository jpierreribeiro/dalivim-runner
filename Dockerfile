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
#
# Pinned at 3.6 (was 3.4): 3.6's bundled kafel (submodule commit 76d0f41) is the
# first pin whose amd64 syscall table NAMES io_uring_setup/enter/register (425-427)
# and userfaultfd (323) — the escape-only syscalls the S2 denylist now KILLs. 3.4's
# kafel predated those names, so adding them by name failed the policy compile and,
# under RUNNER_SANDBOX=require, the boot probe closed (see internal/sandbox/nsjail.go
# and docs/future/security/S2-…). Build deps are unchanged: 3.6 needs the same
# autoconf/bison/flex/protobuf/libnl set (its pasta embedding is opt-in via
# EMBED_PASTA, which we do not set), and `make` still inits the kafel submodule.
FROM debian:bookworm-slim@sha256:60eac759739651111db372c07be67863818726f754804b8707c90979bda511df AS nsjail-build
ARG NSJAIL_VERSION=3.6
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates=20230311+deb12u1 git=1:2.39.5-0+deb12u3 autoconf=2.71-3 bison=2:3.8.2+dfsg-1+b1 flex=2.6.4-8.2 gcc=4:12.2.0-3 g++=4:12.2.0-3 libtool=2.4.7-7~deb12u1 make=4.3-4.1 pkg-config=1.8.1-1 \
      libprotobuf-dev=3.21.12-3+deb12u1 libnl-route-3-dev=3.7.0-0.2+b1 protobuf-compiler=3.21.12-3+deb12u1 \
 && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 --branch "${NSJAIL_VERSION}" https://github.com/google/nsjail /nsjail \
 && make -C /nsjail \
 && test -x /nsjail/nsjail

# ---- rust stage: pinned toolchain + musl target (G13) ----
# The official rust image is Debian-bookworm-based, so its rustc/std are ABI-matched
# to the runtime base below. We add the x86_64-unknown-linux-musl target (its std
# ships precompiled as rlibs) and copy the whole toolchain into the final image;
# rustc self-links the static musl binary via its bundled rust-lld — no musl-gcc
# needed. Pinned to an exact version for reproducibility (bump deliberately).
# TODO(pin-by-digest): capture `docker inspect` digest of rust:1.83.0-slim-bookworm
# and pin it here like the other bases, once CI resolves it.
FROM rust:1.83.0-slim-bookworm AS rust-build
RUN rustup target add x86_64-unknown-linux-musl

# ---- runtime stage: interpreters + nsjail + non-root user ----
# Bookworm base so the nsjail runtime libs (copied from the build stage above)
# match ABI. Interpreted runtimes: python3 (in this base) + nodejs (F-C) + lua5.4
# + sqlite3 (G10, the `sql` in-memory engine — Shape A, an interpreter over a
# .sql file). Compiled runtimes: gcc/g++ + libc6-dev for STATIC linking (F-D);
# Go (G2.1) via the toolchain copied from the build stage.
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
      libprotobuf32=3.21.12-3+deb12u1 libnl-route-3-200=3.7.0-0.2+b1 \
      nodejs=18.20.4+dfsg-1~deb12u2 \
      lua5.4=5.4.4-3+deb12u1 \
      sqlite3=3.40.1-2+deb12u2 \
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
# JUnit Platform Console Standalone (G9 Java test mode): one fat jar with the
# launcher + Jupiter/Vintage engines. A mode=test java request javac-compiles the
# submission against it, runs the launcher (which discovers @Test methods on the
# classpath), and returns the JUnit XML report raw. Pinned by version AND sha256 —
# ADD --checksum verifies the download at build (buildkit; the syntax directive at
# the top enables it), so a rebuild fetches a byte-identical jar or FAILS LOUDLY,
# the same reproducibility discipline as the apt/base pins.
ADD --checksum=sha256:33440476714985bda2584ed6c70d0d877085012343be67e961dcf80dac596227 \
    https://repo1.maven.org/maven2/org/junit/platform/junit-platform-console-standalone/1.11.3/junit-platform-console-standalone-1.11.3.jar \
    /opt/junit/junit-console.jar
# ADD from a URL yields a 0600 root-owned file. The run jail maps the runner's uid
# to 0 INSIDE its user namespace, but the kernel checks file access against the
# REAL (non-zero) uid, so a 0600 root file is unreadable to the jailed process
# ("javac: error reading …: Permission denied"). Make it world-readable so the
# jail-private uid can read it — the same `a+rX` treatment the pre-warmed GOCACHE
# gets below.
RUN chmod -R a+rX /opt/junit
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
# Rust toolchain (G13) for the `rust` compile jail. Copy the whole toolchain (rustc
# + the musl target std + rust-lld) and run rustc directly by absolute path — it
# self-finds its sysroot at ../lib/rustlib, so no rustup runtime is needed. Bound
# into the COMPILE jail only (full rootfs); the minimal-rootfs run jail executes the
# resulting static musl artifact and never sees the toolchain. World-readable so the
# jail-private uid can read it (same `a+rX` as GOCACHE / the JUnit jar).
COPY --from=rust-build /usr/local/rustup/toolchains/1.83.0-x86_64-unknown-linux-gnu /opt/rust
ENV PATH="/opt/rust/bin:${PATH}"
RUN chmod -R a+rX /opt/rust
# TypeScript compiler + Node type definitions (G14) for the `typescript` compile
# jail. TypeScript is a compile step that emits JavaScript (Shape C): the compile
# jail runs `node /opt/typescript/bin/tsc` to type-check + emit main.js, then the
# run jail runs the emitted JS on the SAME Node posture the `javascript` language
# uses (nodejs, installed above) — no second runtime. Two pinned tarballs, bundled
# offline (no `npm install` ever runs per request), the same "bundle the tool,
# pinned" choice G9 made for pytest / the JUnit jar:
#   - typescript: the tsc compiler (a Node program; lib/tsc.js).
#   - @types/node: the Node global/module type declarations, so console/process/
#     Buffer/require and the fs/net modules type-check (without them even
#     `console.log` is a type error under --lib ES2020). PURE .d.ts — no runtime
#     code, so it adds zero execution surface. Pinned to the 18.x line to match the
#     Node 18 the run jail actually runs. tsc compiles with --skipLibCheck, so
#     @types/node's own reference to the unbundled `undici-types` does not block emit.
# ADD --checksum verifies each download's sha256 at build (buildkit; the syntax
# directive at the top enables it), the same reproducibility discipline as the
# apt/base/jar pins — a rebuild fetches byte-identical tarballs or FAILS LOUDLY.
# The npm-registry tarballs unpack with a top-level `package/` dir (strip 1). Bound
# into the COMPILE jail only (full rootfs); world-readable so the jail-private uid
# can read them (same `a+rX` as the Go cache / JUnit jar / Rust toolchain).
ADD --checksum=sha256:10e108c9cf7d5f2879053dff18515fb405abf2ccef63eaaf017d9c571687a1d3 \
    https://registry.npmjs.org/typescript/-/typescript-5.9.3.tgz /opt/ts-src/typescript.tgz
ADD --checksum=sha256:30e36412dac091a634407127bea2c712c55de136e33c2d234b1529bb876418ca \
    https://registry.npmjs.org/@types/node/-/node-18.19.130.tgz /opt/ts-src/types-node.tgz
RUN set -eux; \
    mkdir -p /opt/typescript /opt/ts-types/node; \
    tar -xzf /opt/ts-src/typescript.tgz --strip-components=1 -C /opt/typescript; \
    tar -xzf /opt/ts-src/types-node.tgz --strip-components=1 -C /opt/ts-types/node; \
    rm -rf /opt/ts-src; \
    chmod -R a+rX /opt/typescript /opt/ts-types; \
    test -f /opt/typescript/bin/tsc; \
    test -f /opt/ts-types/node/index.d.ts
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
# NOTE: -trimpath below MUST match the real compiles (single-file goSpec.compile,
# multi-file compiled_multifile.go, AND the G9 `go test -trimpath` in testmode.go).
# It is part of Go's build-cache key, so a mismatch makes this warm cache useless →
# cold stdlib rebuild → compile timeout.
RUN set -eux; \
    mkdir -p /opt/gowarm; \
    printf 'package main\nimport (\n_ "bufio"\n_ "bytes"\n_ "container/heap"\n_ "container/list"\n_ "encoding/json"\n_ "errors"\n_ "fmt"\n_ "math"\n_ "math/rand"\n_ "net"\n_ "os"\n_ "regexp"\n_ "sort"\n_ "strconv"\n_ "strings"\n_ "sync"\n_ "testing"\n_ "testing/quick"\n_ "time"\n)\nfunc main(){}\n' > /opt/gowarm/warm.go; \
    cd /opt/gowarm; \
    CGO_ENABLED=0 GOCACHE=/opt/gocache GOPATH=/opt/gopath GOTOOLCHAIN=local GOENV=off go build -trimpath -o /dev/null warm.go; \
    # ALSO warm the `go test` compile path (G9 Go test mode). `go test` builds a TEST
    # binary whose artifacts `go build` never produces — testing/internal/testdeps,
    # internal/fuzz + its deps, os/signal, the generated test main — so without this a
    # first `go test` cold-rebuilds ~60 packages single-threaded and blows the wall.
    # Same -trimpath + toolchain so the keys match the real run (testmode.go).
    mkdir -p /opt/gowarmt; \
    printf 'module warm\n\ngo 1.23\n' > /opt/gowarmt/go.mod; \
    printf 'package warm\nimport "testing"\nfunc TestWarm(t *testing.T){ _ = 1 }\n' > /opt/gowarmt/warm_test.go; \
    cd /opt/gowarmt; \
    CGO_ENABLED=0 GOCACHE=/opt/gocache GOPATH=/opt/gopath GOTOOLCHAIN=local GOENV=off GOFLAGS=-mod=readonly go test -trimpath -count=1 ./... >/dev/null; \
    chmod -R a+rX /opt/gocache; \
    rm -rf /opt/gowarm /opt/gowarmt /opt/gopath
USER runner
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/runner"]
