# Odin support (B.1) — status and the on-target steps left

Odin (<https://odin-lang.org>) is added as a **Shape B (static-compiled)** language
per [`ADDING-A-LANGUAGE.md`](./ADDING-A-LANGUAGE.md).

## What landed (in code, tested)

- `odinSpec` + `NewOdin` in [`compiled.go`](../../internal/executor/compiled.go):
  `odin build {src} -file -out:{out}`, run the native artifact, full-rootfs
  denylist jail, `parseOdinVersion`.
- Registered in [`cmd/runner/main.go`](../../cmd/runner/main.go).
- Spec-level tests in [`odin_test.go`](../../internal/executor/odin_test.go)
  (templates, `-file`, run-is-artifact, posture, version parse). These pass in
  the unit suite **without** the toolchain, exactly like the other compiled specs
  — the real compile+run is a CI Docker-smoke concern.

Registering Odin is safe before the image has the toolchain: `resolveBin`
degrades to the bare name and `detectVersion` returns "" (empty version) without
blocking boot. A run request for `odin` simply fails to compile until the image
ships the toolchain.

## What is NOT done here (needs a build+run environment)

The dev/unit image has no Odin toolchain and the release download is
proxy-blocked in this environment, so the two things the recipe defers to the
target are still open:

### 1. Dockerfile — install the toolchain

Odin ships prebuilt release tarballs that bundle their own LLVM. Add a stanza to
the **runtime** stage of [`Dockerfile`](../../Dockerfile) (pin the version and,
ideally, a sha256), roughly:

```dockerfile
# ---- Odin toolchain (B.1) for the `odin` compile jail ----
# Prebuilt release bundles LLVM; extract to /opt/odin and expose `odin` on PATH.
ARG ODIN_VERSION=dev-2024-05
RUN set -eux; \
    curl -fsSL -o /tmp/odin.zip \
      "https://github.com/odin-lang/Odin/releases/download/${ODIN_VERSION}/odin-linux-amd64-${ODIN_VERSION}.zip"; \
    mkdir -p /opt/odin && unzip -q /tmp/odin.zip -d /opt/odin && rm /tmp/odin.zip; \
    ln -s /opt/odin/odin /usr/local/bin/odin; \
    chmod -R a+rX /opt/odin
ENV PATH="/opt/odin/bin:${PATH}"
```

Verify the actual release asset name/layout for the pinned version (the archive's
top-level directory and whether `odin` sits at the root or under a subdir) before
trusting the paths above. If the CI build network also blocks the GitHub release,
vendor the tarball or build Odin from source (`make` against a pinned LLVM-dev).

### 2. Confirm the conservative posture choices

`odinSpec` documents three choices made without a local compile; validate each on
the target and adjust the spec:

| Choice | Set to | Verify | If wrong |
|---|---|---|---|
| `runFullRootfs` | `true` (dynamic libc) | `ldd` the artifact — dynamic? | If static-linkable (`-extra-linker-flags:"-static"`), flip to `false` for the minimal rootfs |
| `capAddressSpace` | `true` (normal allocator) | hello-world under `RLIMIT_AS` (256 MB) | If it aborts at startup, set `false` (cgroup-only, like Go/JVM) |
| seccomp | denylist | run representative programs with `RUNNER_STATIC_SECCOMP=complain`, read `dmesg` | widen the allowlist only if measured, else keep denylist |

### 3. On-target run tests (CI smoke)

Add, alongside the other compiled smoke cases: a `success` hello-world
(`package main` + `main :: proc()`), a `memory_exceeded` alloc bomb, a `timeout`
infinite loop, and egress-contained. The unit `odin_test.go` stays spec-level.

## Entrypoint convention

Single-file Odin needs `package main` and a `main :: proc()` in `main.odin`; the
`-file` flag builds that one file. Multi-file (Odin's package = a directory) maps
onto the runner's `files[]` model as a follow-up (mirror the compiled multi-file
build), not part of B.1.
