//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// rlimitNPROC is RLIMIT_NPROC on Linux; package syscall does not export it.
const rlimitNPROC = 6

// isolationCloneflags creates the empty network namespace for the netns backend.
// CLONE_NEWUSER is what lets an unprivileged uid create a network namespace at
// all; CLONE_NEWNET gives the child a namespace with only a (down) loopback and
// no route off-host.
const isolationCloneflags = syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET

// Configure selects and validates the containment backend against what the
// kernel actually permits, logs the decision, and returns the ready Sandbox.
//
// sandboxPolicy (RUNNER_SANDBOX) is auto|require|off (empty => auto):
//   - off:     use the netns backend only (F-03 baseline).
//   - auto:    probe nsjail; use it when it works, else warn and fall back to netns.
//   - require: use nsjail and FAIL CLOSED at boot if the probe fails.
//
// netPolicy (RUNNER_NETWORK_ISOLATION) governs the netns backend's egress
// guarantee and is consulted only when nsjail is not the active backend (nsjail
// always runs each command in its own empty network namespace).
//
// cgroupPolicy (RUNNER_CGROUP) is auto|require|off. Per-run cgroups are provided
// by the nsjail backend; "require" therefore also forbids selecting/falling back
// to netns. It fails closed if nsjail or the delegated subtree is unusable;
// "auto" falls back to rlimit-only bounds with a warning; "off" disables it.
func Configure(sandboxPolicy, netPolicy, cgroupPolicy, cgroupMount string) (Sandbox, error) {
	cgPolicy := normalizePolicy(cgroupPolicy)
	switch cgPolicy {
	case "auto", "require", "off":
	default:
		return nil, fmt.Errorf("RUNNER_CGROUP must be auto, require, or off (got %q)", cgPolicy)
	}

	switch policy := normalizePolicy(sandboxPolicy); policy {
	case "off":
		if cgPolicy == "require" {
			return nil, errors.New("RUNNER_CGROUP=require needs the nsjail backend, but RUNNER_SANDBOX=off")
		}
		slog.Info("nsjail DISABLED (RUNNER_SANDBOX=off); using netns-only backend (F-03)")
		return configureNetns(netPolicy)
	case "auto", "require":
		nj, detail, err := tryNsjail()
		if err == nil {
			cg, cgErr := resolveCgroup(cgPolicy, cgroupMount)
			if cgErr != nil {
				return nil, cgErr // RUNNER_CGROUP=require but the subtree is unusable
			}
			nj.cg = cg
			nj.cgRequired = cgPolicy == "require"
			slog.Info("nsjail ENABLED: each run is contained by a read-only rootfs, mount/pid/ipc/user/net namespaces, a size-capped tmpfs /tmp, a seccomp denylist, no_new_privs, and per-jail rlimits",
				"bin", nj.bin, "memory_accounting", cgroupLabel(cg))
			return nj, nil
		}
		if policy == "require" {
			return nil, fmt.Errorf("RUNNER_SANDBOX=require but nsjail is unavailable: %s", detail)
		}
		if cgPolicy == "require" {
			return nil, fmt.Errorf("RUNNER_CGROUP=require needs nsjail, but nsjail is unavailable: %s", detail)
		}
		slog.Warn("nsjail unavailable; falling back to netns-only backend (F-03) — set RUNNER_SANDBOX=require to fail closed instead",
			"detail", detail)
		return configureNetns(netPolicy)
	default:
		return nil, fmt.Errorf("RUNNER_SANDBOX must be auto, require, or off (got %q)", policy)
	}
}

// resolveCgroup applies the RUNNER_CGROUP dial for the nsjail backend. off =>
// nil (rlimit-only, today's behaviour). auto => probe the delegated subtree; use
// it when usable, else fall back to rlimit-only with a loud warning. require =>
// probe and FAIL CLOSED when the subtree is unusable. A nil manager (any
// non-require failure) is always safe: runs keep their RLIMIT_AS/heap bound.
func resolveCgroup(policy, mount string) (*cgroupManager, error) {
	switch p := normalizePolicy(policy); p {
	case "off":
		slog.Info("cgroup memory accounting DISABLED (RUNNER_CGROUP=off); per-run memory bound by RLIMIT_AS / interpreter heap flag only")
		return nil, nil
	case "auto", "require":
		cg, detail, ok := probeCgroup(mount)
		if ok {
			slog.Info("cgroup memory accounting ENABLED: each run gets a cgroup v2 leaf with memory.max/pids.max; memory_exceeded is classified from the kernel OOM event",
				"mount", mount)
			return cg, nil
		}
		if p == "require" {
			return nil, fmt.Errorf("RUNNER_CGROUP=require but no usable delegated cgroup: %s", detail)
		}
		slog.Warn("cgroup memory accounting UNAVAILABLE; falling back to RLIMIT_AS / heap-flag bounds — delegate a writable cgroup v2 subtree and set RUNNER_CGROUP_MOUNT, or RUNNER_CGROUP=require to fail closed",
			"detail", detail)
		return nil, nil
	default:
		return nil, fmt.Errorf("RUNNER_CGROUP must be auto, require, or off (got %q)", p)
	}
}

// cgroupLabel is the boot-log value for the active memory-accounting mode.
func cgroupLabel(cg *cgroupManager) string {
	if cg == nil {
		return "rlimit-only"
	}
	return "cgroup-v2:" + cg.parent
}

// normalizePolicy lower-cases/trims a policy value, defaulting empty to "auto".
func normalizePolicy(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return "auto"
	}
	return p
}

// ---------------------------------------------------------------------------
// netns backend (F-03): empty network namespace + per-run rlimits via the shell
// ---------------------------------------------------------------------------

// netnsSandbox is the baseline backend: it runs each command in its own process
// group and (when permitted) an empty network namespace, applying the memory and
// CPU rlimits through the child shell. It does NOT apply a per-run process or
// file-size cap: a process-wide RLIMIT_NPROC is a per-uid shared resource that
// starves the Go runtime, so fork-bomb containment lives in the nsjail backend.
type netnsSandbox struct {
	netns bool
}

func (s *netnsSandbox) NetworkIsolated() bool { return s.netns }
func (s *netnsSandbox) Backend() string       { return "netns" }
func (s *netnsSandbox) Ready() bool           { return true }

// MemoryAccounting: the netns backend has no per-run cgroup, so memory is
// bounded by RLIMIT_AS / the interpreter heap flag only.
func (s *netnsSandbox) MemoryAccounting() string { return cgroupLabel(nil) }

// Command wraps the argv in a shell that sets the per-run rlimits, then execs it
// (exec so the shell does not linger as an extra process in the group). Only -v
// (address space) and -t (CPU seconds) are applied: the container's /bin/sh is
// dash, which lacks `ulimit -u`, so the process cap is a per-jail (nsjail)
// concern. The source is written to a file in WorkDir, never the command line.
func (s *netnsSandbox) Command(ctx context.Context, spec Spec) (*exec.Cmd, RunAccounting, error) {
	// Address-space cap is optional: AddressSpaceMB==0 leaves RLIMIT_AS unset (the
	// V8/Node path, which a tight cap would break). CPU seconds are always applied.
	var prefix string
	if spec.AddressSpaceMB > 0 {
		prefix = fmt.Sprintf("ulimit -v %d; ", spec.AddressSpaceMB*1024)
	}
	//nolint:gosec // G204: executing submitted code is the runner's purpose; the
	// shell string interpolates only integer limits and a single-quoted argv, the
	// source lives in a file, and the child runs sandboxed (netns, rlimits, env).
	shellCmd := fmt.Sprintf("%sulimit -t %d; exec %s",
		prefix, cpuCapSeconds(spec.TimeoutMs), shJoin(spec.Argv))
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", shellCmd)
	cmd.Dir = spec.WorkDir
	cmd.SysProcAttr = s.sysProcAttr()
	cmd.Cancel = CancelCmd(cmd)
	// The netns backend has no cgroup accounting; memory stays rlimit-bound.
	return cmd, nil, nil
}

// sysProcAttr builds the per-run attributes: always its own process group (so the
// whole tree is killable on timeout) plus, when isolation is enabled, an empty
// network namespace.
func (s *netnsSandbox) sysProcAttr() *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setpgid: true}
	if s.netns {
		uidMap, gidMap := idMappings()
		attr.Cloneflags = isolationCloneflags
		attr.UidMappings = uidMap
		attr.GidMappings = gidMap
	}
	return attr
}

// configureNetns resolves the network-isolation policy against what the kernel
// permits and returns the netns backend. Policy is auto|require|off (empty =>
// auto): "require" fails closed when unprivileged namespaces are unavailable;
// "auto" falls back to the deploy network with a loud warning; "off" disables
// in-process network isolation.
func configureNetns(policy string) (*netnsSandbox, error) {
	switch p := normalizePolicy(policy); p {
	case "off":
		slog.Warn("network isolation DISABLED (policy=off); egress must be blocked by the deployment network")
		return &netnsSandbox{netns: false}, nil
	case "auto", "require":
		if probeNetns() {
			slog.Info("network isolation ENABLED: each run executes in an empty network namespace (no egress)")
			return &netnsSandbox{netns: true}, nil
		}
		if p == "require" {
			return nil, errors.New("network isolation required but this platform forbids unprivileged network namespaces; cannot guarantee egress denial")
		}
		slog.Warn("network isolation UNAVAILABLE on this platform; code egress is NOT contained by this process — block egress at the deploy layer or set RUNNER_NETWORK_ISOLATION=require to fail closed")
		return &netnsSandbox{netns: false}, nil
	default:
		return nil, fmt.Errorf("network isolation policy must be auto, require, or off (got %q)", p)
	}
}

// idMappings maps the runner's real uid/gid to root inside the new user
// namespace so the interpreter can read its own script; the real kernel uid is
// unchanged.
func idMappings() ([]syscall.SysProcIDMap, []syscall.SysProcIDMap) {
	return []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		[]syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
}

// probeNetns reports whether this process can create an empty network namespace
// for an unprivileged child. Hardened kernels / nested sandboxes make it false.
func probeNetns() bool {
	cmd := exec.Command("/bin/true")
	uidMap, gidMap := idMappings()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  isolationCloneflags,
		UidMappings: uidMap,
		GidMappings: gidMap,
	}
	return cmd.Run() == nil
}

// ---------------------------------------------------------------------------
// nsjail backend (F-B / F-05 / F-11)
// ---------------------------------------------------------------------------

// nsjailSandbox runs each command under a probed nsjail binary. Every run gets a
// read-only rootfs, its own mount/pid/ipc/user/net namespaces, a size-capped
// tmpfs /tmp, a seccomp denylist, no_new_privs, and per-jail nproc/fsize caps.
type nsjailSandbox struct {
	bin          string
	uid          int
	gid          int
	cg           *cgroupManager // nil => rlimit-only memory bound (no delegated cgroup)
	cgRequired   bool
	cgroupFailed atomic.Bool // sticky: a required per-run cgroup failed after boot
}

// NetworkIsolated is always true: nsjail clones a fresh, empty network namespace
// for every run (loopback stays down via --iface_no_lo).
func (s *nsjailSandbox) NetworkIsolated() bool { return true }
func (s *nsjailSandbox) Backend() string       { return "nsjail" }
func (s *nsjailSandbox) Ready() bool           { return !s.cgroupFailed.Load() }

// MemoryAccounting reports the resolved per-run memory bound: "cgroup-v2:<parent>"
// when a delegated cgroup gives each run an authoritative memory.max, else
// "rlimit-only". This is the same label logged at boot (cgroupLabel), surfaced at
// runtime so a caller can tell whether the RLIMIT_AS-incompatible runtimes are
// contained on a memory bomb here.
func (s *nsjailSandbox) MemoryAccounting() string { return cgroupLabel(s.cg) }

// Command builds the nsjail invocation for one run. nsjail owns the child's cwd,
// namespaces, and rlimits; we still put nsjail itself in its own process group so
// a timeout SIGKILLs nsjail and every descendant together. The caller's cmd.Env
// flows to the child unchanged via --keep_env.
//
// When a delegated cgroup is present (F-E/R6), the run also gets a cgroup v2 leaf
// with memory.max/pids.max, and nsjail is cloned straight into it (CLONE_INTO_
// CGROUP via UseCgroupFD) so the whole jailed tree is accounted and OOM-bounded
// there. The returned RunAccounting exposes the OOM verdict + peak. Under
// RUNNER_CGROUP=require, a per-run setup failure is an infrastructure error: no
// command is created, the run is refused, and readiness stays degraded until the
// process is replaced. auto retains its explicit rlimit-only fallback.
func (s *nsjailSandbox) Command(ctx context.Context, spec Spec) (*exec.Cmd, RunAccounting, error) {
	attr := &syscall.SysProcAttr{Setpgid: true}

	var acct RunAccounting
	if s.cg != nil {
		if rc, err := s.cg.begin(spec.MemoryMB, spec.MaxProcesses); err != nil {
			if s.cgRequired {
				s.cgroupFailed.Store(true)
				slog.Error("required per-run cgroup unavailable; refusing run", "err", err)
				return nil, nil, fmt.Errorf("required per-run cgroup unavailable: %w", err)
			}
			slog.Warn("per-run cgroup unavailable for this run; falling back to rlimit bound", "err", err)
		} else {
			attr.UseCgroupFD = true
			attr.CgroupFD = rc.fd()
			acct = rc
		}
	}

	cmd := exec.CommandContext(ctx, s.bin, nsjailArgs(s.uid, s.gid, spec)...)
	cmd.SysProcAttr = attr
	cmd.Cancel = CancelCmd(cmd)
	return cmd, acct, nil
}

// tryNsjail locates the nsjail binary and proves it actually works in this
// environment (user namespace creation, seccomp acceptance) by running /bin/true
// inside a real jail. It returns a human-readable detail on failure for the boot
// log or the fail-closed error.
func tryNsjail() (*nsjailSandbox, string, error) {
	bin, err := exec.LookPath("nsjail")
	if err != nil {
		return nil, "nsjail binary not found on PATH", err
	}
	s := &nsjailSandbox{bin: bin, uid: os.Getuid(), gid: os.Getgid()}
	if detail, ok := probeNsjail(s); !ok {
		return nil, detail, errors.New(detail)
	}
	return s, "", nil
}

// probeNsjail runs /bin/true through a real jail to confirm the kernel permits
// the namespaces and accepts the seccomp policy. It mirrors the F-03 netns probe:
// the boot log (nsjail ENABLED vs fallback) is the source of truth on a platform
// where nsjail cannot be exercised in CI.
func probeNsjail(s *nsjailSandbox) (string, bool) {
	dir, err := os.MkdirTemp("", "dalivim-probe-*")
	if err != nil {
		return "cannot create probe workdir: " + err.Error(), false
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Probe the jail itself, never the cgroup (s.cg is nil here — the probe runs
	// before Configure attaches one), so a cgroup misconfig can't fail the nsjail
	// boot probe; resolveCgroup handles the cgroup dial separately.
	cmd, _, commandErr := s.Command(ctx, Spec{
		Argv:           []string{"/bin/true"},
		WorkDir:        dir,
		TimeoutMs:      2000,
		AddressSpaceMB: 128,
		MaxProcesses:   64,
		MaxFileSizeMB:  4,
	})
	if commandErr != nil {
		return "could not construct probe command: " + commandErr.Error(), false
	}
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Sprintf("probe run failed: %v: %s", err, strings.TrimSpace(string(out))), false
	}
	return "", true
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// shJoin renders argv as a POSIX-sh-safe command string for the netns backend's
// `exec`. The argv is runtime-defined (never student source), but every token is
// single-quoted so a path containing a space or shell metacharacter can never
// alter the command.
func shJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

// CancelCmd is assigned to exec.Cmd.Cancel: it SIGKILLs the whole process group
// so grandchildren die with the child when a run is cancelled or times out.
func CancelCmd(cmd *exec.Cmd) func() error {
	return func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// LimitProcesses lowers the soft RLIMIT_NPROC for THIS process (inherited by all
// executed children). The soft limit never exceeds the hard one.
//
// It is deliberately NOT called at startup: RLIMIT_NPROC is enforced per
// real-uid, so lowering it process-wide throttles every process this uid runs
// and can make the runner fail to fork ("errno=11") on a busy shared host.
// Fork-bomb containment is per-run instead — the nsjail backend applies it
// (--rlimit_nproc) against a jail-private uid. This primitive is kept only for
// that reasoning's paper trail and its test; do not reintroduce it as a global cap.
func LimitProcesses(n int) error {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(rlimitNPROC, &lim); err != nil {
		return err
	}
	cur := uint64(n) //#nosec G115 -- operator-set positive process cap.
	if lim.Max != 0 && cur > lim.Max {
		cur = lim.Max
	}
	lim.Cur = cur
	return syscall.Setrlimit(rlimitNPROC, &lim)
}

// MaxRSSkb returns the peak resident set size (KiB) reported for a finished
// command, or 0 when unavailable. Under the nsjail backend this reflects nsjail's
// own peak rather than the child's (authoritative per-run accounting arrives with
// cgroups v2 — see the plan's F-E/F-F), so treat it as best-effort.
func MaxRSSkb(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return 0
	}
	if usage, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		return int(usage.Maxrss)
	}
	return 0
}
