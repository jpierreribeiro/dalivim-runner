# Adding a language to dalivim-runner

A reusable procedure. Adding a language is a **code change** (the registry is
closed by design — `languages.go:14-17`, `compiled.go:21-22`), but a *mechanical*
one once you know which of the three shapes the language is.

## Step 0 — classify the language into one of three shapes

| Shape | Definition | Examples | Run process | Seccomp | Memory bound |
|---|---|---|---|---|---|
| **A. Interpreted** | interpreter reads source directly | Python, JS, Ruby | the interpreter | denylist | RLIMIT_AS *or* a heap flag |
| **B. Static-compiled** | build a **static** binary, run it | C, C++, Go, Rust | the artifact | denylist **or** tight allowlist | RLIMIT_AS + cgroup (**Go: cgroup only** — see below) |
| **C. VM-compiled** | compile to bytecode, run on a VM | Java, C#, Kotlin | the VM (`java`, `dotnet`) | **denylist only** | VM heap flag + cgroup |

The shape decides everything else. If unsure: does a single native executable come
out of compilation? → B. Does a VM run bytecode? → C. No compile step? → A.

---

## Shape A — interpreted (reference: `python`, `javascript`)

1. **Image**: install the interpreter in the runtime stage (`Dockerfile`). Pin the
   version.
2. **Spec** (`languageSpec`, `languages.go`):
   - `name`, `binNames` (e.g. `["ruby"]`), `runArgs` (argv template running the
     interpreter on the written source file, isolated flags where available —
     Python uses `-I`, Node `--disable-proto=throw`).
   - `capAddressSpace`: `true` if the runtime tolerates a hard `RLIMIT_AS`
     (CPython does); `false` if it reserves a large virtual space (V8/Node) — then
     bound the heap with a runtime flag instead (`nodeHeapArgs` pattern,
     `languages.go:113-115`) and rely on the cgroup for the authoritative OOM.
   - `memErrSubstr`: an OOM stderr marker for the no-cgroup fallback (Python:
     `"MemoryError"`); leave `""` if none and depend on the cgroup.
   - `sourceName`: the file the source is written to (`main.rb`).
   - `versionArgs` + parse function for `runtime_version`.
3. **Register** in `main.go` `NewService(...)`.
4. **Seccomp**: interpreters stay on the **denylist** (their syscall surface is
   too wide for the allowlist). Containment = ro-rootfs + empty netns +
   no_new_privs + rlimits.
5. **Test**: `success` hello-world + stdin echo; `memory_exceeded` (alloc bomb);
   `timeout` (infinite loop); egress contained.

**Effort**: S. Rust-the-script? no — Ruby/PHP/Perl fit here.

---

## Shape B — static-compiled (reference: `c`, `cpp`, `go`; next: `rust`)

1. **Image**: install the toolchain (compile stage or runtime stage). Ensure it
   can emit a **static** binary — this is the invariant that lets the run jail use
   the minimal rootfs and the tight seccomp allowlist. C/C++: `-static`. Go:
   `CGO_ENABLED=0`. Rust: `--target x86_64-unknown-linux-musl` (musl static) or
   `-C target-feature=+crt-static`.
2. **Spec** (`compiledLangSpec`, `compiled.go:23-29`):
   - `compile`: argv ending `-o {out} {src}` (or `{srcs}` after G3).
   - `link` (after G1.1): libraries appended **after** the source (C: `["-lm"]`).
   - `run`: `["{out}"]`.
   - compile-jail env if the toolchain needs a writable cache (`GOCACHE`,
     `CARGO_HOME` → point at `/tmp/...`; the compile jail is writable with
     `TMPDIR=/tmp`). Set `compileTmpfsMB` if that cache is large — nsjail's default
     `/tmp` is only a few MB; Go's `GOCACHE` needs ~40 MB+.
   - **cold-cache trap** (learned from Go): each run gets a **fresh** tmpfs, so the
     toolchain cache is cold every compile. A cold single-file Go build recompiles
     the stdlib and takes ~14 s on one CPU — over the compile budget. Fix: pre-warm
     the cache in the image (`Dockerfile` → `/opt/gocache`, world-readable) and have
     the compile **seed** its writable `/tmp` cache from it (goSpec runs a `/bin/sh`
     prelude `cp -r /opt/gocache /tmp/gocache && exec go build …`, with
     `compileArgv0Absolute` so nsjail execve's the shell). Warm build ≈ 0.3 s.
3. **Register** in `main.go`.
4. **Seccomp — tune the allowlist, or stay on the denylist**: a hello-world may
   pass the C/C++ allowlist (`staticAllowSyscalls`, `nsjail.go:71-81`) as-is, but
   richer runtimes need more. **Roll out `RUNNER_STATIC_SECCOMP=complain` first**
   (DEPLOY §8c), run representative programs, read `sudo dmesg | grep -i seccomp`
   for `syscall=NNN`, and widen the allowlist for what the runtime genuinely needs.
   **Go landed on the denylist**: its scheduler needs `clone`/`futex`/`epoll_*` —
   wider than the C set — so a language whose spec sets `staticAllowlistOK:false` is
   pinned to the denylist regardless of `RUNNER_STATIC_SECCOMP`, so enabling the
   allowlist for C/C++ never SIGSYS-kills it. Still strong: ro-rootfs + empty netns
   + no_new_privs + cgroup.
5. **Memory**: `capAddressSpace: true` (RLIMIT_AS + cgroup) works for a glibc
   static binary (C/C++). **Go is the exception — `capAddressSpace:false`**: the Go
   runtime reserves a huge virtual arena at startup and dies under *any* RLIMIT_AS
   ("failed to reserve page summary memory"), even at 512 MB — exactly like V8. So
   Go (and the compile jail, since `go build` is itself a Go program) skips
   RLIMIT_AS on **both** phases and is bounded by the cgroup `memory.max` only.
6. **Test**: `success`; a **denied-syscall program** (e.g. `socket`) → `SIGSYS`
   under enforce (allowlist languages only); `memory_exceeded` (cgroup);
   `timeout`; egress contained.

**Effort**: M (mostly image + cache pre-warm + seccomp choice). Go is the landed
reference — see [G2.1](G2-language-coverage.md).

---

## Shape C — VM-compiled (landed reference: `java`; next: C#, Kotlin)

This shape uses the **generalised compiled runtime**: the run step is a
spec-provided argv (`runBin` + `run` template with `{dir}`/`{mem}`), not "exec the
artifact", because you run the VM against the bytecode. Java is the landed
reference; these are the worked findings.

1. **Image**: install the JDK (`compile` = `javac`, `run` = the JVM). Debian's
   `openjdk-17-jdk-headless` from bookworm main is ABI-matched to the base — simpler
   and safer than a Temurin backport/stage; bump the version later. Heavy (~300 MB);
   a slim-JRE-for-run / JDK-for-compile split is a size follow-up.
2. **Spec** (`compiledLangSpec`):
   - `compile`: `javac -d {dir} {src}` (writes `Main.class` into the per-run dir).
   - `artifact`: `Main.class` (validated to exist after compile).
   - `run`: `java -XX:+UseSerialGC -XX:-UsePerfData -XX:ActiveProcessorCount=1
     -Xmx{mem}m -cp {dir} Main`; `runBin: ["java"]` (resolved to the absolute
     launcher for the run argv0). `-XX:-UsePerfData` avoids a `/tmp` hsperfdata
     write + `getpwuid` on a jail uid with no passwd entry.
   - `runFullRootfs: true` — the JVM is dynamically linked, so it keeps the full
     read-only rootfs (same posture as the interpreters), NOT the minimal static
     jail.
   - `capAddressSpace: false` — the JVM (and `javac`, itself a JVM) reserve a large
     virtual space and **die under any RLIMIT_AS** ("Could not reserve enough space
     for code cache"). Bound with `-Xmx` + the cgroup.
   - `memErrSubstr: "OutOfMemoryError"` for the no-cgroup fallback OOM classify.
   - **entrypoint convention**: public class `Main`, source written to `Main.java`
     (`javac` ties filename to the public class). Document it; the backend
     enforces/injects it.
   - **memory**: `-Xmx{mem}m` uses the request's `memory_mb`, but JVM non-heap
     overhead (metaspace, code cache, stacks) is ON TOP, so the backend should send
     a generous `memory_mb` for Java. A per-language floor is a follow-up (G5).
   - **timeout**: measured JVM cold start is well under a second here, so no floor
     was needed — but re-check on the target.
3. **Register** in `main.go`.
4. **Seccomp — denylist only.** VMs need `clone`, `openat`, JIT mappings — the
   static allowlist cannot apply. Verified (via `strace`) that the JVM trips NONE
   of the denylist's KILLs (`perf_event_open`, `ptrace`, `bpf`, module ops,
   `mount`) at startup — do the same check for a new VM before trusting it.
5. **Env**: set the run env explicitly and never inherit — in particular do NOT let
   `JAVA_TOOL_OPTIONS` leak in (it prints to stderr and injects flags). The runtime
   forces a non-nil (possibly empty) env so the child never inherits the runner's.
6. **Test**: `success` (println, stdin, collections); `memory_exceeded` (deploy-time
   via `-Xmx`+cgroup, like Go — not the `cgroup=auto` CI); `timeout`; **egress
   contained** (mandatory).

**Effort**: L (new runtime shape + heavy image). Java is the landed reference — see
[G2.2](G2-language-coverage.md).

---

## Universal checklist (every shape)

- [ ] Toolchain/interpreter in the image, version pinned.
- [ ] Spec added; registered in `main.go` `NewService`.
- [ ] `runtime_version` detection works.
- [ ] Memory strategy chosen (RLIMIT_AS vs heap-flag) and OOM classifies as
      `memory_exceeded` (prefer cgroup; add a stderr marker for the no-cgroup path
      if the runtime has a stable one).
- [ ] Seccomp posture decided (denylist, or allowlist tuned via complain-mode).
- [ ] On-target acceptance: `success`, `memory_exceeded`, `timeout`,
      **egress contained**, and (Shape B under enforce) a denied syscall → `SIGSYS`.
- [ ] `deploy/deploy.sh verify` extended (or a language-specific smoke) proves it.
- [ ] Multi-file behaviour defined (G3) — at minimum the single-file default name.
- [ ] Docs: add the language to DEPLOY/TESTING and update this guide's reference
      list.

## Anti-patterns
- **Dynamic linking a "static" language** — breaks the minimal-rootfs run jail and
  the allowlist. Keep Shape B truly static.
- **Opening the allowlist blindly** — if complain-mode shows a syscall you don't
  understand, investigate before allowing it; a newer nsjail/kafel may name it
  properly (the `rseq`/`statx` situation, `nsjail.go` comments) rather than
  needing a wider set.
- **Logging or labelling with source/stdin** — never (see G4).
- **Skipping the egress test** — every new runtime must be re-proven contained;
  don't assume the sandbox covers a runtime it's never seen.
