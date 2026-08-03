// Package sandbox owns the OS-level containment primitives shared by every
// runtime. It exposes a single seam — Sandbox.Command — that turns a language's
// own argv into a fully contained *exec.Cmd: the caller (a Runtime) describes
// WHAT to run (argv, workdir, limits) and the sandbox decides HOW to contain it
// (namespaces, rlimits, seccomp, process group, timeout kill). Keeping the
// syscall-heavy, security-sensitive code behind this small API is what lets the
// executor stay portable and readable.
//
// Two Linux backends implement the interface behind an operator dial:
//
//   - netns  — the F-03 baseline: empty network namespace + per-run rlimits
//     applied via the child shell. Egress-denied, but no seccomp/tmpfs/ro-rootfs.
//   - nsjail — F-B/F-05/F-11: adds a read-only rootfs, a size-capped tmpfs /tmp,
//     a seccomp denylist, no_new_privs, and per-jail nproc/fsize caps on top.
//
// Selection and fail-closed behaviour live in Configure (sandbox_linux.go). Off
// Linux the package degrades to a no-containment stub for local development only.
package sandbox

import (
	"context"
	"os/exec"
)

// JailMount is the fixed path the per-run WorkDir is bind-mounted to inside every
// jail. Runtimes reference their in-jail files (source, compiled artifact)
// relative to it via JailPath so the caller and the sandbox agree on one path.
const JailMount = "/sandbox"

// JailPath joins name onto the in-jail mount, e.g. JailPath("main.c") ->
// "/sandbox/main.c".
func JailPath(name string) string { return JailMount + "/" + name }

// Spec describes one run for the sandbox to contain. It carries only what the
// containment decision needs; the caller sets Stdin/Env/Stdout/Stderr on the
// returned command itself (those are the runtime's concern, not the jail's).
type Spec struct {
	// Argv is the runtime's own command, workdir-relative where it references
	// files (e.g. {"python3", "-I", "main.py"}). The source is NEVER part of the
	// argv — it is written to a file in WorkDir — so there is no shell injection
	// even though a backend may wrap the argv in a shell to apply ulimits.
	Argv []string

	// WorkDir is the throwaway per-run directory holding the source. It is the
	// child's working directory (netns: cmd.Dir; nsjail: bind-mounted read-only
	// to /sandbox and set as --cwd).
	WorkDir string

	// TimeoutMs is the wall-clock budget already clamped by the Service. Backends
	// derive an RLIMIT_CPU cap just above it so a CPU-bound loop still dies when
	// wall-clock cancellation races. The executor owns the matching context
	// deadline and passes that context to Command.
	TimeoutMs int

	// AddressSpaceMB caps the child's virtual address space (RLIMIT_AS), in MB.
	// 0 omits OUR flag on purpose: a runtime like V8/Node reserves a multi-GB
	// virtual "cage" at startup that a tight RLIMIT_AS refuses ("Failed to reserve
	// virtual memory for CodeRange"), so such runtimes pass 0 here and bound real
	// memory another way — an interpreter heap flag plus the container/cgroup
	// memory limit. CPython tolerates the hard cap, so Python passes its budget.
	//
	// CAREFUL: omitting the flag is NOT "unlimited". nsjail applies its OWN default
	// (--rlimit_as 4096 MB) to anything we leave unspecified, so 0 means "nsjail's
	// 4 GB", not "no cap". Node/Go/the JVM fit inside 4 GB; the CoreCLR does not
	// (it aborts at GC heap init with 0x8007000E) — that runtime must additionally
	// set UnlimitedAddressSpace. See UnlimitedAddressSpace.
	AddressSpaceMB int

	// UnlimitedAddressSpace lifts RLIMIT_AS entirely (nsjail --rlimit_as inf) for a
	// runtime that cannot start under ANY address-space cap, including nsjail's own
	// 4 GB default. Only the CoreCLR needs this today: it reserves its virtual arena
	// eagerly and dies at startup under 4 GB ("GC heap initialization failed with
	// error 0x8007000E"), in BOTH the compile (csc is itself a .NET program) and the
	// run phase.
	//
	// This trades a virtual-memory bound for the cgroup one: real memory stays
	// bounded by the per-run memory.max (F-E/R6), which is the authoritative RSS
	// accounting anyway. Without a delegated cgroup there is then no per-run memory
	// ceiling for such a runtime — the reason the C# runtime declines to start in
	// that mode (see the executor's cgroup requirement).
	//
	// Ignored when AddressSpaceMB > 0: an explicit cap always wins over the lift.
	UnlimitedAddressSpace bool

	// MaxOpenFiles raises the jail's RLIMIT_NOFILE (nsjail --rlimit_nofile). 0 keeps
	// nsjail's default of 32 descriptors, which is ample for an interpreter or a
	// static artifact and is a real containment property (it bounds fd exhaustion).
	//
	// A managed runtime that memory-maps one file per assembly blows straight
	// through 32: the CoreCLR fails to load System.Console with a bogus
	// "Could not load file or assembly" — an ENOENT-shaped message for what is
	// really descriptor exhaustion. Such a runtime raises this to what it needs.
	MaxOpenFiles int

	// MemoryMB is the run's real-memory budget in MB. When the nsjail backend has a
	// delegated cgroup v2 subtree (F-E/R6), it becomes the per-run `memory.max`:
	// authoritative RSS accounting that bounds runtimes RLIMIT_AS cannot (V8/Node)
	// and lets `memory_exceeded` be classified from the kernel OOM event instead of
	// a stderr substring. 0, or no delegated cgroup, leaves it unenforced here and
	// the AddressSpaceMB/heap-flag path remains the only bound.
	MemoryMB int

	// MaxProcesses is the per-run process cap for fork-bomb containment
	// (RLIMIT_NPROC inside the jail). Honoured only by the nsjail backend against
	// a jail-private uid; the netns backend ignores it on purpose — a process-wide
	// RLIMIT_NPROC is a per-uid shared resource that starves the Go runtime
	// ("errno=11"), which is exactly why fork-bomb containment moved per-jail.
	MaxProcesses int

	// MaxFileSizeMB caps the size of any single file the child may write
	// (RLIMIT_FSIZE). Honoured only by the nsjail backend (where writes land in a
	// size-capped tmpfs /tmp); 0 leaves it unset.
	MaxFileSizeMB int

	// TmpfsSizeMB sizes the jail's writable /tmp tmpfs (nsjail --tmpfs_size). 0 uses
	// nsjail's small default — enough for gcc's intermediates. The Go compile jail
	// needs a larger /tmp because a cold `go build` writes ~30 MB+ of stdlib objects
	// into GOCACHE (which lives on /tmp). tmpfs is RAM-backed and counts against the
	// jail's cgroup memory.max, so this is a ceiling, not a reservation.
	TmpfsSizeMB int

	// Writable binds WorkDir into the jail READ-WRITE at /sandbox instead of the
	// default read-only. Only the compile phase of a compiled language needs it —
	// so the compiler can write its artifact into the per-run dir, which the host
	// then reads and hands (read-only) to the separate run jail. Execution phases
	// always leave this false (invariant: student code never writes the rootfs).
	Writable bool

	// MinimalRootfs omits the read-only host-rootfs bind, so the jail root is only
	// a fresh tmpfs plus /sandbox and /tmp — no interpreter, no libraries, and
	// crucially NO TOOLCHAIN. It is the F-D run jail for a statically-linked
	// artifact: the binary needs nothing but itself, and with nothing else present
	// there is nothing to execve and no compiler to re-invoke at runtime (D-4).
	// Interpreted runs and the compile phase need the full rootfs, so leave false.
	MinimalRootfs bool

	// TracerProcfs keeps a procfs mounted in the jail instead of the usual
	// --disable_proc. It exists for one reason: a debugger cannot work without it.
	// Measured — with /proc absent, gdb cannot read /proc/<pid>/maps, so it never
	// learns a PIE binary's load offset and every breakpoint lands in the dynamic
	// loader instead of the student's code (docs/future/B2-TRACER-SPIKE.md).
	//
	// The exposure is NOT the host's /proc: the jail already has a fresh PID
	// namespace, so this procfs shows only the jail's own handful of processes
	// (measured: 4). Still, it is strictly more surface than --disable_proc, so it
	// is opt-in and used ONLY by the trace jail of a compiled language.
	TracerProcfs bool

	// Seccomp selects the syscall policy. The default (zero value) is the shared
	// DENYLIST that fits the huge syscall surface of interpreters and the compiler.
	// A static compiled artifact makes very few syscalls, so the F-D run jail can
	// use a tight ALLOWLIST (SeccompStaticEnforce) — anything outside the set is
	// killed with SIGSYS — or SeccompStaticComplain to log violations without
	// killing, for tuning the set on the target.
	Seccomp SeccompProfile
}

// SeccompProfile selects which kafel policy the nsjail backend installs for a run.
type SeccompProfile int

const (
	// SeccompDenylist (default) keeps the dangerous syscalls off and allows the
	// rest — the right shape for CPython/Node/gcc, whose syscall surface is huge.
	SeccompDenylist SeccompProfile = iota
	// SeccompStaticEnforce is the tight allowlist for a static binary: DEFAULT KILL,
	// only the minimal set a self-contained program needs is permitted.
	SeccompStaticEnforce
	// SeccompStaticComplain is the same allowlist with DEFAULT LOG — the syscall
	// still runs but is logged, so the enforced set can be tuned on the target
	// before it is switched to kill.
	SeccompStaticComplain
	// SeccompTracer is the denylist MINUS ptrace/process_vm_readv/process_vm_writev,
	// for the step-through tutor of a COMPILED language (B.2): a native binary has
	// no interpreter hook, so stepping it means a debugger, and a debugger means
	// ptrace. Everything else in the denylist stays. Pair it with TracerProcfs —
	// a debugger needs both — and use it ONLY for mode=trace. See tracerPolicy.
	SeccompTracer
)

// RunAccounting exposes authoritative per-run resource facts a backend gathered
// out-of-band — today, cgroup v2 memory accounting under the nsjail backend. It
// is returned alongside the command and consulted AFTER the run completes:
//
//	cmd, acct, err := sb.Command(ctx, spec)
//	if err != nil { /* infrastructure failure: do not execute */ }
//	if acct != nil { defer acct.Close() }
//	... run cmd ...
//	if acct != nil && acct.OOMKilled() { /* memory_exceeded, authoritatively */ }
//
// A nil RunAccounting means the backend has no out-of-band accounting for this
// run (no delegated cgroup, or the netns/stub backend); callers must nil-check.
type RunAccounting interface {
	// OOMKilled reports whether the kernel OOM-killed a process in this run's
	// cgroup (memory.events oom_kill/oom_group_kill > 0). This is the
	// deterministic memory_exceeded signal that replaces the stderr heuristic.
	OOMKilled() bool

	// PeakMemoryKB is the run's peak memory (cgroup memory.peak) in KiB, or 0 when
	// unavailable — authoritative RSS, unlike the best-effort getrusage Maxrss.
	PeakMemoryKB() int

	// Close releases the per-run cgroup. Safe to call exactly once after the run.
	Close()
}

// Sandbox is the containment backend. Command turns a Spec into a ready-to-run
// command with every OS-level control applied; the caller only wires I/O.
// Implementations are safe for concurrent use (state is resolved once at
// Configure and read-only thereafter).
type Sandbox interface {
	// Command builds the contained command for one run, bound to ctx for the
	// wall-clock deadline. It sets the process group and cancel-kill so a timeout
	// takes down the whole tree; it does not touch Stdin/Env/Stdout/Stderr. The
	// returned RunAccounting is non-nil only when the backend attached out-of-band
	// accounting (a per-run cgroup) to this command; callers must nil-check it.
	Command(ctx context.Context, spec Spec) (*exec.Cmd, RunAccounting, error)

	// Ready reports whether the backend can still uphold the containment posture
	// it promised at boot. A required per-run control that fails after startup
	// makes this false so /readyz removes the instance from service.
	Ready() bool

	// NetworkIsolated reports whether runs execute with egress denied by THIS
	// process (empty network namespace), independent of the deploy network.
	NetworkIsolated() bool

	// Backend names the active containment backend ("nsjail", "netns", "none")
	// for boot logs and diagnostics.
	Backend() string

	// MemoryAccounting names the active per-run memory-bound mechanism:
	// "cgroup-v2:<parent>" when a delegated cgroup v2 subtree gives every run an
	// authoritative memory.max (and classifies memory_exceeded from the kernel OOM
	// event), or "rlimit-only" when memory is bounded solely by RLIMIT_AS / the
	// interpreter heap flag. It is the runtime-observable posture the escape corpus
	// probes (via /readyz) to decide whether the RLIMIT_AS-incompatible runtimes
	// (Go/JS/Java) are actually contained on a memory bomb here, or only on-target.
	MemoryAccounting() string
}
