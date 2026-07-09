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

# ---- runtime stage: python interpreter + nsjail + non-root user ----
# Bookworm base so the nsjail runtime libs (copied from the build stage above)
# match ABI. Python is the only wired runtime for MVP; Node/gcc land with F-C/F-D.
FROM python:3.12-slim-bookworm
# nsjail's runtime shared libraries (protobuf + libnl-route). Nothing else: the
# jail exec's python3, already present in this base.
RUN apt-get update && apt-get install -y --no-install-recommends \
      libprotobuf32 libnl-route-3-200 \
 && rm -rf /var/lib/apt/lists/*
# Non-root, no interactive login shell: the runner never needs a session, and
# dropping privileges shrinks the blast radius of any escape from a run. nsjail
# runs rootless (unprivileged user namespaces), so no elevated caps are needed.
RUN useradd --create-home --shell /usr/sbin/nologin runner
COPY --from=build       /runner         /usr/local/bin/runner
COPY --from=nsjail-build /nsjail/nsjail /usr/local/bin/nsjail
USER runner
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/runner"]
