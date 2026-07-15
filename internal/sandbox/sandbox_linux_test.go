package sandbox

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
)

// TestNetnsSysProcAttr_SetpgidAlways ensures the process-group setup used for
// timeout kills is present whether or not network isolation is enabled.
func TestNetnsSysProcAttr_SetpgidAlways(t *testing.T) {
	off := &netnsSandbox{netns: false}
	if attr := off.sysProcAttr(); !attr.Setpgid || attr.Cloneflags != 0 {
		t.Fatalf("isolation off: want Setpgid and no cloneflags, got %+v", attr)
	}

	if !probeNetns() {
		return // cannot assert the enabled path on this platform
	}
	on := &netnsSandbox{netns: true}
	attr := on.sysProcAttr()
	if !attr.Setpgid {
		t.Fatal("isolation on: Setpgid must still be set so timeouts can kill the group")
	}
	if attr.Cloneflags&syscall.CLONE_NEWNET == 0 || attr.Cloneflags&syscall.CLONE_NEWUSER == 0 {
		t.Fatalf("isolation on: want CLONE_NEWUSER|CLONE_NEWNET, got cloneflags=%#x", attr.Cloneflags)
	}
}

// TestConfigure_RejectsUnknownSandboxPolicy pins the closed RUNNER_SANDBOX vocabulary.
func TestConfigure_RejectsUnknownSandboxPolicy(t *testing.T) {
	if _, err := Configure("banana", "auto", "off", ""); err == nil {
		t.Fatal("expected an error for an unknown sandbox policy")
	}
}

// TestConfigure_OffUsesNetnsBackend confirms RUNNER_SANDBOX=off selects the
// netns backend, and that a netns policy of off disables egress isolation.
func TestConfigure_OffUsesNetnsBackend(t *testing.T) {
	sb, err := Configure("off", "off", "off", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sb.Backend() != "netns" {
		t.Fatalf("want netns backend, got %q", sb.Backend())
	}
	if sb.NetworkIsolated() {
		t.Fatal("net policy=off must disable network isolation")
	}
}

// TestConfigure_RequireFailsClosedWithoutNsjail proves the fail-closed contract:
// when nsjail cannot be used and the operator demanded it, boot must error rather
// than silently downgrade. (nsjail is absent in CI, which is exactly this case.)
func TestConfigure_RequireFailsClosedWithoutNsjail(t *testing.T) {
	if _, err := probeNsjailAvailable(); err == nil {
		t.Skip("nsjail is available here; cannot exercise the fail-closed path")
	}
	if _, err := Configure("require", "auto", "off", ""); err == nil {
		t.Fatal("RUNNER_SANDBOX=require must fail closed when nsjail is unavailable")
	}
}

// TestConfigure_AutoFallsBackToNetns confirms that without nsjail, auto degrades
// to the netns backend instead of failing.
func TestConfigure_AutoFallsBackToNetns(t *testing.T) {
	if _, err := probeNsjailAvailable(); err == nil {
		t.Skip("nsjail is available here; auto would select it, not the fallback")
	}
	sb, err := Configure("auto", "off", "off", "")
	if err != nil {
		t.Fatalf("auto must not fail when nsjail is absent: %v", err)
	}
	if sb.Backend() != "netns" {
		t.Fatalf("want netns fallback, got %q", sb.Backend())
	}
}

// A required cgroup cannot exist on the netns fallback. This closes the gap
// where production's strict cgroup default could otherwise be bypassed merely
// because RUNNER_SANDBOX remained auto and nsjail was absent.
func TestConfigure_CgroupRequireForbidsNetnsFallback(t *testing.T) {
	if _, err := Configure("off", "off", "require", ""); err == nil {
		t.Fatal("RUNNER_CGROUP=require must reject an explicitly disabled nsjail backend")
	}
	if _, err := probeNsjailAvailable(); err == nil {
		t.Skip("nsjail is available here; cannot exercise the automatic fallback")
	}
	if _, err := Configure("auto", "off", "require", "/definitely/not/a/cgroup"); err == nil {
		t.Fatal("RUNNER_CGROUP=require must fail when auto cannot activate nsjail")
	}
}

// TestNsjailCommand_RequiredCgroupFailsClosed pins RUN-01 at the per-run seam:
// losing the delegated subtree after a successful boot must not construct an
// executable command, and the failure must make readiness sticky-false.
func TestNsjailCommand_RequiredCgroupFailsClosed(t *testing.T) {
	s := &nsjailSandbox{
		bin:        "/bin/true",
		cg:         &cgroupManager{parent: filepath.Join(t.TempDir(), "missing")},
		cgRequired: true,
	}
	cmd, acct, err := s.Command(context.Background(), Spec{
		Argv: []string{"/bin/true"}, WorkDir: t.TempDir(), MemoryMB: 64, MaxProcesses: 8,
	})
	if err == nil || cmd != nil || acct != nil {
		t.Fatalf("required cgroup failure must return (nil,nil,error), got (%v,%v,%v)", cmd, acct, err)
	}
	if s.Ready() {
		t.Fatal("required cgroup failure must degrade readiness")
	}
}

// auto is an explicit availability tradeoff: it may retain the old rlimit-only
// fallback, and a transient begin failure must not mark the instance unhealthy.
func TestNsjailCommand_AutoCgroupMayFallback(t *testing.T) {
	s := &nsjailSandbox{
		bin: "/bin/true",
		cg:  &cgroupManager{parent: filepath.Join(t.TempDir(), "missing")},
	}
	cmd, acct, err := s.Command(context.Background(), Spec{
		Argv: []string{"/bin/true"}, WorkDir: t.TempDir(), MemoryMB: 64, MaxProcesses: 8,
	})
	if err != nil || cmd == nil || acct != nil {
		t.Fatalf("auto cgroup failure should fall back to a command without accounting, got (%v,%v,%v)", cmd, acct, err)
	}
	if !s.Ready() {
		t.Fatal("auto fallback must not trip required-containment readiness")
	}
}

// probeNsjailAvailable reports whether a working nsjail exists here, so the
// fail-closed/fallback tests can skip cleanly on a host that has one.
func probeNsjailAvailable() (*nsjailSandbox, error) {
	s, _, err := tryNsjail()
	return s, err
}

func TestLimitProcesses_LowersSoftLimit(t *testing.T) {
	var orig syscall.Rlimit
	if err := syscall.Getrlimit(rlimitNPROC, &orig); err != nil {
		t.Skipf("RLIMIT_NPROC unavailable: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Setrlimit(rlimitNPROC, &orig) })

	const target = 4096 // well above the test process's own usage
	if orig.Max != 0 && target > orig.Max {
		t.Skip("hard limit below target")
	}
	if err := LimitProcesses(target); err != nil {
		t.Fatalf("LimitProcesses: %v", err)
	}
	var got syscall.Rlimit
	if err := syscall.Getrlimit(rlimitNPROC, &got); err != nil {
		t.Fatal(err)
	}
	if got.Cur != target {
		t.Fatalf("NPROC soft limit = %d, want %d", got.Cur, target)
	}
}
