# S3 — Supply-chain: SBOM + image signing / provenance (cosign)

> **Status — ✅ implemented (2026-07-11), CI-only per the roadmap framing.** An
> isolated `publish` job (`.github/workflows/ci.yml`) runs on push to `main` /
> `workflow_dispatch` — never on PR or the weekly schedule, so signing-infra
> availability never gates functional CI, and gated behind `test`/`fuzz`/`runner-smoke`
> so a failed build is never signed. It pushes the image to
> `ghcr.io/jpierreribeiro/dalivim-runner`, then **keyless** (Fulcio OIDC, no stored
> key): `cosign sign` the digest, generate an SPDX-json **SBOM** with trivy and attach
> it via `cosign attest` (also uploaded as a build artifact), and attach a **SLSA
> build-provenance** predicate via `cosign attest --type slsaprovenance` binding the
> digest to the workflow + commit. All three (signature, SBOM, provenance) are written
> to GHCR by cosign — **not** GitHub's `actions/attest-build-provenance`, whose
> attestation API returns "Feature not available for user-owned private repositories";
> the cosign path is registry-stored, verifiable the same way, and unaffected. OIDC +
> `packages: write` are scoped to the job only; the workflow keeps top-level
> `contents: read` (no `attestations: write` needed).
>
> **Deploy scope was kept CI-only** (matching the roadmap's "S3 = CI-only, no runtime
> change"): the default deploy still **builds from pinned source on the box** — for a
> "dumb, contained executor," inspectable source is not obviously weaker than trusting
> a registry, and pull-and-verify adds new failure modes (registry outage, cosign on
> the box). The spec's fail-closed verify ships as an **opt-in** `deploy.sh
> verify-image` (`cosign verify` with the signer identity **pinned** to this repo's
> workflow), documented in `docs/DEPLOY.md §8d`, so flipping to a full pull-and-verify
> deploy later is a config change, not a rewrite. **Note:** the job's first LIVE run is
> post-merge on `main` (OIDC + GHCR push don't run on PR branches). Planning notes kept
> below.

The image is already **reproducibly pinned** (bases by digest, every apt toolchain by
exact version) and **CVE-scanned** (trivy, fixable HIGH/CRITICAL gate). The next
maturity degree is **attestable provenance**: produce a Software Bill of Materials,
**sign** the image, and attach a build-provenance attestation — so a deploy can
*verify* it is running the exact artifact CI built, and an auditor can enumerate every
component from a signed document.

---

## Motivation

Pinning + scanning proves the image is *built from known-good inputs* and *has no
fixable known CVEs at build time*. It does **not** prove, at deploy or audit time,
that the running image **is** the one CI produced (no tamper in the registry / no
substitution) or provide a machine-readable component inventory for continuous CVE
watch after the fact. Signing + SBOM close that: **provenance** ("this digest was
built by this workflow from this commit") and **inventory** ("here is every package,
signed"). For a service whose whole job is running untrusted code, the integrity of
its own image is load-bearing.

## Current state

- **Reproducible pins**: bases by `@sha256` digest and apt toolchains by exact
  version, with a documented refresh procedure (`Dockerfile:1-19,23,38,53`).
- **CVE gate**: a dedicated `security-scan` job runs trivy against the shipped image,
  `--severity HIGH,CRITICAL --ignore-unfixed --exit-code 1`
  (`.github/workflows/ci.yml:733-777`), on PRs and weekly (`ci.yml:14-21`).
- **No SBOM** is generated or published; **no image signature**; **no build
  provenance attestation**. The image is built as `runner:smoke`/`runner:scan` in CI
  and (per `deploy/`) built on the target — there is no signed, attested artifact.
- `permissions: contents: read` at the workflow top (`ci.yml:23-24`) — signing needs
  `id-token: write` (OIDC) + `packages/attestations: write`, scoped to the signing
  job only.

## Proposed change

Add a **release/publish** job (distinct from the functional/scan jobs, so signing
concerns stay isolated — the same separation rationale as `security-scan`,
`ci.yml:727-732`) that, for a pushed image digest:

1. **Generate an SBOM** in a standard format (SPDX or CycloneDX) with trivy (already
   installed, `ci.yml:742-761`) or syft: `trivy image --format spdx-json` /
   `syft <img> -o cyclonedx-json`. Publish it as a build artifact and (below) as a
   signed attestation.
2. **Sign the image with cosign, keyless** (Sigstore/Fulcio OIDC — no long-lived
   key to store): `cosign sign <registry>/<repo>@<digest>` using the job's OIDC
   token. Keyless keeps the "zero secrets to manage" posture the runner already
   values.
3. **Attach attestations** to the digest: the SBOM (`cosign attest --type spdxjson
   --predicate sbom.json`) and **build provenance** (SLSA — via `cosign attest
   --type slsaprovenance` or the GitHub `actions/attest-build-provenance` action),
   binding the image digest to the workflow, commit SHA, and runner identity.
4. **Verify on deploy**: extend `deploy/deploy.sh` (the on-target proof already runs a
   `verify` step, referenced across the CI cgroup job, `ci.yml:580,715`) to
   `cosign verify` the pulled digest against the expected workflow identity **before**
   starting the container — fail closed if the signature/identity doesn't match, the
   same fail-closed discipline as `RUNNER_SANDBOX=require`.

Scope OIDC narrowly: add `id-token: write` and `attestations: write` **only** to the
new job (`ci.yml` job-level `permissions`), leaving the top-level `contents: read`
(`ci.yml:23-24`) intact for everything else.

## Contract / config impact

- **None on the runtime.** No code, no wire, no env in the service. Entirely CI +
  deploy tooling.
- Deploy gains a **verification gate** (`cosign verify` before run) — a new operator
  requirement, documented in `docs/DEPLOY.md` alongside the existing posture dials.
- The registry gains signed artifacts + attestations attached to each published
  digest.

## Security considerations (of doing the hardening)

- **Keyless signing avoids a stored key** — no new long-lived secret to leak; identity
  is the ephemeral OIDC token bound to the workflow. This preserves the runner's
  "minimal secret footprint" stance.
- **The verify step must fail closed** — a missing/invalid signature or an unexpected
  signer identity must **block the deploy**, not warn. A verify that soft-fails is
  security theater; wire it like the boot probe (`sandbox_linux.go:60-62`).
- **Pin the trusted signer identity** (the workflow's OIDC subject) in the deploy
  verify, so a signature from *any* Fulcio identity isn't accepted — only this repo's
  release workflow. Otherwise provenance proves "signed by someone", not "built by
  us".
- **Don't gate functional CI on signing.** Keep it a publish-time job so a Sigstore/
  Fulcio outage can't red the PR lane (mirrors keeping CVE scan separate,
  `ci.yml:727-732`); the deploy-time verify is where it's load-bearing.

## Testing

- **SBOM present & valid**: the publish job emits a parseable SPDX/CycloneDX document
  listing the pinned toolchains (python/node/gcc/go/openjdk/nsjail libs) — spot-check
  a known package+version appears.
- **Signature round-trips**: `cosign verify` against the expected workflow identity
  **passes** for a freshly published digest.
- **Tamper/negative**: `cosign verify` **fails** for an unsigned or wrong-identity
  digest, and the deploy verify **refuses to start** the container on that failure
  (the fail-closed proof).
- **Attestation binds provenance**: the SLSA predicate names the correct commit SHA
  and workflow — verified programmatically in the job.

## Effort

**S/M.** Mostly CI wiring (SBOM gen + cosign keyless + attest) and a deploy verify
step. trivy is already present for SBOM; the new surface is the OIDC permissions and
the fail-closed verify on the target.

## Phase S3 acceptance

- Every published image digest has a signed SBOM (SPDX/CycloneDX) and a SLSA
  build-provenance attestation, produced by an isolated publish job with narrowly
  scoped OIDC permissions.
- `deploy` verifies the signature **and** the signer identity against this repo's
  release workflow before starting the container, failing closed on any mismatch.
- Functional/CVE CI lanes are unaffected by signing-infra availability.
