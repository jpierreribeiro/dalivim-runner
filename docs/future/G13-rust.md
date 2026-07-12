# G13 — Rust (static-compiled, Shape B)

Rust is the fourth compiled language and the cleanest new addition since Go: it
slots into **Shape B** (`ADDING-A-LANGUAGE.md:47`) — a static binary comes out of
compilation and the run jail executes only that artifact — reusing the two-jail
`compiledRuntime` (`internal/executor/compiled.go:264`) verbatim. It is the landed
static reference (`c`, `cpp`, `go`) with one deliberately-chosen toolchain wrinkle
(musl) and none of Go's cold-cache pain.

> **The line.** The runner compiles a single-file, std-only Rust program to a
> static musl binary and runs it in the minimal-rootfs jail. It never fetches a
> crate, never runs `cargo`, never sees the network — single-crate std is the whole
> surface, exactly like single-file Go (`compiled.go:145`).

---

## Motivation

Rust is a first-class teaching and interview language with no execution judge here.
It is a natural fit: a static binary is the artifact the F-D minimal-rootfs run jail
was built for, and the std library is self-contained (no runtime, no VM). It adds a
modern systems language for the cost of one toolchain in the image.

## Current state

- Compiled languages are a **closed registry** of `compiledLangSpec`
  (`compiled.go:23`) instances (`cSpec`/`cppSpec`/`goSpec`/`javaSpec`,
  `compiled.go:107-233`) registered in `main.go` `NewService`
  (`cmd/runner/main.go:80-84`). There is no `rust` entry.
- `compiledRuntime` (`compiled.go:264`) runs the two-jail compile→run flow: an
  untrusted compile jail (writable `/sandbox`, full rootfs, toolchain present) that
  emits an artifact the host validates (`compiled.go:544-553`), then a stricter run
  jail. `MinimalRootfs` + the static seccomp allowlist (`SeccompStaticEnforce`,
  `sandbox/sandbox.go:122`) are the F-D tightening a **static** artifact unlocks.
- The static seccomp allowlist is `staticAllowSyscalls` (`sandbox/nsjail.go:71-81`);
  `staticAllowlistOK:false` on a spec pins it to the denylist regardless of
  `RUNNER_STATIC_SECCOMP` (Go's posture, `compiled.go:179`).
- `filePolicies` (`internal/executor/filepolicy.go:112`) has no `rust` entry.
- The image ships gcc/g++/Go/JDK (`Dockerfile`); no Rust toolchain.

## Proposed change

### Spec (`compiledLangSpec`)

```go
var rustSpec = compiledLangSpec{
    name:       "rust",
    sourceFile: "main.rs",
    // Static by construction: the musl target sets crt-static, so the binary is
    // fully self-contained and the run jail keeps the minimal rootfs (D-4), like
    // C/C++/Go. -O optimises; --edition 2021 pins the language edition (determinism).
    // A single .rs with only std needs no crate resolution — no cargo, no network.
    compile:  []string{"rustc", "--edition", "2021", "-O",
                       "--target", "x86_64-unknown-linux-musl", "-o", "{out}", "{src}"},
    run:      []string{"{out}"},
    binNames: []string{"rustc"},
    // rustc writes incremental/temp artifacts under TMPDIR=/tmp; the compile jail is
    // writable with a size-capped /tmp. Unlike Go there is NO cold-stdlib rebuild:
    // std is shipped precompiled as rlibs with the musl target, so a single-file
    // build is ~0.5 s cold — no /opt cache pre-warm needed. Bump compileTmpfsMB only
    // if a real program's temp exceeds the default.
    runEnv:            determinismEnv(),
    // Rust uses the system allocator (musl malloc), not a Go/V8-style virtual arena,
    // so a hard RLIMIT_AS bounds it deterministically — capAddressSpace TRUE (like C).
    capAddressSpace:   true,
    // Start on the DENYLIST: Rust std touches futex/getrandom/sigaltstack/rt_sig* /
    // poll — wider than the C/C++ static allowlist names. Prove containment on the
    // denylist first, then tune an allowlist via RUNNER_STATIC_SECCOMP=complain
    // (ADDING-A-LANGUAGE.md:70-79). Denylist is already strong (ro-rootfs + empty
    // netns + no_new_privs + cgroup).
    staticAllowlistOK: false,
    versionArgs:       []string{"--version"}, // "rustc 1.83.0 (…)"
    parseVersion:      secondField,           // -> "1.83.0"
}
```

Register in `main.go` `NewService` (`cmd/runner/main.go:80`) via `NewRust(sb,
compiledConfig(cfg))`, mirroring `NewC`/`NewGo` (`compiled.go:297-342`).

### Toolchain & image (the one real decision)

- **musl-static + a pinned rustup toolchain** (chosen). A build stage installs
  rustup, pins an exact toolchain (`rustup toolchain install 1.83.0`), adds the
  `x86_64-unknown-linux-musl` target, and the final image copies the toolchain in —
  the same "toolchain copied from a build stage" pattern Go uses (`Dockerfile:88`).
  Debian's `rustc` (1.63) is too old and lacks the musl std out of the box, so
  rustup is the deliberate choice. Add `musl-tools` (`musl-gcc`) as the musl linker
  (or pin `-C linker=rust-lld`); verify the linker path on-target.
- Pin the toolchain version; a rebuild installs the byte-identical compiler.

### Classification

Reuses `compiledRuntime.execute`'s switch unchanged (`compiled.go:633-647`): a
compile failure → `compile_error` (the `rustc` diagnostics are the `CompileOutput`);
a panic/abort → `runtime_error` (non-zero exit / `SIGABRT` signal); `timeout`;
`memory_exceeded` from the cgroup OOM (or RLIMIT_AS). No new status.

## Contract / config impact

- One new language id `rust`; a `rust` `FilePolicy` (`.rs` only; forbid
  `Cargo.toml`/`Cargo.lock`/`build.rs` — offline, single-crate). Multi-file (G3) is a
  follow-up: Rust `mod` needs a fixed file layout; single-file default `main.rs`
  ships first.
- No new run/limit knobs — reuses the compiled compile-timeout + memory limits.
- Image: the pinned rustup toolchain + musl target + `musl-tools`.

## Security considerations

- **Truly static** (musl crt-static) — the invariant that keeps the run jail
  minimal-rootfs and (later) allowlist-eligible. Do **not** let it become
  dynamically linked (anti-pattern, `ADDING-A-LANGUAGE.md:164`).
- **Offline single-crate**: no `cargo`, no crate registry, no `build.rs` execution
  (forbidden by the file policy). The empty netns is the structural backstop; the
  policy makes it explicit.
- **Seccomp**: denylist first; every syscall the tuning adds must be understood
  (`ADDING-A-LANGUAGE.md:166`).
- **Determinism**: `std::collections::HashMap` randomises iteration order (SipHash
  seeded from `getrandom`) and — unlike Python's `PYTHONHASHSEED` (G11) — exposes
  **no env knob**. It is a documented non-guarantee, exactly like Go's map order
  (`docs/DETERMINISM.md`): "use `BTreeMap` or sort for stable output." Add Rust to
  that doc's per-language table.

## Testing

- **Success**: `fn main(){ println!("{}", 2+2) }` → `success` / `"4\n"`; stdin echo.
- **Compile error**: a type error → `compile_error`, `rustc` diagnostics in
  `compile_output`, nothing executed.
- **Panic**: `panic!("boom")` → `runtime_error` (non-zero / SIGABRT), not success.
- **memory_exceeded**: a `Vec` allocation bomb → cgroup OOM (and RLIMIT_AS).
- **timeout**: `loop {}` → `timeout`.
- **Isolation (on-target, mandatory)**: egress via `std::net::TcpStream::connect`
  fails closed (empty netns); a host write fails (ro rootfs). Add `rust` to the
  escape corpus `LANGS` (`scripts/smoke-escape.sh:64`) with per-case snippets.
- **Static proof**: `file`/`ldd` on the artifact shows "statically linked".

## Effort

**M.** Image (rustup toolchain + musl target) is the bulk; the spec is a
near-copy of `cSpec`/`goSpec`. No cache pre-warm (std is precompiled), no VM, no new
run shape — the lowest-risk of the three additions.

## Phase G13 acceptance

- A `rust` submission compiles a static musl binary and runs it in the minimal-rootfs
  jail; `success`/`compile_error`/`runtime_error`/`timeout`/`memory_exceeded` all
  classify correctly.
- Egress + host-write contained on-target (escape corpus extended); the artifact is
  provably static.
- `runtime_version` reports the rustc version; `docs/DETERMINISM.md` records the
  HashMap-order non-guarantee.
