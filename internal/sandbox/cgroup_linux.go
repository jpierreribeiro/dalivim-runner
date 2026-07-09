//go:build linux

package sandbox

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// cgroup2SuperMagic identifies a cgroup v2 filesystem (statfs f_type). Guards
// against a RUNNER_CGROUP_MOUNT that points at an ordinary writable directory,
// where creating "memory.max" would otherwise succeed as a regular file and give
// a false-positive probe.
const cgroup2SuperMagic = 0x63677270

// ---------------------------------------------------------------------------
// F-E / R6 — authoritative per-run memory accounting via cgroup v2.
//
// The nsjail backend, when a WRITABLE cgroup v2 subtree has been delegated to
// the runner's uid, gives every run its own leaf cgroup with `memory.max` and
// `pids.max`, and clones nsjail straight into it (clone3 CLONE_INTO_CGROUP via
// exec.Cmd's UseCgroupFD). Two things this buys that rlimits cannot:
//
//   - a hard RSS bound for runtimes RLIMIT_AS breaks (V8/Node reserve a multi-GB
//     virtual cage that a tight RLIMIT_AS refuses; cgroup memory.max counts real
//     pages, not virtual reservation);
//   - deterministic memory_exceeded — read the kernel's own OOM counter
//     (memory.events oom_kill) after the run instead of grepping stderr for
//     "MemoryError"/"heap out of memory" (the fragile R6 heuristic).
//
// The runner OWNS each run's cgroup end-to-end (create → set limits → run into →
// read memory.events → remove), rather than delegating to nsjail's own --cgroup
// flags, precisely so the OOM counter is readable AFTER the run: nsjail removes
// the cgroup it creates on exit, which would make the event racy to observe.
//
// Everything here is fail-safe. Absent a delegated cgroup (Railway, CI, local),
// the manager is nil and runs fall back to the RLIMIT_AS / heap-flag bound
// exactly as before — a missing cgroup can never fail a run.
// ---------------------------------------------------------------------------

// cgroupManager is the delegated, writable cgroup v2 parent under which each run
// gets a leaf cgroup. Resolved once at Configure (probeCgroup) and read-only
// thereafter, so it is safe for concurrent use.
type cgroupManager struct {
	parent string // e.g. /sys/fs/cgroup/dalivim — writable, memory+pids delegated
}

// probeCgroup verifies a cgroup v2 subtree is genuinely usable for per-run
// accounting: it must be a directory into which the runner can create a child
// cgroup AND write memory.max + pids.max (i.e. the parent's subtree_control
// already delegates the memory and pids controllers). It proves this the only
// honest way — by doing it once against a throwaway child and cleaning up — so a
// half-delegated mount fails the boot probe rather than the first real run.
// Returns (manager, "", true) on success or (nil, detail, false) with a
// human-readable reason for the boot log / fail-closed error.
func probeCgroup(mount string) (*cgroupManager, string, bool) {
	if strings.TrimSpace(mount) == "" {
		return nil, "no cgroup mount configured (RUNNER_CGROUP_MOUNT empty)", false
	}
	fi, err := os.Stat(mount)
	if err != nil {
		return nil, fmt.Sprintf("cgroup mount %q not present: %v", mount, err), false
	}
	if !fi.IsDir() {
		return nil, fmt.Sprintf("cgroup mount %q is not a directory", mount), false
	}
	if !isCgroup2(mount) {
		return nil, fmt.Sprintf("cgroup mount %q is not a cgroup v2 filesystem", mount), false
	}
	m := &cgroupManager{parent: mount}
	rc, err := m.begin(64, 16) // throwaway limits; proves controllers are delegated
	if err != nil {
		return nil, fmt.Sprintf("cannot create a delegated child cgroup under %q (is +memory +pids in its cgroup.subtree_control, owned by this uid?): %v", mount, err), false
	}
	defer rc.Close()

	// Prove the mechanism real runs depend on end to end: clone a throwaway
	// process straight into the leaf cgroup (CLONE_INTO_CGROUP via UseCgroupFD).
	// mkdir+write (above) proves we OWN the leaf, but the clone additionally needs
	// write access to the common ancestor of the runner's own cgroup and the
	// target — so it fails EPERM when the runner's cgroup sits OUTSIDE the
	// delegated subtree (its common ancestor is then the root, owned by uid 0),
	// even though --cgroupns=host is set and the leaf is chowned. Catching it here
	// makes RUNNER_CGROUP=require fail closed at boot with an actionable reason
	// instead of breaking the first real run.
	probe := exec.Command("/bin/true")
	probe.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: rc.fd()}
	if err := probe.Run(); err != nil {
		return nil, fmt.Sprintf("could not launch a process into the delegated cgroup under %q: %v — CLONE_INTO_CGROUP needs write access to the common ancestor of the runner's own cgroup (%s) and the target, so the runner's cgroup must live INSIDE the delegated subtree. Start the container within it (Docker: --cgroup-parent=%s, which needs the cgroupfs driver — the systemd driver wants a delegated .slice instead) so they share a writable ancestor; a chown of the leaf alone is not enough. (--cgroupns=host is also required.)",
			mount, err, selfCgroup(), filepath.Base(mount)), false
	}
	return m, "", true
}

// selfCgroup returns the runner's own cgroup v2 path (the "0::<path>" line of
// /proc/self/cgroup), or "unknown" — used to explain a CLONE_INTO_CGROUP EPERM.
func selfCgroup() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return path
		}
	}
	return "unknown"
}

// begin creates one run's leaf cgroup, applies its limits, and opens the
// directory fd used for CLONE_INTO_CGROUP. memMB<=0 leaves memory unbounded;
// pids<=0 leaves the pid count unbounded (nsjail's --rlimit_nproc still applies).
func (m *cgroupManager) begin(memMB, pids int) (*runCgroup, error) {
	dir, err := os.MkdirTemp(m.parent, "run-")
	if err != nil {
		return nil, err
	}
	rc := &runCgroup{dir: dir}

	if memMB > 0 {
		if err := writeCgroupFile(dir, "memory.max", strconv.Itoa(memMB*1024*1024)); err != nil {
			rc.Close()
			return nil, fmt.Errorf("memory.max: %w", err)
		}
		// Deny swap so memory.max is a hard RSS ceiling and cannot be escaped into
		// swap. Best-effort: some hosts lack the swap controller.
		_ = writeCgroupFile(dir, "memory.swap.max", "0")
	}
	if pids > 0 {
		if err := writeCgroupFile(dir, "pids.max", strconv.Itoa(pids)); err != nil {
			rc.Close()
			return nil, fmt.Errorf("pids.max: %w", err)
		}
	}

	// O_DIRECTORY fd of the leaf cgroup, handed to exec.Cmd.CgroupFD so the child
	// (nsjail, and every process it forks) is born inside this cgroup.
	f, err := os.OpenFile(dir, os.O_RDONLY, 0)
	if err != nil {
		rc.Close()
		return nil, err
	}
	rc.f = f
	return rc, nil
}

// runCgroup is one run's leaf cgroup; it implements RunAccounting.
type runCgroup struct {
	dir string
	f   *os.File
}

// fd is the cgroup directory descriptor for exec.Cmd.CgroupFD (-1 when unopened).
func (rc *runCgroup) fd() int {
	if rc.f == nil {
		return -1
	}
	return int(rc.f.Fd())
}

// OOMKilled reports whether the kernel OOM-killed anything in this run's cgroup,
// read from memory.events (oom_kill counts per-process kills; oom_group_kill
// counts whole-cgroup kills). Either being non-zero is authoritative memory
// exhaustion — no stderr guessing.
func (rc *runCgroup) OOMKilled() bool {
	f, err := os.Open(filepath.Join(rc.dir, "memory.events"))
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok {
			continue
		}
		if key == "oom_kill" || key == "oom_group_kill" {
			if n, err := strconv.Atoi(val); err == nil && n > 0 {
				return true
			}
		}
	}
	return false
}

// PeakMemoryKB returns the cgroup's high-water memory (memory.peak) in KiB, or 0
// when the file is absent (kernels < 5.19) or unreadable.
func (rc *runCgroup) PeakMemoryKB() int {
	b, err := os.ReadFile(filepath.Join(rc.dir, "memory.peak"))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < 0 {
		return 0
	}
	return n / 1024
}

// Close releases the leaf cgroup: it closes the fd and removes the directory.
// rmdir can transiently fail with EBUSY between the last process exiting and the
// kernel reaping the cgroup, so it retries briefly. Safe to call once.
func (rc *runCgroup) Close() {
	if rc.f != nil {
		_ = rc.f.Close()
		rc.f = nil
	}
	if rc.dir == "" {
		return
	}
	for i := 0; i < 20; i++ {
		if err := os.Remove(rc.dir); err == nil || os.IsNotExist(err) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	rc.dir = ""
}

// writeCgroupFile writes a single control value to a cgroup file. The file
// already exists (the controller created it); this truncates and writes.
func writeCgroupFile(dir, name, value string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644)
}

// isCgroup2 reports whether path lives on a cgroup v2 filesystem.
func isCgroup2(path string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false
	}
	return int64(st.Type) == cgroup2SuperMagic
}
