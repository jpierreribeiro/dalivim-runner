package sandbox

import (
	"syscall"
	"testing"
)

// TestSysProcAttr_SetpgidAlways ensures the process-group setup used for timeout
// kills is present whether or not network isolation is enabled.
func TestSysProcAttr_SetpgidAlways(t *testing.T) {
	off := &Sandbox{netns: false}
	if attr := off.SysProcAttr(); !attr.Setpgid || attr.Cloneflags != 0 {
		t.Fatalf("isolation off: want Setpgid and no cloneflags, got %+v", attr)
	}

	if !probeNetns() {
		return // cannot assert the enabled path on this platform
	}
	on := &Sandbox{netns: true}
	attr := on.SysProcAttr()
	if !attr.Setpgid {
		t.Fatal("isolation on: Setpgid must still be set so timeouts can kill the group")
	}
	if attr.Cloneflags&syscall.CLONE_NEWNET == 0 || attr.Cloneflags&syscall.CLONE_NEWUSER == 0 {
		t.Fatalf("isolation on: want CLONE_NEWUSER|CLONE_NEWNET, got cloneflags=%#x", attr.Cloneflags)
	}
}

// TestConfigure_RejectsUnknownPolicy pins the closed policy vocabulary.
func TestConfigure_RejectsUnknownPolicy(t *testing.T) {
	if _, err := Configure("banana"); err == nil {
		t.Fatal("expected an error for an unknown isolation policy")
	}
}

// TestConfigure_OffDisablesIsolation confirms the explicit opt-out.
func TestConfigure_OffDisablesIsolation(t *testing.T) {
	sb, err := Configure("off")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sb.NetworkIsolated() {
		t.Fatal("policy=off must disable network isolation")
	}
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
