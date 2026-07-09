//go:build linux

// Package sandbox owns the OS-level containment primitives shared by every
// runtime: per-run process attributes (empty network namespace + own process
// group), the process-group kill that enforces timeouts, and the process-wide
// fork-bomb cap. Keeping the syscall-heavy, security-sensitive code behind this
// small API is what lets the executor stay portable and readable, and is the
// seam where future hardening (nsjail, cgroups, seccomp) will plug in.
package sandbox

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// rlimitNPROC is RLIMIT_NPROC on Linux; package syscall does not export it.
const rlimitNPROC = 6

// isolationCloneflags creates the empty network namespace. CLONE_NEWUSER is what
// lets an unprivileged uid create a network namespace at all; CLONE_NEWNET gives
// the child a namespace with only a (down) loopback and no route off-host.
const isolationCloneflags = syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET

// Sandbox holds the containment decisions resolved once at startup and consulted
// per run. It is safe for concurrent use (its fields are read-only after
// Configure).
type Sandbox struct {
	netns bool
}

// NetworkIsolated reports whether each run executes in an empty network
// namespace (egress denied by THIS process, independent of the deploy network).
func (s *Sandbox) NetworkIsolated() bool { return s.netns }

// Configure resolves the network-isolation policy against what the kernel
// actually permits, logs the decision, and returns the sandbox. Policy is
// auto|require|off (empty => auto): "require" fails closed (returns an error)
// when unprivileged namespaces are unavailable; "auto" falls back to the deploy
// network with a loud warning; "off" disables in-process isolation.
func Configure(policy string) (*Sandbox, error) {
	policy = strings.ToLower(strings.TrimSpace(policy))
	if policy == "" {
		policy = "auto"
	}
	switch policy {
	case "off":
		slog.Warn("network isolation DISABLED (policy=off); egress must be blocked by the deployment network")
		return &Sandbox{netns: false}, nil
	case "auto", "require":
		if probeNetns() {
			slog.Info("network isolation ENABLED: each run executes in an empty network namespace (no egress)")
			return &Sandbox{netns: true}, nil
		}
		if policy == "require" {
			return nil, errors.New("network isolation required but this platform forbids unprivileged network namespaces; cannot guarantee egress denial")
		}
		slog.Warn("network isolation UNAVAILABLE on this platform; code egress is NOT contained by this process — block egress at the deploy layer or set RUNNER_NETWORK_ISOLATION=require to fail closed")
		return &Sandbox{netns: false}, nil
	default:
		return nil, fmt.Errorf("network isolation policy must be auto, require, or off (got %q)", policy)
	}
}

// idMappings maps the runner's real uid/gid to root inside the new user
// namespace so the interpreter can read its own script; the real kernel uid is
// unchanged, so the process-wide RLIMIT_NPROC still contains fork bombs across
// the namespace.
func idMappings() ([]syscall.SysProcIDMap, []syscall.SysProcIDMap) {
	return []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		[]syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
}

// SysProcAttr builds the per-run attributes: always its own process group (so the
// whole tree is killable on timeout) plus, when isolation is enabled, an empty
// network namespace.
func (s *Sandbox) SysProcAttr() *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setpgid: true}
	if s.netns {
		uidMap, gidMap := idMappings()
		attr.Cloneflags = isolationCloneflags
		attr.UidMappings = uidMap
		attr.GidMappings = gidMap
	}
	return attr
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
// Fork-bomb containment must be per-run instead — the per-jail sandbox applies
// it (nsjail --rlimit_nproc) against a jail-private uid. This primitive is kept
// only for that future per-jail path; do not reintroduce it as a global cap.
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
// command, or 0 when unavailable.
func MaxRSSkb(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return 0
	}
	if usage, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		return int(usage.Maxrss)
	}
	return 0
}
