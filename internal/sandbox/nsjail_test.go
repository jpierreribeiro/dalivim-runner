//go:build linux

package sandbox

import (
	"strings"
	"testing"
)

// argValue returns the token following the first occurrence of flag, or "" when
// the flag is absent (so tests can assert both presence and value).
func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func hasArg(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func sampleSpec() Spec {
	return Spec{
		Argv:           []string{"python3", "-I", "main.py"},
		WorkDir:        "/tmp/dalivim-run-abc",
		TimeoutMs:      3000,
		AddressSpaceMB: 128,
		MaxProcesses:   256,
		MaxFileSizeMB:  64,
	}
}

// TestNsjailArgs_AppliesEveryLayer pins that each containment control the plan
// requires is present with the right value. This is the CI-observable contract
// for a jail that cannot itself be exercised without the binary + userns.
func TestNsjailArgs_AppliesEveryLayer(t *testing.T) {
	args := nsjailArgs(1000, 1000, sampleSpec())

	// Read-only rootfs, per-run tmpfs /tmp, source mounted read-only at /sandbox.
	if argValue(args, "--bindmount_ro") != "/" { // first ro mount is the whole rootfs
		t.Fatalf("expected a read-only rootfs bind mount, got %q", argValue(args, "--bindmount_ro"))
	}
	if argValue(args, "--tmpfsmount") != "/tmp" {
		t.Fatalf("expected tmpfs /tmp, got %q", argValue(args, "--tmpfsmount"))
	}
	if !hasArg(args, "/tmp/dalivim-run-abc:/sandbox") {
		t.Fatalf("expected the workdir bound read-only to /sandbox, args=%v", args)
	}
	if argValue(args, "--cwd") != "/sandbox" {
		t.Fatalf("expected cwd /sandbox, got %q", argValue(args, "--cwd"))
	}

	// Per-run rlimits derived from the spec.
	if argValue(args, "--rlimit_as") != "128" {
		t.Fatalf("expected RLIMIT_AS 128MB, got %q", argValue(args, "--rlimit_as"))
	}
	if argValue(args, "--rlimit_cpu") != "4" { // (3000+999)/1000 + 1
		t.Fatalf("expected RLIMIT_CPU 4s, got %q", argValue(args, "--rlimit_cpu"))
	}
	if argValue(args, "--rlimit_nproc") != "256" {
		t.Fatalf("expected RLIMIT_NPROC 256, got %q", argValue(args, "--rlimit_nproc"))
	}
	if argValue(args, "--rlimit_fsize") != "64" {
		t.Fatalf("expected RLIMIT_FSIZE 64MB, got %q", argValue(args, "--rlimit_fsize"))
	}
	if argValue(args, "--time_limit") != "3" { // ceil(3000/1000)
		t.Fatalf("expected wall time_limit 3s, got %q", argValue(args, "--time_limit"))
	}

	// Seccomp denylist + uid/gid single-id mapping (rootless, newuidmap-free).
	if !strings.Contains(argValue(args, "--seccomp_string"), "KILL") {
		t.Fatal("expected a seccomp kafel policy with a KILL block")
	}
	// --user/--group (not --uid_mapping/--gid_mapping): direct /proc uid_map write,
	// no setuid newuidmap/newgidmap helper (see nsjailArgs for why).
	if argValue(args, "--user") != "0:1000:1" || argValue(args, "--group") != "0:1000:1" {
		t.Fatalf("expected single-id root mapping via --user/--group, got user=%q group=%q",
			argValue(args, "--user"), argValue(args, "--group"))
	}
	if !hasArg(args, "--iface_no_lo") || !hasArg(args, "--disable_proc") || !hasArg(args, "--keep_env") {
		t.Fatalf("expected --iface_no_lo, --disable_proc and --keep_env, args=%v", args)
	}
}

// TestNsjailArgs_ArgvIsAfterSeparator ensures the student argv is placed after
// the `--` guard so no token can be reinterpreted as an nsjail flag.
func TestNsjailArgs_ArgvIsAfterSeparator(t *testing.T) {
	args := nsjailArgs(1000, 1000, sampleSpec())
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep == -1 {
		t.Fatal("expected a -- separator before the argv")
	}
	got := strings.Join(args[sep+1:], " ")
	if got != "python3 -I main.py" {
		t.Fatalf("argv after separator = %q, want %q", got, "python3 -I main.py")
	}
}

// TestNsjailArgs_OptionalLimitsOmitted confirms a zero nproc/fsize/address-space
// leaves the flag off (nsjail default), rather than emitting a "0" cap.
func TestNsjailArgs_OptionalLimitsOmitted(t *testing.T) {
	spec := sampleSpec()
	spec.MaxProcesses = 0
	spec.MaxFileSizeMB = 0
	args := nsjailArgs(1000, 1000, spec)
	if hasArg(args, "--rlimit_nproc") {
		t.Fatal("MaxProcesses=0 must omit --rlimit_nproc")
	}
	if hasArg(args, "--rlimit_fsize") {
		t.Fatal("MaxFileSizeMB=0 must omit --rlimit_fsize")
	}
}

// TestNsjailArgs_ZeroAddressSpaceOmitsRlimitAS pins the Node path: a run that
// cannot take a virtual-address cap (V8's multi-GB cage) passes AddressSpaceMB=0
// and must get NO --rlimit_as flag, not "--rlimit_as 0".
func TestNsjailArgs_ZeroAddressSpaceOmitsRlimitAS(t *testing.T) {
	spec := sampleSpec()
	spec.AddressSpaceMB = 0
	args := nsjailArgs(1000, 1000, spec)
	if hasArg(args, "--rlimit_as") {
		t.Fatal("AddressSpaceMB=0 must omit --rlimit_as (V8/Node cannot start under a tight cap)")
	}
}

// flagBefore returns the token immediately preceding the first occurrence of
// value, or "" if value is absent or first.
func flagBefore(args []string, value string) string {
	for i, a := range args {
		if a == value && i > 0 {
			return args[i-1]
		}
	}
	return ""
}

// TestNsjailArgs_WorkDirMountMode pins the compile-vs-run distinction: the run
// jail binds the workdir READ-ONLY, while the compile jail (WritableWorkDir)
// binds it read-write so the compiler can emit its artifact. The rootfs bind
// stays read-only in both cases.
func TestNsjailArgs_WorkDirMountMode(t *testing.T) {
	const mount = "/tmp/dalivim-run-abc:/sandbox"

	ro := nsjailArgs(1000, 1000, sampleSpec())
	if got := flagBefore(ro, mount); got != "--bindmount_ro" {
		t.Fatalf("read-only run jail: workdir mount flag = %q, want --bindmount_ro", got)
	}

	spec := sampleSpec()
	spec.WritableWorkDir = true
	rw := nsjailArgs(1000, 1000, spec)
	if got := flagBefore(rw, mount); got != "--bindmount" {
		t.Fatalf("writable compile jail: workdir mount flag = %q, want --bindmount", got)
	}
	// The rootfs must remain read-only even when the workdir is writable.
	if flagBefore(rw, "/") != "--bindmount_ro" {
		t.Fatal("rootfs must stay --bindmount_ro even with a writable workdir")
	}
}

func TestCapSeconds(t *testing.T) {
	if got := cpuCapSeconds(3000); got != 4 {
		t.Fatalf("cpuCapSeconds(3000) = %d, want 4", got)
	}
	if got := cpuCapSeconds(1); got != 2 {
		t.Fatalf("cpuCapSeconds(1) = %d, want 2", got)
	}
	if got := wallCapSeconds(3000); got != 3 {
		t.Fatalf("wallCapSeconds(3000) = %d, want 3", got)
	}
	if got := wallCapSeconds(1); got != 1 {
		t.Fatalf("wallCapSeconds(1) = %d, want 1 (min)", got)
	}
	if got := wallCapSeconds(0); got != 1 {
		t.Fatalf("wallCapSeconds(0) = %d, want 1 (min)", got)
	}
}

func TestShJoin_QuotesEveryToken(t *testing.T) {
	if got := shJoin([]string{"python3", "-I", "main.py"}); got != `'python3' '-I' 'main.py'` {
		t.Fatalf("shJoin = %q", got)
	}
	// A single quote in a token must not break out of the quoting.
	if got := shJoin([]string{"a'b"}); got != `'a'\''b'` {
		t.Fatalf("shJoin single-quote escaping = %q", got)
	}
}
