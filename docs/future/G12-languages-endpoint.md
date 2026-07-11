# G12 — `GET /languages` capability-discovery endpoint

> **Status — ✅ implemented (2026-07-11).** `GET /languages` is live: unauthenticated
> and secret-free (same posture as `/readyz`), it returns the boot-resolved catalog
> (`Service.Catalog()` → id / version / kind / multifile / batch) plus the effective
> limits. A test pins that the catalog id set equals the run-dispatch registry (no
> drift) and that no token leaks into the payload. Planning notes kept below.

A small observability/DX addition ([G4](G4-observability.md) family): expose the
runner's supported languages, detected versions, and effective limits so the
frontend/backend can render and validate **dynamically** instead of hardcoding a
list that drifts every time a language is added. The data already exists in process;
it just isn't reachable over HTTP.

---

## Motivation

Every time a language lands (Go, Java, Lua, and next SQL/test-mode), any consumer
that shows "supported languages" or validates the `language` field must be updated
in lockstep, out-of-band. That is avoidable coupling: the runner **is** the source
of truth for what it can run and at which versions. A discovery endpoint lets the
frontend populate a language picker, and the backend reject an unsupported language
early, from the runner itself — no second list to keep in sync.

It also makes the fleet self-describing: `/languages` on an instance reports exactly
what *that build* supports (versions included), which is useful during a rolling
deploy that adds a language.

## Current state

- The registry knows its languages: `Service.Languages()` returns the registered ids
  (`internal/executor/executor.go:111-117`) — **but it is only used for diagnostics
  and has no HTTP route.**
- Each runtime already detects and holds its version: `Runtime.Version()`
  (`executor.go:83-91`), populated once at construction via `detectVersion`
  (`internal/executor/interpreted.go:282-291`, compiled at `compiled.go:328`).
- Limits are resolved once at boot into `executor.Limits`
  (`cmd/runner/main.go:56-75`) and the transport caps into `httpapi.Config`
  (`internal/transport/httpapi/server.go:24-62`) — known, not exposed.
- Routes today: `POST /run`, `POST /run/python`, `GET /healthz`, `GET /readyz`,
  `GET /metrics` (`internal/transport/httpapi/server.go:108-113`). No `/languages`.

## Proposed change

Add an **unauthenticated** `GET /languages` (it reveals only capability, no secrets —
same posture as `/readyz`, `handlers.go:301-325`) returning the registered languages
with version and the effective limits that apply to a run:

```jsonc
{
  "languages": [
    { "id": "python", "version": "3.12.3", "kind": "interpreted",
      "multifile": true, "batch": true },
    { "id": "go",     "version": "1.26",   "kind": "compiled",
      "multifile": true, "batch": true },
    { "id": "sql",    "version": "3.45.1", "kind": "query",
      "multifile": true, "batch": false }
    // ...
  ],
  "limits": {
    "default_timeout_ms": 3000, "max_timeout_ms": 10000,
    "default_memory_mb": 128,   "max_memory_mb": 512,
    "max_source_bytes": 200000, "max_files": 50, "max_batch": 100
    // the same ceilings clamped in executor.go:193-205 + the transport caps
  }
}
```

Implementation locus:

- **Executor:** extend the registry surface — a `Service.Catalog()` returning, per
  language, `{id, version, kind, multifile, batch}`. `kind` is derivable
  (`interpretedRuntime` vs `compiledRuntime`), `batch` from the `batchRunner`
  assertion (`executor.go:73-78`), `multifile` from `filePolicies` membership
  (`filepolicy.go:112`). Version is `Runtime.Version()`.
- **Transport:** a `h.languages` handler serializing `Catalog()` + the boot-resolved
  limits (thread the relevant `executor.Limits`/caps into the `handler`, same way
  `/readyz` got the sandbox posture, `handlers.go:28-33`). Register
  `mux.HandleFunc("GET /languages", h.languages)` (`server.go:111-113`). Method
  routing makes a wrong method a 405 for free (`server.go:68-72`).

Keep it **static** — the catalog is resolved at boot and read-only thereafter (like
the readiness posture), so the handler allocates a small response from immutable
state; no locking, no per-request work.

## Contract / config impact

- **Additive, read-only.** New `GET /languages`; no change to `/run` or any existing
  response. No new env.
- The response is **capability, not secrets** → unauthenticated, cacheable. Document
  it as such (a consumer may cache it for the life of a deploy).
- Optional: include the deprecated `python` alias note so a consumer knows
  `POST /run/python` maps to `language:"python"`.

## Security considerations

- **No secrets in the payload** — languages, versions, and public limits only; never
  tokens, paths, or backend posture beyond what `/readyz` already exposes. Safe to
  leave unauthenticated (consistent with `/healthz`/`/readyz`).
- **Version disclosure is a mild fingerprinting aid** (a consumer learns the exact
  interpreter versions). This is acceptable — the versions are already inferable from
  `runtime_version` on any `/run` response, and the value to legitimate callers
  outweighs it. If an operator objects, gate it behind the metrics-token set (reuse
  the `/metrics` gating pattern, `server.go:103-106`, `113`) — but default open.
- Static, allocation-bounded response → no DoS surface.

## Testing

- **Shape:** `GET /languages` → 200, lists every registered language with a non-empty
  `version` (where the interpreter is present) and correct `kind`/`multifile`/`batch`
  flags.
- **In sync with the registry:** a table test asserting the endpoint's id set equals
  `Service.Languages()` — so adding a language can't silently omit it from discovery.
- **Method routing:** `POST /languages` → 405.
- **No secrets:** a test asserting the response contains none of the token/env values.

## Effort

**XS/S.** A `Catalog()` builder over the existing registry + a static handler + a
route. The only design choice is whether to gate it (default: open).

## Phase G12 acceptance

- `GET /languages` returns the live catalog (id, version, kind, multifile, batch) and
  the effective limits, from boot-resolved immutable state.
- The catalog id set provably equals the run dispatch registry (no drift).
- The endpoint is unauthenticated, static, and secret-free.
