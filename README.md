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
  **read-only** at `/mnt` with writes confined to a size-capped tmpfs `/tmp`;
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

## Test

```sh
go test ./...
```

Execution tests skip cleanly when `python3` is absent; network-namespace tests
skip when the platform forbids unprivileged namespaces.

## Container

```sh
docker build -t dalivim-runner .
```

Multi-stage: a static Go binary + `nsjail` compiled from source, on
`python:3.12-slim-bookworm`, running as a non-root user (nsjail runs rootless, so
no elevated capabilities are required).

> **Full step-by-step deploy runbook:** [docs/DEPLOY.md](docs/DEPLOY.md) — provision
> → build → run → verify → HTTPS + firewall → wire the backend. The section below
> is the quick version.

### Production run (VPS / target)

The service **fails closed**: without `RUNNER_SERVICE_TOKEN` (and outside
`development`) it refuses to boot with
`RUNNER_SERVICE_TOKEN is required outside development`. That is the security gate,
not a crash — set the token. Use the **same** value on the gateway
(`RUNNER_SERVICE_TOKEN`), which sends it as `X-Runner-Token`.

Run this on a host you control (a root VPS), **not** a managed container PaaS:
nsjail rootless needs unprivileged user namespaces, and a hardened PaaS
(Kubernetes-based) typically blocks the `clone(CLONE_NEWUSER)` it relies on, so
the jail can't engage there.

```sh
# 1. Host: allow unprivileged user namespaces (once). Ubuntu 23.10+ restricts
#    them via AppArmor by default; Debian uses a different knob.
sudo tee /etc/sysctl.d/99-userns.conf >/dev/null <<'EOF'
kernel.apparmor_restrict_unprivileged_userns=0
user.max_user_namespaces=15000
EOF
# Debian: replace the first line with  kernel.unprivileged_userns_clone=1
sudo sysctl --system

# 2. Generate a strong shared secret (once; store it in your secret manager)
openssl rand -hex 32

# 3. Run it. --security-opt seccomp=unconfined/apparmor=unconfined loosen the
#    OUTER container just enough for nsjail to create the namespaces; the jail
#    around student code keeps its own seccomp/rootfs/rlimits (see the note below).
#    RUNNER_SANDBOX=require fails the boot closed if the jail can't engage.
docker run -d --name dalivim-runner -p 127.0.0.1:8090:8090 \
  -e RUNNER_SERVICE_TOKEN=<the-generated-secret> \
  -e RUNNER_SANDBOX=require \
  -e RUNNER_NETWORK_ISOLATION=require \
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
  --pids-limit=512 --ulimit nproc=512 --cpus=1 --memory=1g \
  dalivim-runner

# 4. Confirm the jail actually engaged (this is the F-B acceptance criterion)
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

> **Why the `--security-opt` flags?** Docker's default seccomp profile blocks
> `clone` with the `CLONE_NEW*` namespace flags for containers without
> `CAP_SYS_ADMIN`, and Ubuntu's default AppArmor profile restricts unprivileged
> userns — either one stops nsjail from *building* the jail. Setting both to
> `unconfined` loosens only the **outer supervisor** container (our trusted code);
> the sandbox around student code still enforces its own seccomp denylist,
> read-only rootfs, and rlimits. **Never** use `--privileged` — nsjail does not
> need it and it would defeat the isolation.

### Troubleshooting: `RUNNER_SANDBOX=require` fails to boot

The boot probe runs `/bin/true` in a real jail; the log line after the colon is
the exact nsjail error. Common ones seen on real targets:

| nsjail error | Cause | Fix |
|---|---|---|
| `clone(...CLONE_NEWUSER...) Operation not permitted` | the container/host blocks unprivileged userns | add the two `--security-opt ... unconfined` flags; set the host sysctls in step 1; on a managed PaaS this usually can't be fixed — move to a root VPS |
| `execve('/usr/bin/newgidmap'): No such file or directory` | (pre-fix) an explicit uid/gid mapping forced the setuid helpers | fixed: the runner no longer passes `--uid_mapping/--gid_mapping`, so nsjail self-maps without the helper |
| `Could not compile policy: Undefined identifier '<x>'` | a seccomp syscall name absent from kafel's table | the denylist uses only names present in kafel's amd64 table |
| `mkdir('.../<mnt>'): Permission denied` then `mount(...) failed` | (pre-fix) mounting the workdir on a path missing from the read-only rootfs | fixed: the workdir binds onto `/mnt`, which already exists in the base image |
| `execve('python3') ... No such file or directory` | (pre-fix) a bare argv name — nsjail `execve`s without a PATH search | fixed: the runtime passes the interpreter's absolute path |

Prove userns works on the host outside Docker (should print `ok`):

```sh
unshare --user --net --map-root-user /bin/true && echo ok
```

If that fails, the host itself forbids unprivileged userns — enable it (step 1)
or use a host/VM that allows it; nsjail cannot run rootless without it.

### Troubleshooting: the runner boots but you can't reach it

If `/healthz` from the host resets/refuses (`curl` exit 56/7) while it works from
*inside* the container (`docker exec runner python3 -c "import urllib.request;
print(urllib.request.urlopen('http://127.0.0.1:8090/healthz').read())"`), the
server is healthy but the host↔container bridge is firewalled on this box (common
on VPSes). The server binds all interfaces (`:8090`), so the fix is to skip the
bridge with **host networking** — drop `-p` and add `--network host`:

```sh
docker run -d --name dalivim-runner \
  -e RUNNER_SERVICE_TOKEN=<secret> -e RUNNER_SANDBOX=require \
  --network host \
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
  --pids-limit=512 --ulimit nproc=512 --cpus=1 --memory=1g \
  dalivim-runner
```

Per-run network isolation is unaffected — nsjail gives each run its own empty
netns regardless of the container's network mode. With host networking the runner
listens on `0.0.0.0:8090`, so put it behind a reverse proxy (TLS) and firewall
`8090` off the public interface (allow only `443`).

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
