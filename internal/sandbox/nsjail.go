//go:build linux

package sandbox

import "strconv"

// seccompPolicy is the kafel filter nsjail installs for every run. It is a
// DENYLIST (default ALLOW, kill the dangerous ones) rather than an allowlist:
// CPython and Node have an enormous syscall surface that an allowlist breaks
// easily, so we start by killing the syscalls a sandbox escape actually needs —
// tracer attach, mount/namespace manipulation, kernel-module and BPF loading,
// key management, reboot/swap, and fd-to-path handle tricks — and tighten per
// language later (a static C binary tolerates a strict allowlist; see the plan).
//
// clone3 is deliberately NOT killed: modern glibc (bookworm) uses it for thread
// creation and posix_spawn, so killing it breaks the interpreter itself. Fork
// bombs are contained by --rlimit_nproc + the concurrency cap, not by blocking
// the clone family.
//
// The unmount syscall is spelled `umount` here, not `umount2`: kafel's amd64
// table names syscall 166 (the only unmount syscall on x86-64) `umount`, and it
// rejects the identifier `umount2` at policy-compile time — which, with
// RUNNER_SANDBOX=require, fails the boot probe closed. Caught by the CI smoke
// job the first time nsjail actually ran.
const seccompPolicy = `POLICY dalivim {
	KILL {
		ptrace, process_vm_readv, process_vm_writev,
		mount, umount, pivot_root, chroot,
		kexec_load, init_module, finit_module, delete_module,
		bpf, setns, unshare,
		add_key, keyctl, request_key,
		reboot, swapon, swapoff,
		open_by_handle_at, name_to_handle_at, perf_event_open
	}
}
USE dalivim DEFAULT ALLOW`

// staticAllowSyscalls is the minimal syscall set a statically-linked C/C++ program
// needs (F-D run jail) — glibc static startup (brk/arch_prctl/set_tid_address/
// set_robust_list/rseq/prlimit64), memory (mmap/munmap/mprotect/mremap/madvise/
// brk), stdio on already-open fds (read/write/…/fstat/ioctl/lseek/poll), signals,
// time, cheap identity getters, and exit — and deliberately EXCLUDES the escape
// surface a compute program never needs: execve/execveat (no re-exec), the socket
// family (no egress), open/openat (no file access), clone/fork, ptrace, and every
// namespace/mount/module syscall. Everything not listed is killed with SIGSYS
// (DEFAULT KILL) — far tighter than the interpreter denylist. The set is a
// starting point tuned on the target via the complain profile; see the plan §3.4.
// Note on kafel identifiers: kafel's amd64 table uses the KERNEL entry names, so
// the stat/uname family carry the `new` prefix — syscall 5 is `newfstat` (not
// `fstat`) and 63 is `newuname` (not `uname`). Using the glibc-common spelling
// fails the policy compile (fail-closed), same class as the umount/umount2 catch.
//
// And nsjail 3.4 bundles a kafel old enough to lack the NAMES of the newest
// syscalls, so those are given by amd64 NUMBER (kafel accepts numbers regardless
// of its name-table age): 332 = statx, 334 = rseq. glibc-static needs both —
// stat() goes through statx on modern glibc, and __libc_start_main registers
// rseq at startup — and they are amd64-only, matching the amd64-only image.
const staticAllowSyscalls = `read, write, readv, writev, pread64, pwrite64,
		close, newfstat, newfstatat, 332, lseek, ioctl, fcntl,
		dup, dup2, dup3, poll, ppoll, pselect6, select,
		brk, mmap, munmap, mprotect, mremap, madvise,
		rt_sigaction, rt_sigprocmask, rt_sigreturn, sigaltstack,
		arch_prctl, set_tid_address, set_robust_list, 334, prlimit64,
		futex, sched_yield, sched_getaffinity, getcpu,
		clock_gettime, clock_getres, clock_nanosleep, nanosleep, gettimeofday, time,
		getpid, gettid, getuid, geteuid, getgid, getegid, getrandom, newuname, sysinfo,
		exit, exit_group, restart_syscall`

// staticAllowlistPolicy renders the static-binary allowlist kafel policy with the
// given default action: "KILL" to enforce (SIGSYS on anything unlisted) or "LOG"
// to only log violations while still running them (target tuning).
func staticAllowlistPolicy(defaultAction string) string {
	return "POLICY dalivim_static {\n\tALLOW {\n\t\t" + staticAllowSyscalls +
		"\n\t}\n}\nUSE dalivim_static DEFAULT " + defaultAction
}

// seccompPolicyFor returns the kafel policy string for a run's chosen profile.
func seccompPolicyFor(p SeccompProfile) string {
	switch p {
	case SeccompStaticEnforce:
		return staticAllowlistPolicy("KILL")
	case SeccompStaticComplain:
		return staticAllowlistPolicy("LOG")
	default:
		return seccompPolicy
	}
}

// jailMount is the path the per-run WorkDir is bind-mounted to inside the jail;
// Argv referencing files (main.py) is resolved against it via --cwd. It aliases
// the exported JailMount so runtimes (which build argv with sandbox.JailPath) and
// the arg builder agree on one path.
const jailMount = JailMount

// nsjailArgs builds the full nsjail argument vector (excluding the binary path)
// for one run. It is pure so the exact containment flags are unit-testable
// without the nsjail binary or unprivileged user namespaces (neither of which
// exists in CI — see the plan's "validation on the target is mandatory" note).
//
// uid/gid are the runner's real ids, mapped to root inside a fresh user
// namespace so the interpreter can read its own script — the same single-id,
// newuidmap-free mapping the F-03 netns path uses, which is what makes nsjail
// work rootless on Railway.
func nsjailArgs(uid, gid int, spec Spec) []string {
	args := []string{
		"--mode", "o", // execve once, then exit — not a persistent daemon
		"--quiet",
		"--disable_proc",                                             // no /proc in the jail: hides host pids, cuts attack surface
		"--iface_no_lo",                                              // even loopback stays down: an empty, egress-less network
		"--time_limit", strconv.Itoa(wallCapSeconds(spec.TimeoutMs)), // hard wall-clock belt
		"--rlimit_cpu", strconv.Itoa(cpuCapSeconds(spec.TimeoutMs)), // RLIMIT_CPU, s
		// Map real uid/gid -> root inside the user namespace (single id, size 1).
		// --user/--group (NOT --uid_mapping/--gid_mapping) is deliberate: both take
		// the same inside:outside:count form, but --user/--group set is_newidmap=0
		// so nsjail writes /proc/PID/{uid,gid}_map DIRECTLY, whereas
		// --uid_mapping/--gid_mapping set is_newidmap=1 and shell out to the setuid
		// newuidmap/newgidmap helpers — which the runtime image does not ship (and,
		// mapping only the caller's own id, does not need). Using the helper flags
		// made the boot probe fail closed on `newgidmap: No such file or directory`.
		"--user", "0:" + strconv.Itoa(uid) + ":1",
		"--group", "0:" + strconv.Itoa(gid) + ":1",
	}

	// Rootfs. Interpreters and the compiler need the whole host rootfs read-only
	// (their binary, libs, headers). A statically-linked artifact needs nothing but
	// itself, so MinimalRootfs omits the host bind entirely — the jail root is then
	// just a fresh tmpfs + /sandbox + /tmp, with no toolchain to re-invoke (D-4).
	if !spec.MinimalRootfs {
		args = append(args, "--bindmount_ro", "/") // arch-agnostic: brings interpreter + libs
	}
	// A fresh, size-capped writable /tmp bounds the F-11 host-OOM vector.
	args = append(args, "--tmpfsmount", "/tmp")

	// The per-run dir at a fixed path. Read-only by default (student code never
	// writes the rootfs); the compile phase alone gets a read-write bind so the
	// compiler can drop its artifact there for the host to pick up.
	workMount := "--bindmount_ro"
	if spec.Writable {
		workMount = "--bindmount"
	}
	args = append(args,
		workMount, spec.WorkDir+":"+jailMount,
		"--cwd", jailMount,
		"--keep_env", // pass exactly the minimal env the caller set on cmd.Env
		"--seccomp_string", seccompPolicyFor(spec.Seccomp),
	)
	// RLIMIT_AS, MB. 0 => leave it unset: V8/Node cannot start under a tight
	// address-space cap (multi-GB virtual reservation), so those runs bound memory
	// via an interpreter heap flag + the container/cgroup limit instead.
	if spec.AddressSpaceMB > 0 {
		args = append(args, "--rlimit_as", strconv.Itoa(spec.AddressSpaceMB))
	}
	// Per-run fork-bomb cap against the jail-private uid (safe here, unlike a
	// process-wide RLIMIT_NPROC). 0 => leave nsjail's default.
	if spec.MaxProcesses > 0 {
		args = append(args, "--rlimit_nproc", strconv.Itoa(spec.MaxProcesses))
	}
	// Cap any single file the child writes (RLIMIT_FSIZE, MB). 0 => leave default.
	if spec.MaxFileSizeMB > 0 {
		args = append(args, "--rlimit_fsize", strconv.Itoa(spec.MaxFileSizeMB))
	}
	args = append(args, "--") // separates the jail flags from the student's argv
	return append(args, spec.Argv...)
}

// cpuCapSeconds is the RLIMIT_CPU budget: just above the wall-clock timeout so a
// CPU-bound loop is killed by the CPU limit even if wall-clock cancellation
// races. Shared by both backends so the cap is identical regardless of which is
// active.
func cpuCapSeconds(timeoutMs int) int { return (timeoutMs+999)/1000 + 1 }

// wallCapSeconds is nsjail's --time_limit (whole seconds, rounded up, min 1). It
// backs up the Go context deadline the executor already applies.
func wallCapSeconds(timeoutMs int) int {
	s := (timeoutMs + 999) / 1000
	if s < 1 {
		return 1
	}
	return s
}
