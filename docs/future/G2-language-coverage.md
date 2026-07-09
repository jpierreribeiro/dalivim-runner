# G2 — Language coverage (Go, Java)

Add the two languages the platform needs, and codify *how* to add more. Additive
— no contract break. Read [ADDING-A-LANGUAGE](ADDING-A-LANGUAGE.md) alongside this;
that guide is the reusable procedure, this file is the two concrete instances.

The runner has **three runtime shapes**, and each new language is one of them:

| Shape | Examples | Sandbox seccomp | Memory bound |
|---|---|---|---|
| **Interpreted** | python, javascript | denylist | RLIMIT_AS (py) / heap flag (node) |
| **Static-compiled** | c, cpp, **Go** | denylist, or **allowlist** (tight) | RLIMIT_AS + cgroup |
| **VM-compiled** | **Java** (JVM), C# | denylist only | `-Xmx` + cgroup |

Go is the easy one (fits the existing static-compiled path). Java introduces the
third shape and needs a small runtime abstraction.

---

## G2.1 — Go (static-compiled)

### Motivation
Go is a natural fit: `go build` yields a static binary, so it slots into the same
two-jail flow as C/C++ and can even take the tight seccomp allowlist.

### Current state
Compiled languages are `compiledLangSpec` entries (`compiled.go:31-47`) with
`compile`/`run` slices; the two-jail flow (`compiled.go:118-256`) is
language-agnostic. Registered in `main.go:57-68`. No Go toolchain in the image.

### Proposed change
1. **Image**: add the Go toolchain to the runtime stage (or a build stage that
   copies `go`). Pin a version. Note Go's build cache/`GOCACHE` needs a writable
   dir — the compile jail is `Writable:true` with `TMPDIR=/tmp`; set
   `GOCACHE=/tmp/gocache`, `GOPATH=/tmp/gopath`, `GOFLAGS=-mod=mod`,
   `CGO_ENABLED=0` (pure-static, no libc link).
2. **Spec**:
   ```go
   // compile: ["go","build","-o","{out}","{src}"]   (src = main.go)
   // run:     ["{out}"]
   // env for compile jail: CGO_ENABLED=0 GOCACHE=/tmp/gocache GOPATH=/tmp/gopath
   ```
   `go build` wants a `.go` file (or a module). Single-file: write `main.go`,
   `go build` it directly. (Module/multi-file is G3.)
3. **Seccomp**: `CGO_ENABLED=0` gives a static binary; the Go runtime's syscall
   set is wider than a C hello-world (it spins scheduler threads → `clone`,
   `rt_sigprocmask`, `futex`, `sched_yield`, `mmap`, `sigaltstack`,
   `epoll_*`/`nanosleep` for the netpoller/timer). **Run the allowlist in
   `complain` first** (DEPLOY §8c) with real Go programs and widen
   `staticAllowSyscalls` (`nsjail.go:71-81`) for whatever the Go runtime needs —
   likely `clone`/`clone3`, `futex` (already), `epoll_create1`/`epoll_ctl`/
   `epoll_pwait`, `tgkill`, `rt_sigreturn` (already). If it needs too much, keep
   Go on the **denylist** (still strong: ro-rootfs + empty netns + no_new_privs).

### Contract / config impact
New language id `go`. No contract change (additive).

### Security considerations
Go's goroutine scheduler uses `clone` for OS threads — this is why the C
allowlist (which omits `clone`) may be too tight. Decide per the complain-mode
findings: widen the allowlist for Go, or run Go under the denylist. Either is
acceptable; document the choice.

### Testing
- On-target complain pass: `fmt.Println`, goroutines + channels, `sort`,
  `math` — collect `dmesg` seccomp violations, tune.
- Then `success` under the chosen profile; memory bomb → `memory_exceeded`;
  egress → contained.

### Effort
M (mostly image + seccomp tuning).

---

## G2.2 — Java (VM-compiled — the third shape)

### Motivation
Java is ubiquitous in CS teaching (DS&A courses). It's the first language that is
**neither** interpreted **nor** a static binary: `javac` compiles to bytecode,
then the **JVM** runs it. This breaks two assumptions baked into the current code.

### Current state
Two runtime kinds exist: `interpreted` (write source, run interpreter on it) and
`compiled` (two-jail: build a **static binary**, run it). Java fits neither
cleanly:
- The "artifact" is `Main.class` (or a jar), not an executable — the run jail's
  `run: ["{out}"]` assumption (`compiled.go`) doesn't hold; you run
  `java -cp {dir} Main`.
- The run process is `java` (dynamically linked, huge syscall surface, JIT) — the
  **static seccomp allowlist cannot apply**; Java stays on the denylist.
- JVM memory isn't RLIMIT_AS-boundable (it reserves a large virtual space like
  V8) — bound with `-Xmx` + cgroup, exactly like Node (`capAddressSpace:false`).

### Proposed change
Introduce a **third runtime kind** — `vmCompiledRuntime` — or generalise
`compiledRuntime` so the run step is a spec-provided argv template, not "exec the
artifact". Concretely:

1. **Spec shape** needs: compile argv (`javac -d {dir} {src}`), run argv
   (`java -XX:+UseSerialGC -Xmx{memMB}m -cp {dir} Main`), a `capAddressSpace:false`
   flag (like Node), and an entrypoint class name convention (`Main`).
2. **Public-class filename rule**: `javac` requires the file match the public
   class name. Fix the entrypoint contract: require the student's public class to
   be `Main` and write source to `Main.java` (document this; the backend can
   enforce/inject). Multi-class single-file is fine (nested/package-private
   classes). Multiple public classes = G3.
3. **Image**: add a JDK (e.g. Temurin 21) to the runtime stage. This is heavy
   (~200 MB) — consider a separate image tag or a slim JRE for run + JDK for
   compile.
4. **Memory**: `-Xmx{memMB}m` + rely on cgroup for the authoritative OOM
   (`memory_exceeded`). Add JVM startup overhead to the default timeout for Java,
   or a per-language timeout floor (see G1/per-language tuning) — cold JVM start
   is 100–300 ms and eats into a 3 s budget.
5. **Seccomp**: denylist only (JVM needs `clone`, `openat`, `socket`-adjacent,
   `perf`? — verify perf_event_open isn't needed since the denylist KILLs it;
   JIT may want it → test). Run under denylist; if the JVM trips a denylisted
   syscall, decide whether to relax that one entry for the JVM path.

### Contract / config impact
New language id `java`. Possibly per-language config (memory floor, timeout
floor). New image (or tag). No wire-contract break, but a documented **entrypoint
convention** (public class `Main`).

### Security considerations
Java has the widest syscall surface of any target — it stays on the denylist and
leans on ro-rootfs + empty netns + cgroup + no_new_privs. Confirm the denylist's
KILLs (`ptrace`, `bpf`, `perf_event_open`, module ops, `mount`) don't break the
JVM (they shouldn't for ordinary programs; JFR/perf tooling would, and students
don't need it). **Egress test is mandatory** — the JVM must not reach the network.

### Testing
- Compile+run: `System.out.println`, `Scanner` on stdin, `Collections.sort`,
  `HashMap`. Verify `success`, correct stdout.
- Memory: allocate a huge array → `memory_exceeded` (via cgroup + `-Xmx`).
- Timeout: infinite loop → `timeout`.
- Containment: socket connect → `runtime_error`/blocked.
- Startup budget: confirm a trivial program finishes within the default timeout;
  if not, set a Java timeout floor.

### Effort
L (new runtime shape + heavy image + JVM-specific tuning).

---

## G2.3 — The extension guide

`ADDING-A-LANGUAGE.md` generalises the above into a checklist for the three
shapes, so the next language (Rust → static-compiled; Ruby → interpreted; C# →
VM-compiled) is a recipe, not a research project. Keep it updated as Go and Java
land — they are the reference implementations for shapes 2 and 3.

## Phase G2 acceptance
- `go` and `java` return `success` for ordinary programs, with `memory_exceeded`,
  `timeout`, and egress-contained all proven on-target.
- Seccomp posture for each is documented (Go: allowlist-if-possible else denylist;
  Java: denylist).
- `ADDING-A-LANGUAGE.md` reflects both as worked examples.
