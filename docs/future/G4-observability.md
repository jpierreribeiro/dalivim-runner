# G4 — Observability & operability

Make the runner legible in production: know its load, latency, and failure mix;
trace one run from the backend to the jail; prove containment holds on every
build. No contract change (metrics/logs are additive); the escape corpus is a CI
gate.

Items: G4.1 Prometheus metrics, G4.2 per-run structured logs + request IDs,
G4.3 `/readyz`, G4.4 escape corpus in CI.

---

## G4.1 — Prometheus `/metrics`

### Motivation
Today the only signal is boot logs. Under real load you can't answer: how many
runs/sec, p50/p99 latency, how often we 503 (overload), the status mix
(timeout/oom/compile_error rate), per-language volume. All of that is operational
oxygen for a judge behind a class of students hitting "submit" at once.

### Current state
No metrics of any kind; only `GET /healthz` (`server.go:46`). Concurrency
limiter's 503s (`middleware.go:49-52`) are invisible.

### Proposed change
Add `GET /metrics` (Prometheus text format). Suggested series:
- `runner_runs_total{language,status}` — counter.
- `runner_run_duration_seconds{language}` — histogram (run wall time).
- `runner_compile_duration_seconds{language}` — histogram.
- `runner_inflight` — gauge (current concurrency).
- `runner_overload_total` — counter (503 load-sheds).
- `runner_oom_total{language}`, `runner_timeout_total{language}` — counters.
- `runner_output_truncated_total` / `runner_output_limit_exceeded_total`
  (pairs with G1.4).

Auth: `/metrics` should **not** be public (it leaks usage). Either gate it behind
the `X-Runner-Token` (like `/run`) or a separate `RUNNER_METRICS_TOKEN`, or bind
it to a non-public interface. Given the runner is reached over public HTTPS
(Caddy), gate it — do **not** expose `/metrics` unauthenticated.

### Contract / config impact
New endpoint; optional `RUNNER_METRICS_TOKEN`. No `/run` change.

### Security considerations
Metrics reveal submission volume/patterns → treat as sensitive; require auth.
Label cardinality: `language` and `status` are bounded sets — safe. **Never** put
student code, tokens, or free-form strings in labels.

### Effort
M (add a metrics registry + instrument the run paths + gated endpoint).

---

## G4.2 — Per-run structured logs + request IDs

### Motivation
A completed `/run` currently logs **nothing** — you can't reconstruct what ran,
how it ended, or correlate a backend error with a runner run. When a student
reports "my submission failed", there's no trail.

### Current state
slog JSON boot logs only (`main.go:26`); no per-request logging, no request/trace
IDs (§7 of the capability map).

### Proposed change
- On every run, emit one structured log line at completion:
  `{request_id, language, status, duration_ms, compile_ms, memory_kb, exit_code,
  signal, truncated}`. **Never log source_code, stdin, or stdout** (PII / size);
  log sizes and hashes if useful, not contents.
- Accept an inbound `X-Request-ID` header from the backend; generate one if
  absent; echo it in the response header and include it in the log line. This
  stitches backend logs ↔ runner logs for a single submission.
- Optional: OpenTelemetry span per run if the backend already traces; otherwise
  the request ID is enough.

### Contract / config impact
New `X-Request-ID` request/response header (optional). No body change.

### Security considerations
The logging policy is the point: **outcomes and telemetry, never payloads.** A
run's source and I/O are student data — keep them out of logs entirely.

### Effort
S–M.

---

## G4.3 — `/readyz` (distinct from `/healthz`)

### Motivation
`/healthz` answers "is the process alive". It does **not** answer "can this
instance safely run untrusted code" — which is the real readiness question. On a
host where the nsjail probe failed under `RUNNER_SANDBOX=auto`, `/healthz` is
still `ok` while the runner is silently degraded to the weak netns fallback. An
orchestrator/load balancer needs a probe that fails when containment isn't
engaged.

### Current state
Only `/healthz` (liveness). The sandbox probe result (nsjail vs netns vs off) is
known at boot but not exposed (`sandbox_linux.go:43-69`).

### Proposed change
Add `GET /readyz` that returns `200` only when the resolved posture meets a
threshold, and `503` otherwise. Threshold configurable: e.g. ready iff nsjail is
active (and, if `RUNNER_CGROUP=require`, the cgroup engaged). Body reports the
resolved backend + seccomp + cgroup mode for quick diagnosis. This lets you run
`RUNNER_SANDBOX=auto` but still refuse traffic when it degraded — a middle ground
between silent degradation and `require`'s hard boot-fail.

### Contract / config impact
New endpoint; optional `RUNNER_READY_REQUIRES` (e.g. `nsjail,cgroup`). No `/run`
change.

### Security considerations
Makes silent degradation *visible* — a safety improvement. `/readyz` can be
public (it only reports posture booleans, no secrets) or gated; either is fine.

### Effort
S.

---

## G4.4 — Escape corpus in CI (containment regression net)

### Motivation
The hardening is only as good as its regression coverage. A refactor could
silently loosen the sandbox (a dropped seccomp entry, a mount that becomes
writable) and no test would catch it. A **corpus of programs that MUST fail to
escape** turns containment into an assertion checked on every build. (This is the
security-hardening half of the original F-E intent.)

### Current state
CI proves the *happy path* (ordinary C/C++ runs under enforce). There is no suite
of adversarial programs asserting containment holds.

### Proposed change
A CI job (on a runner with userns — the GitHub runner supports it, or a
self-hosted one) that runs a corpus and asserts each is **contained**, not
successful:
- **Egress**: TCP/UDP connect, DNS → must be `runtime_error`/blocked, never reach
  the net.
- **Filesystem**: read `/etc/shadow`, write outside `/tmp`, create in `/` → denied.
- **Privilege**: `setuid`, `ptrace` a sibling, load a kernel module, `mount` →
  killed (seccomp) / EPERM.
- **Fork bomb**: `while(1) fork()` → capped by nproc/pids, run ends, host
  unaffected.
- **Namespace escape**: `unshare`, `setns`, `/proc` mount attempts → denied.
- **Seccomp allowlist (C/C++ under enforce)**: a program calling a *denied*
  syscall (e.g. `socket`) → killed with `SIGSYS`, status reflects it.
Each corpus entry declares its expected contained-outcome; the job fails if any
escapes. Run under both nsjail postures if feasible.

### Contract / config impact
None — CI only.

### Security considerations
This *is* the security net. Keep the corpus versioned next to the sandbox code so
a change that weakens containment fails CI in the same PR. Mark the corpus clearly
as intentional-attack fixtures (so scanners/readers don't mistake them for
malware in the product).

### Effort
M — needs a userns-capable CI runner and a well-chosen corpus. High leverage.

---

## Phase G4 acceptance
- `/metrics` exposes run counts/latency/overload, gated by auth.
- Every run emits one payload-free structured log line with a request ID that
  matches the backend's.
- `/readyz` returns 503 when containment isn't engaged.
- The escape corpus runs in CI and fails the build on any containment regression.
