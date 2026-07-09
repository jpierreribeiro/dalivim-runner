# G8 — Ops & containment maintenance

Grounding the "fleet resilience" and "security maintenance" directions against the
code showed most of that work is **already done** — graceful drain exists, the
escape corpus already runs in CI on every build and weekly. This spec deliberately
captures **only the genuine residuals**: three small, high-hygiene items, each with
its "don't rebuild this" note so nobody re-implements what's already shipped.

---

## Already done — do NOT rebuild
- **Graceful shutdown / drain**: `cmd/runner/main.go:86-87`
  (`signal.NotifyContext`, `SIGTERM`/`Interrupt`) + `internal/transport/httpapi/server.go:95-118`
  (`http.Shutdown` with a 15 s grace at `:112`). In-flight runs drain on deploy.
- **Concurrency backpressure**: `internal/transport/httpapi/middleware.go:42-58`
  (semaphore, default 8 — `internal/config/config.go:65` — `503 + Retry-After: 1`).
- **Escape corpus in CI**: `scripts/smoke-escape.sh` (8 adversarial cases + a
  host-survival check) wired into `.github/workflows/ci.yml:382`, plus a **weekly
  cron** (`ci.yml:21`), a **Trivy** CVE gate (`ci.yml:428`), and a **`SIGSYS`
  under enforce** assertion (`ci.yml:340-380`).
- **Auth**: constant-time token compare (`middleware.go:19-30`), token loaded and
  required outside dev (`config.go:57`, `:82-83`).

There is **no containment gap** to chase. The items below are hygiene.

---

## G8.1 — Language-parametrize the escape corpus

### Motivation
`smoke-escape.sh` submits `language:"python"` for **all** 8 cases
(`smoke-escape.sh:30`). Containment for Go/Java/C/C++/JS is proven only by
scattered individual CI steps, not a uniform corpus. The `ADDING-A-LANGUAGE.md`
rule — *"every new runtime must be re-proven contained; don't assume the sandbox
covers a runtime it's never seen"* — is not enforced **structurally**. As G2/G3
add languages, this is exactly the coverage that silently rots.

### Current state
One Python-only corpus + per-language egress/host-write asserted ad hoc across
`ci.yml`. Seccomp allowlist is `staticAllowSyscalls` (`internal/sandbox/nsjail.go:71-81`);
only **C/C++** run under the enforce allowlist (`compiled.go:107`, `:122`), while
Go (`compiled.go:157`), Java, and the interpreted languages run on the **denylist**
(`compiled.go:272-275` pins non-allowlist langs there).

### Proposed change
Table-drive the corpus over the language set: `{language → applicable cases}`.
- **Universal cases** (all languages): egress blocked, host-write denied,
  `/etc/shadow` denied, fork bomb capped, big alloc → `memory_exceeded`, spin →
  `timeout`.
- **Language-specific cases**: `SIGSYS`-on-`socket` is only meaningful for C/C++
  **under enforce**; Go/Java on the denylist assert egress/host containment via the
  denylist path. Encode which cases apply per language.
- Rewrite `smoke-escape.sh` to loop languages (each case expressed in that
  language), or add a Go table test that shells the corpus per language. The build
  **fails** if any language escapes any applicable case.

### Effort
S/M — mostly test harness. **Ride it alongside every new language** (G2/G3) so
adding a language and proving it contained are one change.

---

## G8.2 — Token rotation (accept a set, not one static string)

### Motivation
`RUNNER_SERVICE_TOKEN` is a single static value (`config.go:57`); rotating it is a
hard cutover with a window where either the old or the new token is rejected. No
overlap = risky rotation, so in practice it never happens.

### Current state
One token, compared with `subtle.ConstantTimeCompare` against the `X-Runner-Token`
header (`middleware.go:19-30`). A separate single `MetricsToken` (`config.go:66`).

### Proposed change
Accept a **set** of valid tokens (`RUNNER_SERVICE_TOKENS`, comma/space-separated);
a request authorizes if it matches **any**, each compared in constant time. Keep
`RUNNER_SERVICE_TOKEN` as the single-value alias. Same treatment for the metrics
token. Rotation becomes zero-downtime: add new → deploy → point backend at new →
remove old.

### Security considerations
- Constant-time compare against **each** candidate; do not early-exit on the first
  byte of the first token. Cap the set size.
- Still **fail-closed** outside dev (an empty set must not authorize).
- If you add key-ids, log *which* key authorized (never the token value) — useful
  for spotting a stale credential still in use before you retire it.

### Effort
S — a small auth-middleware + config-parse change.

---

## G8.3 — Align shutdown grace with the max run wall-time

### Motivation
The drain grace is a fixed 15 s (`server.go:112`). If a single run's wall timeout
can exceed 15 s (`RUNNER_MAX_TIMEOUT_MS`), a deploy `SIGTERM` kills that run
mid-flight even though drain is "graceful" — the student sees a spurious failure
during every deploy that happens to overlap a long run.

### Current state
Fixed 15 s grace vs a configurable per-run wall-time ceiling; the two are not
reconciled.

### Proposed change
Make the shutdown grace **≥ the max allowed run wall-time** — derive it from the
timeout ceiling, or make it configurable with a documented invariant
`grace ≥ max_run_timeout + slack`. Verify the current 15 s against the configured
max timeout and reconcile. (Alternative: on `SIGTERM`, stop accepting new runs but
let each in-flight run finish up to its own deadline.)

### Effort
XS — a config-relationship fix plus a documented invariant.

---

## Contract / config impact
- **G8.1**: none (CI only).
- **G8.2**: new `RUNNER_SERVICE_TOKENS` (plural) env; `RUNNER_SERVICE_TOKEN` stays
  as a single-value alias. Additive; no behaviour change for existing deploys.
- **G8.3**: new/derived shutdown-grace config; invariant `grace ≥ max run
  wall-time`.

## Testing
- **G8.1**: the parametrized corpus runs every language and fails the build if any
  escapes any applicable case.
- **G8.2**: two valid tokens both authorize; a removed token → `401`; constant-time
  preserved; empty set outside dev fails closed.
- **G8.3**: a run near the max timeout survives a `SIGTERM` issued mid-run (drains
  to completion); a run that would exceed the grace is documented/handled.

## Effort
S/M total across the three. None is blocking — do them opportunistically, but
**G8.1 should ride alongside every new language** (G2/G3).

## Phase G8 acceptance
- The escape corpus asserts containment for **all** languages, gated in CI.
- Token rotation is possible with a zero-downtime overlap window.
- Shutdown never kills an in-flight run that is still within its own deadline.
