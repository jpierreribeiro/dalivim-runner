# dalivim-runner

Isolated, language-agnostic **code execution microservice**. It receives source
code over HTTP, executes it in a hostile-boundary sandbox (timeouts, output caps,
best-effort memory limits, empty network namespace, fork-bomb cap, non-root uid),
and returns a structured result. It is **not** an LMS and holds no domain logic —
it is an executor. Untrusted code never runs inside the calling API's process; it
runs here, in a dedicated container on an internal-only network.

Extracted from the `dalivim-backend` monolith (was `runner-service/`) into a
standalone repo and restructured to clean architecture.

## Layout

```
cmd/runner/            composition root (config → sandbox → executor → server)
internal/
  config/              env parsing + validation (fail-closed on missing token)
  sandbox/             OS-level containment (netns, RLIMIT_NPROC, pgroup kill)
                       sandbox_linux.go = real; sandbox_other.go = dev stub
  executor/            language-agnostic core: dispatch, limit clamping, runtimes
                       python.go = the only wired runtime today
  transport/httpapi/   HTTP server, routing, PSK middleware, handlers
pkg/runnerapi/         public wire contract (importable by the gateway)
```

Dependency direction points inward: `httpapi → executor → {sandbox, runnerapi}`.
The executor has no HTTP or syscall knowledge; the sandbox holds all the
security-sensitive syscall code behind a small API — the seam where future
hardening (nsjail, cgroups, seccomp) plugs in.

## API

### `POST /run` (canonical, language-agnostic)

```json
{
  "language": "python",
  "source_code": "print('hello')",
  "stdin": "",
  "timeout_ms": 3000,
  "memory_mb": 128
}
```

Response:

```json
{
  "status": "success",
  "stdout": "hello\n",
  "stderr": "",
  "exit_code": 0,
  "duration_ms": 41,
  "memory_kb": 18400,
  "runtime_name": "python",
  "runtime_version": "3.12.3",
  "python_version": "3.12.3"
}
```

`status` ∈ `success | runtime_error | timeout | memory_exceeded | internal_error`.
Only `language` and `source_code` are required; each limit falls back to the
service default and is clamped to the hard ceiling. Unknown language → `400`.

`python_version` is a **deprecated** alias of `runtime_version`, kept so the
current gateway adapter works unchanged. New callers should read
`runtime_version` / `runtime_name`.

### `POST /run/python` (deprecated alias)

Same body without `language` (python is injected). Kept so the pre-extraction
backend keeps working with only an env change. Prefer `POST /run`.

### `GET /healthz`

Unauthenticated liveness probe → `200 ok`.

## Security

- **Zero-trust gate:** every `/run*` request must send the pre-shared
  `X-Runner-Token`; compared in constant time. The service **refuses to boot**
  without `RUNNER_SERVICE_TOKEN` unless `RUNNER_ENV=development`.
- **Network:** each run executes in an empty network namespace (unprivileged
  user+net namespaces) → no egress, independent of the deploy network.
- **CPU/wall:** wall-clock deadline + `ulimit -t`; the whole process group is
  SIGKILLed on timeout.
- **Memory:** best-effort address-space cap via `ulimit -v`.
- **Fork bombs:** `RLIMIT_NPROC` capped process-wide.
- **Filesystem:** throwaway temp dir per run; `python3 -I`; restricted PATH/env;
  runs as a non-root uid.

> Linux-only by design: the isolation guarantees depend on Linux namespaces and
> rlimits. `sandbox_other.go` lets the service build/run on other OSes for local
> development **with no containment** — never deploy off Linux.

## Configuration

| Env | Default | Effect |
|---|---|---|
| `RUNNER_SERVICE_TOKEN` | — | Shared secret; callers send `X-Runner-Token`. **Required** unless `RUNNER_ENV=development`. |
| `RUNNER_ENV` | (unset → strict) | `development` allows booting without a token. Leave unset in production. |
| `RUNNER_NETWORK_ISOLATION` | `auto` | `auto`: empty netns when permitted, else warn + fall back. `require`: fail closed at boot. `off`: disable. |
| `RUNNER_MAX_PROCESSES` | `256` | Per-uid process cap (fork-bomb containment). |
| `RUNNER_PORT` / `PORT` | `8090` | Listen port (`PORT` is the platform-injected fallback). |
| `RUNNER_DEFAULT_TIMEOUT_MS` / `RUNNER_MAX_TIMEOUT_MS` | `3000` / `10000` | Per-run wall-clock default + hard cap. |
| `RUNNER_DEFAULT_MEMORY_MB` / `RUNNER_MAX_MEMORY_MB` | `128` / `512` | Per-run memory default + hard cap. |
| `RUNNER_MAX_SOURCE_BYTES` | `200000` | Max accepted `source_code` size. |
| `RUNNER_MAX_OUTPUT_BYTES` | `65536` | Per-stream stdout/stderr capture cap. |

## Run locally

```sh
RUNNER_ENV=development go run ./cmd/runner    # requires python3 on PATH

curl -s localhost:8090/run \
  -H 'content-type: application/json' \
  -d '{"language":"python","source_code":"print(2+2)"}'
```

## Test

```sh
go test ./...
```

Execution tests skip cleanly when `python3` is absent; network-namespace tests
skip when the platform forbids unprivileged namespaces.

## Container

```sh
docker build -t dalivim-runner .
docker run --rm -e RUNNER_SERVICE_TOKEN=dev -p 8090:8090 dalivim-runner
```

Multi-stage, static Go binary on `python:3.12-slim`, running as a non-root user.

## Gateway integration (the main API)

The backend already speaks this contract via `internal/runner` (client →
`local.Adapter` → `Gateway`). Because `POST /run/python` is preserved, **no code
change is required** to cut over — repoint the URL:

```
RUNNER_SERVICE_URL=http://<dalivim-runner-host>:8090
RUNNER_SERVICE_TOKEN=<same secret set on this service>
```

Recommended follow-up (a small adapter change, not required for cutover): switch
the client to `POST /run` with `"language": "python"` and read `runtime_version`
instead of `python_version`, then drop the deprecated alias here.
