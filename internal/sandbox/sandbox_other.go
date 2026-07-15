//go:build !linux

package sandbox

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"strings"
)

// stubSandbox is the degraded non-Linux backend. The runner's isolation
// guarantees — namespaces, seccomp, rlimits — are Linux-only. On any other OS the
// package still compiles and runs so the service can be built and exercised
// during local development, but it provides NO containment beyond a wall-clock
// deadline. Never deploy the runner off Linux.
type stubSandbox struct{}

// Configure warns loudly that no in-process containment is available. A
// required cgroup still fails closed so production's strict default cannot be
// silently bypassed by building for the wrong OS.
func Configure(_, _, cgroupPolicy, _ string) (Sandbox, error) {
	if strings.EqualFold(strings.TrimSpace(cgroupPolicy), "require") {
		return nil, errors.New("RUNNER_CGROUP=require is unavailable off Linux")
	}
	slog.Warn("containment UNAVAILABLE: this OS is not Linux; the runner provides NO in-process isolation — local development only")
	return &stubSandbox{}, nil
}

func (s *stubSandbox) NetworkIsolated() bool { return false }
func (s *stubSandbox) Backend() string       { return "none" }
func (s *stubSandbox) Ready() bool           { return true }

// MemoryAccounting: the stub provides no containment; there is no cgroup here.
func (s *stubSandbox) MemoryAccounting() string { return "rlimit-only" }

// Command runs the argv directly with no containment (no shell wrapper: ulimit
// semantics are Linux-specific). It still honours the context deadline. There is
// no cgroup accounting off Linux, so RunAccounting is always nil.
func (s *stubSandbox) Command(ctx context.Context, spec Spec) (*exec.Cmd, RunAccounting, error) {
	//nolint:gosec // G204: local-development-only stub; real containment is Linux-only.
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.WorkDir
	return cmd, nil, nil
}

// CancelCmd falls back to killing just the direct child process.
func CancelCmd(cmd *exec.Cmd) func() error {
	return func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}

// LimitProcesses is a no-op off Linux (RLIMIT_NPROC semantics differ or are
// absent).
func LimitProcesses(_ int) error { return nil }

// MaxRSSkb is unavailable off Linux.
func MaxRSSkb(_ *exec.Cmd) int { return 0 }
