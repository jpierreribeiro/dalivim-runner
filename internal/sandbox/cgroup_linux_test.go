//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveCgroup_Dial pins the RUNNER_CGROUP state machine without a real
// delegated cgroup (none exists in `go test`): off disables, an unknown value is
// rejected, require fails closed on an unusable mount, and auto degrades safely.
func TestResolveCgroup_Dial(t *testing.T) {
	if cg, err := resolveCgroup("off", "/sys/fs/cgroup/whatever"); err != nil || cg != nil {
		t.Fatalf("off => (nil,nil), got (%v,%v)", cg, err)
	}
	if _, err := resolveCgroup("banana", ""); err == nil {
		t.Fatal("unknown RUNNER_CGROUP value must be rejected")
	}
	if _, err := resolveCgroup("require", "/definitely/not/a/cgroup/xyz"); err == nil {
		t.Fatal("require must fail closed when the mount is unusable")
	}
	if _, err := resolveCgroup("require", ""); err == nil {
		t.Fatal("require must fail closed when no mount is configured")
	}
	if cg, err := resolveCgroup("auto", "/definitely/not/a/cgroup/xyz"); err != nil || cg != nil {
		t.Fatalf("auto must fall back to (nil,nil) on an unusable mount, got (%v,%v)", cg, err)
	}
}

func TestCgroupLabel(t *testing.T) {
	if got := cgroupLabel(nil); got != "rlimit-only" {
		t.Fatalf("cgroupLabel(nil) = %q, want rlimit-only", got)
	}
	if got := cgroupLabel(&cgroupManager{parent: "/sys/fs/cgroup/dalivim"}); got != "cgroup-v2:/sys/fs/cgroup/dalivim" {
		t.Fatalf("cgroupLabel = %q", got)
	}
}

// TestSelfCgroup reports the runner's own cgroup path (used to explain a
// CLONE_INTO_CGROUP EPERM); on any cgroup v2 host it is an absolute path, and it
// degrades to "unknown" rather than erroring.
func TestSelfCgroup(t *testing.T) {
	got := selfCgroup()
	if got == "" {
		t.Fatal("selfCgroup() must never be empty")
	}
	if got != "unknown" && !strings.HasPrefix(got, "/") {
		t.Fatalf("selfCgroup() = %q, want an absolute path or \"unknown\"", got)
	}
}

// TestProbeCgroup_RejectsNonCgroupDir proves the statfs guard: an ordinary
// writable directory must NOT pass as a cgroup (otherwise writing memory.max
// would create a regular file and give a false positive).
func TestProbeCgroup_RejectsNonCgroupDir(t *testing.T) {
	if _, _, ok := probeCgroup(t.TempDir()); ok {
		t.Fatal("an ordinary directory must not be accepted as a cgroup v2 mount")
	}
	if _, _, ok := probeCgroup(""); ok {
		t.Fatal("an empty mount must not be accepted")
	}
}

// TestRunCgroup_OOMKilled parses memory.events fixtures the way the kernel writes
// them; oom_kill or oom_group_kill > 0 is the authoritative memory_exceeded.
func TestRunCgroup_OOMKilled(t *testing.T) {
	cases := map[string]bool{
		"low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\n": false,
		"low 0\nhigh 0\nmax 2\noom 1\noom_kill 1\noom_group_kill 0\n": true,
		"oom_kill 0\noom_group_kill 4\n":                              true,
		"max 5\n":                                                     false, // hitting max without a kill is not OOM
	}
	for events, want := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "memory.events"), []byte(events), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := (&runCgroup{dir: dir}).OOMKilled(); got != want {
			t.Fatalf("OOMKilled(%q) = %v, want %v", events, got, want)
		}
	}
	// A missing memory.events (cgroup already reaped) reads as not-killed.
	if (&runCgroup{dir: t.TempDir()}).OOMKilled() {
		t.Fatal("missing memory.events must read as not OOM-killed")
	}
}

func TestRunCgroup_PeakMemoryKB(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.peak"), []byte("2097152\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := (&runCgroup{dir: dir}).PeakMemoryKB(); got != 2048 {
		t.Fatalf("PeakMemoryKB = %d, want 2048", got)
	}
	if got := (&runCgroup{dir: t.TempDir()}).PeakMemoryKB(); got != 0 {
		t.Fatalf("missing memory.peak => 0, got %d", got)
	}
}
