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
// Every identifier here must exist in kafel's per-arch syscall table or the
// policy fails to compile and the whole jail refuses to start. Note kafel names
// the amd64 unmount syscall (nr 166) `umount`, NOT `umount2` — do not "fix" it.
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

// jailMount is the path the per-run WorkDir is bind-mounted to inside the jail;
// Argv referencing files (main.py) is resolved against it via --cwd.
//
// It MUST be a directory that already exists in the (read-only) rootfs: with
// `--bindmount_ro /` the jail root is the host root mounted read-only, so nsjail
// cannot mkdir a fresh mountpoint like /sandbox on it ("Permission denied").
// /mnt is a standard, empty FHS directory present in the base image, so binding
// over it needs no mkdir.
const jailMount = "/mnt"

// nsjailArgs builds the full nsjail argument vector (excluding the binary path)
// for one run. It is pure so the exact containment flags are unit-testable
// without the nsjail binary or unprivileged user namespaces (neither of which
// exists in CI — see the plan's "validation on the target is mandatory" note).
//
// It deliberately does NOT pass --uid_mapping/--gid_mapping. An explicit mapping
// makes nsjail shell out to the setuid newuidmap/newgidmap helpers (from the
// uidmap package, absent in a minimal rootless image); without them nsjail's
// default maps the process's own uid/gid identically into the namespace via a
// direct /proc write and handles /proc/pid/setgroups itself — no helper, which
// is what makes it work rootless in a plain container. Bonus: the student then
// runs as a non-root uid *inside* the jail too, not uid 0.
func nsjailArgs(spec Spec) []string {
	args := []string{
		"--mode", "o", // execve once, then exit — not a persistent daemon
		"--quiet",
		"--disable_proc",                                             // no /proc in the jail: hides host pids, cuts attack surface
		"--iface_no_lo",                                              // even loopback stays down: an empty, egress-less network
		"--time_limit", strconv.Itoa(wallCapSeconds(spec.TimeoutMs)), // hard wall-clock belt
		"--rlimit_as", strconv.Itoa(spec.MemoryMB), // RLIMIT_AS, MB
		"--rlimit_cpu", strconv.Itoa(cpuCapSeconds(spec.TimeoutMs)), // RLIMIT_CPU, s
		// Whole host rootfs read-only (arch-agnostic: brings the interpreter and
		// its libs) + a fresh, size-capped writable /tmp (bounds the F-11 host-OOM
		// vector) + the source dir mounted read-only at a fixed path.
		"--bindmount_ro", "/",
		"--tmpfsmount", "/tmp",
		"--bindmount_ro", spec.WorkDir + ":" + jailMount,
		"--cwd", jailMount,
		"--keep_env", // pass exactly the minimal env the caller set on cmd.Env
		"--seccomp_string", seccompPolicy,
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
