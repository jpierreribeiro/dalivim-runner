# Security hardening roadmap — dalivim-runner

Kept **separate** from the feature roadmap (`../README.md`) on purpose: these items
harden the runner's existing guarantees rather than adding capability. They touch the
security-critical core (request validation, the seccomp policy, the supply chain), so
they get their own space, their own review bar, and their own on-target proof.

The runner's live containment posture (already in prod) is catalogued in
`../../RUNNER_AUDIT_AND_CONTRACT.md` and `../../RUNNER_HARDENING_CHECKLIST.md`. These
`S*` specs are the **next** hardening degree, not the baseline.

## Roadmap at a glance

| # | Theme | Value | Size | Breaks contract? | Risk if skipped |
|---|---|---|---|---|---|
| **[S1](S1-fuzzing-validators.md)** ✅ | Fuzz the request validators (path grammar, JSON decode) | 🔴 high | **S** | no | a parser edge case reaches materialization |
| **[S2](S2-seccomp-allowlist-interpreters.md)** | Tighten interpreters toward a seccomp allowlist (nsjail/kafel bump) | 🟠 medium | M/L | no | wider kernel attack surface than necessary |
| **[S3](S3-supply-chain-sbom-signing.md)** | SBOM + image signing / provenance (cosign) | 🟢 medium | S/M | no | no attestable provenance of the shipped image |

## How each spec is written

Same 7-part shape as the feature specs (`../README.md:61-72`): Motivation · Current
state (`file:line`) · Proposed change · Contract/config impact · Security
considerations (here: *what could go wrong doing the hardening itself*) · Testing ·
Effort. Every claim is anchored to code.

## Ordering

**S1 first** — highest ROI, lowest effort, zero deploy risk (it's a test suite over
pure functions). ✅ **done (2026-07-11)** — six invariant fuzz targets + a bounded CI
lane. **S3 next** — mechanical, CI-only, no runtime change. **S2 last** —
it changes the live seccomp policy and **must** be validated on-target (a wrong
allowlist fails the nsjail boot probe closed under `RUNNER_SANDBOX=require`), so it
carries real rollout risk and should ride behind the `complain`-mode tooling that
already exists.

## The one absolute

Every item preserves **"the runner stays dumb and contained."** None of these adds a
capability, relaxes a limit, or lets the request influence the jail. Hardening only
ever *removes* degrees of freedom.
