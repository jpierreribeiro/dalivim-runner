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
  sandbox/             OS-level containment behind Sandbox.Command(Spec): the
                       nsjail and netns backends + the RUNNER_SANDBOX dial/probe.
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

A Runtime only says *what* to run (argv, workdir, limits) via a `sandbox.Spec`;
the **sandbox backend** decides *how* to contain it. Two Linux backends sit behind
the `RUNNER_SANDBOX` dial, resolved once at boot with a live probe:

| Backend | Selected when | Adds on top |
|---|---|---|
| **nsjail** | `RUNNER_SANDBOX=auto` (probe passes) or `require` | read-only rootfs, own mount/pid/ipc/user/net namespaces, size-capped tmpfs `/tmp`, **seccomp** denylist, `no_new_privs`, per-jail `RLIMIT_NPROC`/`FSIZE` |
| **netns** (F-03 baseline) | `RUNNER_SANDBOX=off`, or `auto` when nsjail is unavailable | empty network namespace + per-run `RLIMIT_AS`/`RLIMIT_CPU` via the child shell |

`auto` falls back to netns with a loud warning if the platform can't run nsjail;
`require` **fails closed at boot** instead. The boot log (`nsjail ENABLED` vs
`falling back to netns-only`) is the source of truth — nsjail can't be exercised
in CI (it needs the binary + unprivileged user namespaces), so a verification
deploy with `RUNNER_SANDBOX=require` is how you prove it engaged.

- **Zero-trust gate:** every `/run*` request must send the pre-shared
  `X-Runner-Token`; compared in constant time. The service **refuses to boot**
  without `RUNNER_SERVICE_TOKEN` unless `RUNNER_ENV=development`.
- **Network:** each run executes in an empty network namespace → no egress,
  independent of the deploy network. nsjail also brings loopback down.
- **Syscalls (nsjail only):** a seccomp denylist kills the syscalls a sandbox
  escape needs — `ptrace`, `mount`/`unshare`/`setns`, `bpf`, kernel-module loads,
  `keyctl`, `reboot`/`swapon`, `*_handle_at`, `perf_event_open`.
- **CPU/wall:** wall-clock deadline (Go context + nsjail `--time_limit`) plus an
  `RLIMIT_CPU` cap just above it; the whole process group is SIGKILLed on timeout.
- **Memory:** best-effort address-space cap (`RLIMIT_AS`). Authoritative per-run
  accounting via cgroups is future work (see the hardening plan's F-E/F-F).
- **Overload:** a bounded number of executions run at once
  (`RUNNER_MAX_CONCURRENT_RUNS`); excess requests are shed immediately with `503`
  + `Retry-After` so the caller can fall back instead of the container being
  driven into swap/OOM.
- **Fork bombs:** contained **per-run** by the nsjail backend (`--rlimit_nproc`
  against a jail-private uid), never a process-wide `RLIMIT_NPROC` — a global cap
  is enforced per real-uid and would throttle the runner itself on a busy host
  (`errno=11`). On the netns fallback, containment is the bounded concurrency cap
  + CPU/wall limits.
- **Filesystem:** throwaway temp dir per run; under nsjail it is mounted
  **read-only** at `/sandbox` with writes confined to a size-capped tmpfs `/tmp`;
  `python3 -I`; restricted PATH/env; runs as a non-root uid.

> Linux-only by design: the isolation guarantees depend on Linux namespaces and
> rlimits. `sandbox_other.go` lets the service build/run on other OSes for local
> development **with no containment** — never deploy off Linux.

## Configuration

| Env | Default | Effect |
|---|---|---|
| `RUNNER_SERVICE_TOKEN` | — | Shared secret; callers send `X-Runner-Token`. **Required** unless `RUNNER_ENV=development`. |
| `RUNNER_ENV` | (unset → strict) | `development` allows booting without a token. Leave unset in production. |
| `RUNNER_SANDBOX` | `auto` | Selects the containment backend. `auto`: use nsjail when its boot probe passes, else fall back to netns. `require`: nsjail only — **fail closed at boot** if unavailable. `off`: netns backend only. |
| `RUNNER_NETWORK_ISOLATION` | `auto` | Governs the **netns** backend's egress guarantee (ignored when nsjail is active, which always isolates the network). `auto`: empty netns when permitted, else warn + fall back. `require`: fail closed at boot. `off`: disable. |
| `RUNNER_MAX_CONCURRENT_RUNS` | `8` | Max simultaneous executions; excess requests get `503` + `Retry-After`. `0` disables the limit. |
| `RUNNER_MAX_PROCESSES` | `256` | Per-run process cap (`RLIMIT_NPROC`) applied by the nsjail backend against a jail-private uid; never applied process-wide (see Security → Fork bombs). |
| `RUNNER_MAX_FILE_SIZE_MB` | `64` | Per-run file-size cap (`RLIMIT_FSIZE`) applied by the nsjail backend. |
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

## Test / CI

```sh
go vet ./...
go test -race ./...
```

Execution tests skip cleanly when `python3` is absent; network-namespace tests
skip when the platform forbids unprivileged namespaces.

CI (`.github/workflows/ci.yml`) has two jobs:

- **`test`** — `go vet` + `go test -race ./...` (the unit suite: arg builder,
  sandbox dial state machine, transport, executor).
- **`runner-smoke`** — builds the Docker image (nsjail compiled from source),
  boots it under **target-like limits** (`--pids-limit`, `--cpus=1`,
  `--memory=512m`, `GOMAXPROCS=1`) with **`RUNNER_SANDBOX=require`** so a failed
  nsjail boot probe **crashes the container** instead of downgrading, asserts the
  boot log shows `nsjail ENABLED`, hits `/healthz`, then does a real
  `POST /run` for `print(2+2)` and the adversarial **escape corpus**
  (`scripts/smoke-escape.sh`): fork bomb, secret/host-env read, seccomp-killed
  syscall, output flood — each must be contained and the runner must survive.

### Why the smoke container relaxes Docker's own sandbox

nsjail builds each per-run jail by creating an unprivileged **user namespace**
and `mount`-ing a read-only rootfs + tmpfs `/tmp`. Docker's *default* seccomp
profile blocks the `CLONE_NEWUSER` unshare, and the default AppArmor profile
denies those mounts — so nsjail cannot start unless the **outer** container is
run with `--security-opt seccomp=unconfined --security-opt apparmor=unconfined`.

That relaxation applies **only to the runner daemon's container**. It adds **no
capabilities**, the container still runs as the non-root `runner` user, and it
does **not** touch the inner per-run jail: every submission still executes inside
its own user/pid/net/mount namespaces, a read-only rootfs, a size-capped tmpfs
`/tmp`, the seccomp **denylist**, `no_new_privs`, and per-jail `RLIMIT_NPROC`/
`FSIZE`. The escape corpus is the proof the inner jail is intact. In production
(Railway) the runner is the container's only workload, so the same relaxation is
the correct posture — the meaningful boundary is the per-run jail, not Docker's
generic profile around a single-purpose daemon.

`scripts/smoke-run.sh` (`POST /run`, exact stdout+status assertion, non-zero on
mismatch) is the low-level helper both the happy path and the corpus reuse.

## Container

```sh
docker build -t dalivim-runner .
docker run --rm -e RUNNER_SERVICE_TOKEN=dev -p 8090:8090 dalivim-runner
```

Multi-stage: a static Go binary + `nsjail` compiled from source, on
`python:3.12-slim-bookworm`, running as a non-root user (nsjail runs rootless, so
no elevated capabilities are required). Locally, without the nsjail binary the
service boots on the netns backend; set `RUNNER_SANDBOX=require` in a verification
deploy to confirm nsjail engaged.

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
