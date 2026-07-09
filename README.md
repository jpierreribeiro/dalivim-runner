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
                       languages.go = closed languageSpec registry (python, js);
                       interpreted.go = one spec-driven runtime both share
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

`status` ∈ `success | runtime_error | timeout | memory_exceeded | compile_error |
internal_error`. Only `language` and `source_code` are required; each limit falls
back to the service default and is clamped to the hard ceiling. Unknown
language → `400`.

**Languages:** `python`, `javascript` (interpreted), `c`, `cpp` (compiled). A
compiled request may set `compile_timeout_ms` (bounds the compile phase, separate
from `timeout_ms`); a failed compile returns `status: compile_error` with the
compiler diagnostics in `compile_output`, and nothing is executed. `signal` names
the signal that killed a run (e.g. `SIGSEGV`) when it died by one.

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
- **Memory:** an address-space cap (`RLIMIT_AS`) plus, where a cgroup v2 subtree
  is delegated (`RUNNER_CGROUP`, F-E/R6), a per-run `memory.max` — authoritative
  RSS accounting that also classifies `memory_exceeded` from the kernel OOM event.
- **Overload:** a bounded number of executions run at once
  (`RUNNER_MAX_CONCURRENT_RUNS`); excess requests are shed immediately with `503`
  + `Retry-After` so the caller can fall back instead of the container being
  driven into swap/OOM.
- **Infra failover:** if the runner cannot stand up the sandbox for a run
  (`status: internal_error`), it replies `503` + `Retry-After`, not a terminal
  `200`, so the gateway treats it as a provider failure and can fall back. Student
  outcomes (`success`/`runtime_error`/`timeout`/`memory_exceeded`) stay `200`.
- **Fork bombs:** contained **per-run** by the nsjail backend (`--rlimit_nproc`
  against a jail-private uid), never a process-wide `RLIMIT_NPROC` — a global cap
  is enforced per real-uid and would throttle the runner itself on a busy host
  (`errno=11`). On the netns fallback, containment is the bounded concurrency cap
  + CPU/wall limits.
- **Filesystem:** throwaway temp dir per run; under nsjail it is mounted
  **read-only** at `/sandbox` with writes confined to a size-capped tmpfs `/tmp`;
  the source is written to a file (never the command line); restricted PATH/env;
  runs as a non-root uid. Interpreted runtimes are single-file: `python3 -I` and
  `node --disable-proto=throw` (no npm / `node_modules`).
- **Compiled languages (C/C++), two jails (F-D):** the compiler is itself hostile
  input (template/macro bombs, recursive `#include`), so it runs in its own jail —
  read-only rootfs, no network, a `-static` link, and its own CPU/mem/time budget
  (`RUNNER_COMPILE_*`); a non-zero exit or compile timeout is `compile_error` and
  **nothing runs**. The validated static artifact then executes in a **separate,
  stricter run jail with a *minimal rootfs*** — only the binary + tmpfs `/tmp`, no
  libc, and crucially **no toolchain**, so a submission cannot re-invoke the
  compiler or exec anything else at runtime (D-4). The static run jail can also
  take a **tight seccomp allowlist** (`RUNNER_STATIC_SECCOMP=enforce`) — only the
  minimal syscall set a self-contained program needs, DEFAULT KILL on everything
  else (no `socket`/`open`/`openat`/`ptrace`/`clone`/`mount`) — far stronger than
  the shared interpreter denylist. (`execve` is permitted for one structural
  reason: nsjail installs the filter then execve's the payload, so denying it
  would stop the binary launching; it's neutered by the minimal rootfs — nothing
  else to exec, and no `open`/`openat` to create one.) It ships behind a dial
  (default `off`); a `complain` mode logs violations without killing, for pinning
  the set on the target first.
- **Memory:** CPython runs under a hard `RLIMIT_AS` (a memory bomb → deterministic
  `memory_exceeded`). Node cannot — V8 reserves a multi-GB virtual cage at startup
  that a tight `RLIMIT_AS` refuses — so Node skips the address-space cap and bounds
  its heap with `--max-old-space-size`. When a cgroup is delegated (`RUNNER_CGROUP`,
  F-E/R6) both runtimes additionally get a per-run `memory.max` — a true RSS
  ceiling that finally bounds Node hard, and `memory_exceeded` is then read from
  the cgroup OOM event rather than a stderr substring. Without a delegated cgroup
  the runner falls back to the rlimit/heap bound exactly as before.

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
| `RUNNER_CGROUP` | `auto` | Per-run **cgroup v2** memory/pids accounting under the nsjail backend (F-E/R6). `auto`: use the delegated subtree when usable, else fall back to `RLIMIT_AS`/heap-flag bounds. `require`: **fail closed at boot** if no usable cgroup. `off`: rlimit-only. When on, `memory.max`/`pids.max` bound each run and `memory_exceeded` is read from the kernel OOM event, not stderr. |
| `RUNNER_CGROUP_MOUNT` | `/sys/fs/cgroup/dalivim` | Delegated, writable cgroup v2 subtree the runner creates per-run leaves under (see `docs/DEPLOY.md` → cgroup delegation). Only consulted under `RUNNER_CGROUP=auto\|require`. |
| `RUNNER_MAX_CONCURRENT_RUNS` | `8` | Max simultaneous executions; excess requests get `503` + `Retry-After`. `0` disables the limit. |
| `RUNNER_MAX_PROCESSES` | `256` | Per-run process cap (`RLIMIT_NPROC`) applied by the nsjail backend against a jail-private uid; never applied process-wide (see Security → Fork bombs). |
| `RUNNER_MAX_FILE_SIZE_MB` | `64` | Per-run file-size cap (`RLIMIT_FSIZE`) applied by the nsjail backend. |
| `RUNNER_PORT` / `PORT` | `8090` | Listen port (`PORT` is the platform-injected fallback). |
| `RUNNER_DEFAULT_TIMEOUT_MS` / `RUNNER_MAX_TIMEOUT_MS` | `3000` / `10000` | Per-run wall-clock default + hard cap. |
| `RUNNER_DEFAULT_MEMORY_MB` / `RUNNER_MAX_MEMORY_MB` | `128` / `512` | Per-run memory default + hard cap. |
| `RUNNER_MAX_SOURCE_BYTES` | `200000` | Max accepted `source_code` size. |
| `RUNNER_MAX_OUTPUT_BYTES` | `65536` | Per-stream stdout/stderr capture cap. |
| `RUNNER_COMPILE_TIMEOUT_MS` / `RUNNER_MAX_COMPILE_TIMEOUT_MS` | `10000` / `20000` | Compiled languages: compile-phase wall/CPU budget + hard cap (separate from execution). |
| `RUNNER_COMPILE_MEMORY_MB` | `512` | Compiled languages: `RLIMIT_AS`/cgroup for the compiler (tames template/macro bombs). |
| `RUNNER_MAX_ARTIFACT_MB` | `32` | Compiled languages: reject a compiled artifact larger than this. |
| `RUNNER_STATIC_SECCOMP` | `off` | Compiled **run** jail seccomp: `off` = shared denylist; `enforce` = tight static-binary allowlist (SIGSYS on anything unlisted); `complain` = allowlist logged, not killed (for tuning the set on the target). |

## Run locally

```sh
RUNNER_ENV=development go run ./cmd/runner    # python3 / node on PATH to run those

curl -s localhost:8090/run \
  -H 'content-type: application/json' \
  -d '{"language":"python","source_code":"print(2+2)"}'

curl -s localhost:8090/run \
  -H 'content-type: application/json' \
  -d '{"language":"javascript","source_code":"console.log(2+2)"}'
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
  syscall, output flood, egress + cloud-metadata, CPU spin, memory bomb, host
  write — each must be contained and the runner must survive. A **backpressure**
  check (`scripts/smoke-stress.sh`, a `RUNNER_MAX_CONCURRENT_RUNS=1` container)
  then proves saturation is shed with `503` + `Retry-After`.

### Why the smoke container relaxes Docker's own sandbox

nsjail builds each per-run jail by creating an unprivileged **user namespace**
and `mount`-ing a read-only rootfs + tmpfs `/tmp`. Three things block that on a
stock GitHub `ubuntu-latest` (24.04) runner, so the smoke job clears exactly
these — no more:

| Relaxation | Scope | Why nsjail needs it |
|---|---|---|
| `--security-opt seccomp=unconfined` | container | Docker's default seccomp profile blocks the `CLONE_NEWUSER` unshare nsjail uses to create its user namespace |
| `--security-opt apparmor=unconfined` | container | the docker-default AppArmor profile denies the `mount` ops nsjail performs for the read-only rootfs + tmpfs `/tmp` |
| `sysctl kernel.apparmor_restrict_unprivileged_userns=0` | runner **host** | Ubuntu 24.04 strips `CAP_SYS_ADMIN` inside unprivileged userns (→ `mount('/','/'): EPERM`); a host-kernel global that a container `--security-opt` cannot lift |

Those apply **only to the runner daemon's container and the CI host**. They add
**no capabilities**, the container still runs as the non-root `runner` user, and
they do **not** touch the inner per-run jail: every submission still executes
inside its own user/pid/net/mount namespaces, a read-only rootfs, a size-capped
tmpfs `/tmp`, the seccomp **denylist**, `no_new_privs`, and per-jail
`RLIMIT_NPROC`/`FSIZE`. The escape corpus is the proof the inner jail is intact.
In production (Railway) the runner is the container's only workload, so the same
posture is correct — the meaningful boundary is the per-run jail, not Docker's
generic profile around a single-purpose daemon.

`scripts/smoke-run.sh` (`POST /run`, exact stdout+status assertion, non-zero on
mismatch) is the low-level helper both the happy path and the corpus reuse.

## Container

```sh
docker build -t dalivim-runner .
```

Multi-stage: a static Go binary + `nsjail` compiled from source, on
`python:3.12-slim-bookworm`, running as a non-root user (nsjail runs rootless, so
no elevated capabilities are required).

> **Full step-by-step deploy runbook:** [docs/DEPLOY.md](docs/DEPLOY.md) —
> provision → build → run → verify → HTTPS + firewall → wire the backend,
> validated end-to-end on a root VPS. The section below is the quick version.

### Production run (VPS / target)

The service **fails closed**: without `RUNNER_SERVICE_TOKEN` (and outside
`development`) it refuses to boot with
`RUNNER_SERVICE_TOKEN is required outside development`. That is the security gate,
not a crash — set the token. Use the **same** value on the gateway
(`RUNNER_SERVICE_TOKEN`), which sends it as `X-Runner-Token`.

```sh
# 1. Generate a strong shared secret (once; store it in your secret manager)
openssl rand -hex 32

# 2. Run fail-closed on the sandbox too, so a broken jail stops the boot instead
#    of silently downgrading to netns.
docker run -d --name dalivim-runner -p 8090:8090 \
  -e RUNNER_SERVICE_TOKEN=<the-generated-secret> \
  -e RUNNER_SANDBOX=require \
  --pids-limit=512 --ulimit nproc=512 --cpus=1 --memory=512m \
  dalivim-runner

# 3. Confirm the jail actually engaged (this is the F-B acceptance criterion)
docker logs dalivim-runner 2>&1 | grep -E "nsjail (ENABLED|unavailable)|network isolation"
```

- `nsjail ENABLED …` → the jail is active (read-only rootfs, tmpfs `/tmp`, seccomp,
  per-jail rlimits). 
- `RUNNER_SANDBOX=require but nsjail is unavailable: <detail>` → the boot probe ran
  `/bin/true` in a real jail and it failed; the text after the colon is why (see
  troubleshooting below).

For local development only (no token gate, no containment guarantees):

```sh
docker run --rm -e RUNNER_ENV=development -p 8090:8090 dalivim-runner
```

### Troubleshooting: nsjail unavailable / `require` fails to boot

nsjail rootless needs **unprivileged user namespaces**, and inside Docker the
default seccomp profile can block the `clone(CLONE_NEWUSER)` it relies on. If a
`RUNNER_SANDBOX=require` boot fails (or `auto` logs a fallback to netns), check, on
the VPS host:

```sh
# 1. Kernel must allow unprivileged userns (1, or the knob may be absent on newer kernels)
sysctl kernel.unprivileged_userns_clone 2>/dev/null; cat /proc/sys/user/max_user_namespaces

# 2. Prove it outside Docker first — this should print "ok" with no error
unshare --user --net --map-root-user /bin/true && echo ok
```

If the host allows userns but the **container** doesn't, relax only the container's
seccomp so nsjail can create the namespace — the *jailed run* still enforces its
own seccomp/rootfs/rlimits inside:

```sh
docker run -d --name dalivim-runner -p 8090:8090 \
  -e RUNNER_SERVICE_TOKEN=<secret> -e RUNNER_SANDBOX=require \
  --security-opt seccomp=unconfined \
  --pids-limit=512 --ulimit nproc=512 --cpus=1 --memory=512m \
  dalivim-runner
```

> `--security-opt seccomp=unconfined` loosens the **outer** container so nsjail can
> build the jail; it does **not** loosen the sandbox around student code. Never add
> `--privileged` — nsjail does not need it, and it would defeat the isolation.
> If the host itself forbids unprivileged userns (`max_user_namespaces=0` or a
> hardened kernel), enable it (`sysctl -w kernel.unprivileged_userns_clone=1`,
> `sysctl -w user.max_user_namespaces=15000`) or run on a host/VM that allows it —
> nsjail cannot be made to work rootless without it.

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
