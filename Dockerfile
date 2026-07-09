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

# ---- runtime stage: python interpreter + non-root user ----
# python:slim carries the interpreter the runner exec's. nsjail / compilers are
# deliberately out of scope for this first packaging step.
FROM python:3.12-slim
# Non-root, no interactive login shell: the runner never needs a session, and
# dropping privileges shrinks the blast radius of any escape from a run.
RUN useradd --create-home --shell /usr/sbin/nologin runner
COPY --from=build /runner /usr/local/bin/runner
USER runner
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/runner"]
