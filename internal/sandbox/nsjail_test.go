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
// and must get NO --rlimit_as flag, not "--rlimit_as 0". Omitting the flag leaves
// nsjail's OWN 4 GB default standing — which is the point for Node (it fits), and
// is why a runtime that does NOT fit needs UnlimitedAddressSpace below.
func TestNsjailArgs_ZeroAddressSpaceOmitsRlimitAS(t *testing.T) {
	spec := sampleSpec()
	spec.AddressSpaceMB = 0
	args := nsjailArgs(1000, 1000, spec)
	if hasArg(args, "--rlimit_as") {
		t.Fatal("AddressSpaceMB=0 must omit --rlimit_as (V8/Node cannot start under a tight cap)")
	}
}

// TestNsjailArgs_UnlimitedAddressSpace pins the CoreCLR path: omitting our cap is
// not enough, because nsjail then applies its own 4 GB default and the CLR aborts
// at GC heap init (0x8007000E). UnlimitedAddressSpace must lift it EXPLICITLY —
// and an explicit AddressSpaceMB still wins, so the lift can never widen a run
// that asked to be capped.
func TestNsjailArgs_UnlimitedAddressSpace(t *testing.T) {
	spec := sampleSpec()
	spec.AddressSpaceMB = 0
	spec.UnlimitedAddressSpace = true
	if got := argValue(nsjailArgs(1000, 1000, spec), "--rlimit_as"); got != "inf" {
		t.Fatalf("UnlimitedAddressSpace must emit --rlimit_as inf, got %q", got)
	}

	spec.AddressSpaceMB = 128 // an explicit cap outranks the lift
	if got := argValue(nsjailArgs(1000, 1000, spec), "--rlimit_as"); got != "128" {
		t.Fatalf("an explicit AddressSpaceMB must win over the lift, got %q", got)
	}
}

// TestNsjailArgs_MaxOpenFiles pins RLIMIT_NOFILE: nsjail's default of 32 stands
// unless a runtime asks for more (the CoreCLR maps one file per assembly and
// misreports the exhaustion as "Could not load file or assembly").
func TestNsjailArgs_MaxOpenFiles(t *testing.T) {
	spec := sampleSpec()
	spec.MaxOpenFiles = 1024
	if got := argValue(nsjailArgs(1000, 1000, spec), "--rlimit_nofile"); got != "1024" {
		t.Fatalf("MaxOpenFiles=1024 must emit --rlimit_nofile 1024, got %q", got)
	}

	spec.MaxOpenFiles = 0
	if hasArg(nsjailArgs(1000, 1000, spec), "--rlimit_nofile") {
		t.Fatal("MaxOpenFiles=0 must omit --rlimit_nofile (nsjail's tight default is the right one)")
	}
}

// TestNsjailArgs_TmpfsSize pins the Go compile path: TmpfsSizeMB sizes /tmp via
// the arbitrary-mount form (nsjail 3.4 has no --tmpfs_size flag) so a cold
// `go build` fits its GOCACHE; 0 leaves the plain --tmpfsmount at nsjail's default.
func TestNsjailArgs_TmpfsSize(t *testing.T) {
	spec := sampleSpec()
	spec.TmpfsSizeMB = 256
	args := nsjailArgs(1000, 1000, spec)
	if got := argValue(args, "--mount"); got != "none:/tmp:tmpfs:size=268435456" {
		t.Fatalf("TmpfsSizeMB=256 must size /tmp via --mount, got %q", got)
	}
	if hasArg(args, "--tmpfsmount") {
		t.Fatal("a sized /tmp uses --mount, not the default --tmpfsmount")
	}

	spec.TmpfsSizeMB = 0
	zero := nsjailArgs(1000, 1000, spec)
	if !pairAt(zero, "--tmpfsmount", "/tmp") {
		t.Fatal("TmpfsSizeMB=0 must use the default --tmpfsmount /tmp")
	}
	if hasArg(zero, "--mount") {
		t.Fatal("TmpfsSizeMB=0 must not emit a sized --mount")
	}
}

// pairAt reports whether args contains flag immediately followed by value.
func pairAt(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// flagBefore returns the token immediately preceding the first occurrence of
// value, or "" if value is absent / at index 0.
func flagBefore(args []string, value string) string {
	for i, a := range args {
		if a == value && i > 0 {
			return args[i-1]
		}
	}
	return ""
}

// TestNsjailArgs_DefaultInterpretedMounts pins the interpreted/compile-jail shape:
// full host rootfs read-only + the workdir bound READ-ONLY at /sandbox.
func TestNsjailArgs_DefaultInterpretedMounts(t *testing.T) {
	args := nsjailArgs(1000, 1000, sampleSpec()) // Writable=false, MinimalRootfs=false
	if !pairAt(args, "--bindmount_ro", "/") {
		t.Fatalf("expected the full host rootfs bound read-only, args=%v", args)
	}
	if got := flagBefore(args, "/tmp/dalivim-run-abc:/sandbox"); got != "--bindmount_ro" {
		t.Fatalf("expected the workdir bound read-only, flag before it = %q", got)
	}
}

// TestNsjailArgs_CompilePhaseWritable pins the compile jail: full rootfs (the
// toolchain) but a READ-WRITE workdir so the compiler can drop its artifact.
func TestNsjailArgs_CompilePhaseWritable(t *testing.T) {
	spec := sampleSpec()
	spec.Writable = true
	args := nsjailArgs(1000, 1000, spec)
	if !pairAt(args, "--bindmount_ro", "/") {
		t.Fatal("compile jail must still bring the full rootfs (toolchain)")
	}
	if got := flagBefore(args, "/tmp/dalivim-run-abc:/sandbox"); got != "--bindmount" {
		t.Fatalf("Writable spec must bind the workdir read-write, flag before it = %q", got)
	}
}

// TestNsjailArgs_MinimalRootfs pins the run jail for a static artifact: NO host
// rootfs bind (no toolchain/libs), only the read-only workdir + tmpfs /tmp.
func TestNsjailArgs_MinimalRootfs(t *testing.T) {
	spec := sampleSpec()
	spec.MinimalRootfs = true
	args := nsjailArgs(1000, 1000, spec)
	if pairAt(args, "--bindmount_ro", "/") {
		t.Fatal("MinimalRootfs must NOT bind the host rootfs (no toolchain in the run jail)")
	}
	if !pairAt(args, "--tmpfsmount", "/tmp") {
		t.Fatal("MinimalRootfs still needs a tmpfs /tmp")
	}
	if got := flagBefore(args, "/tmp/dalivim-run-abc:/sandbox"); got != "--bindmount_ro" {
		t.Fatalf("run jail must bind the artifact read-only, flag before it = %q", got)
	}
}

// TestSeccompProfiles pins policy selection: the default is the DENYLIST; the
// static profiles are ALLOWLISTS (DEFAULT KILL / LOG) that must NOT permit the
// escape-surface syscalls a compute program never needs.
func TestSeccompProfiles(t *testing.T) {
	deny := seccompPolicyFor(SeccompDenylist)
	if !strings.Contains(deny, "DEFAULT ALLOW") {
		t.Fatalf("denylist must be DEFAULT ALLOW, got %q", deny)
	}
	// S2: the escape-only primitives (io_uring family + userfaultfd) must be in the
	// KILL block. This is a name-level pin; the on-target CI smoke proves they are
	// actually SIGSYS-killed on the shipped nsjail 3.6. clone/clone3 must stay OUT
	// of KILL — killing them breaks glibc thread creation (fork-bomb containment is
	// --rlimit_nproc + the concurrency cap).
	for _, killed := range []string{"io_uring_setup", "io_uring_enter", "io_uring_register", "userfaultfd"} {
		if !containsToken(deny, killed) {
			t.Fatalf("denylist must KILL escape-only syscall %q", killed)
		}
	}
	for _, allowed := range []string{"clone", "clone3"} {
		if containsToken(deny, allowed) {
			t.Fatalf("denylist must NOT kill %q (breaks glibc thread creation)", allowed)
		}
	}

	enforce := seccompPolicyFor(SeccompStaticEnforce)
	if !strings.Contains(enforce, "ALLOW {") || !strings.Contains(enforce, "DEFAULT KILL") {
		t.Fatalf("enforce must be an allowlist with DEFAULT KILL, got %q", enforce)
	}
	// execve is intentionally allowed (nsjail execve's the payload after installing
	// the filter; neutered by the minimal-rootfs run jail — see nsjailArgs). The
	// real escape surface must still be excluded.
	for _, banned := range []string{"execveat", "socket", "openat", "open", "ptrace", "clone", "clone3", "mount", "setns", "unshare"} {
		// word-boundary check so e.g. "open_by_handle_at" wouldn't match "openat"
		if containsToken(enforce, banned) {
			t.Fatalf("static allowlist must NOT permit %q", banned)
		}
	}
	for _, needed := range []string{"read", "write", "exit_group", "brk", "mmap", "rt_sigreturn"} {
		if !containsToken(enforce, needed) {
			t.Fatalf("static allowlist missing essential syscall %q", needed)
		}
	}

	if !strings.Contains(seccompPolicyFor(SeccompStaticComplain), "DEFAULT LOG") {
		t.Fatal("complain profile must be DEFAULT LOG (log, don't kill)")
	}
}

// containsToken reports whether s contains name as a whole comma/space/brace-
// delimited token, not as a substring of another identifier.
func containsToken(s, name string) bool {
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '{' || r == '}'
	}) {
		if f == name {
			return true
		}
	}
	return false
}

// TestNsjailArgs_SeccompSelection pins that the chosen profile reaches nsjail.
func TestNsjailArgs_SeccompSelection(t *testing.T) {
	spec := sampleSpec()
	spec.Seccomp = SeccompStaticEnforce
	if got := argValue(nsjailArgs(1000, 1000, spec), "--seccomp_string"); !strings.Contains(got, "DEFAULT KILL") {
		t.Fatalf("SeccompStaticEnforce must install the allowlist, got %q", got)
	}
	if got := argValue(nsjailArgs(1000, 1000, sampleSpec()), "--seccomp_string"); !strings.Contains(got, "DEFAULT ALLOW") {
		t.Fatalf("default spec must keep the denylist, got %q", got)
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
