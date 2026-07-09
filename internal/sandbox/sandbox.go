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

	// MemoryMB caps the child's address space (RLIMIT_AS). Already clamped.
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
}

// Sandbox is the containment backend. Command turns a Spec into a ready-to-run
// command with every OS-level control applied; the caller only wires I/O.
// Implementations are safe for concurrent use (state is resolved once at
// Configure and read-only thereafter).
type Sandbox interface {
	// Command builds the contained command for one run, bound to ctx for the
	// wall-clock deadline. It sets the process group and cancel-kill so a timeout
	// takes down the whole tree; it does not touch Stdin/Env/Stdout/Stderr.
	Command(ctx context.Context, spec Spec) *exec.Cmd

	// NetworkIsolated reports whether runs execute with egress denied by THIS
	// process (empty network namespace), independent of the deploy network.
	NetworkIsolated() bool

	// Backend names the active containment backend ("nsjail", "netns", "none")
	// for boot logs and diagnostics.
	Backend() string
}
