package executor

import (
	"strconv"
	"strings"
)

// languageSpec describes one INTERPRETED language: everything the generic
// interpretedRuntime needs to run a single source file inside the sandbox. It is
// the extension point invariant the plan calls for — a new interpreted language
// is a new entry in the closed registry below plus a constructor, nothing more;
// no syscall or namespace code lives here (that is the sandbox's job).
//
// The registry is CLOSED BY DESIGN: adding a language is a deliberate edit to
// this file and to the wire contract's language catalog, never driven by request
// input. Compiled languages (F-D) introduce their own spec/runtime with a compile
// phase and do not share this type.
type languageSpec struct {
	// name is the wire identifier callers send (contract.Language), e.g. "python".
	name string

	// sourceFile is the basename the submitted source is written to in the per-run
	// workdir, e.g. "main.py". The extension is what lets the interpreter treat the
	// file as its own language.
	sourceFile string

	// binNames are interpreter lookup candidates; the first found on PATH wins and
	// is resolved to an ABSOLUTE path once at startup (the nsjail backend execve's
	// argv[0] directly, with no PATH search, so a bare name fails inside the jail).
	binNames []string

	// runArgs are the interpreter flags plus the source filename, appended after
	// the resolved interpreter path to form the jail argv. The source is referenced
	// here only as a FILE, never inlined — so there is no shell/argv injection.
	runArgs []string

	// env is the minimal environment handed to the child; nothing from the host
	// leaks in beyond what is listed.
	env []string

	// versionArgs prints the interpreter version (e.g. ["--version"]); parseVersion
	// turns its output into a bare semantic version like "3.12.3".
	versionArgs  []string
	parseVersion func(output string) string

	// memErrSubstr, when non-empty, classifies a FAILED run as memory_exceeded if
	// the substring appears in stderr. This is the fragile per-language heuristic
	// the plan (R6) replaces with a real memory signal in F-E; it is kept so
	// today's Python behaviour is preserved, and languages whose OOM has no stable
	// text (Node) opt out with "" rather than guessing.
	memErrSubstr string

	// capAddressSpace decides how the run's memory budget is enforced. CPython
	// tolerates a hard RLIMIT_AS (virtual address space) cap, so Python sets true
	// and a memory bomb dies deterministically as memory_exceeded. V8 (Node)
	// reserves a multi-GB virtual cage at startup that a tight RLIMIT_AS refuses,
	// so Node sets false and bounds its heap via memoryArgs instead, relying on the
	// container/cgroup memory limit for the RSS backstop (plan §2.3: Railway uses
	// container limits, VPS uses cgroups; per-jail cgroup memory.max is the F-E
	// upgrade).
	capAddressSpace bool

	// memoryArgs returns per-run interpreter flags derived from the memory budget
	// (MB), inserted before runArgs. nil when the language needs none.
	memoryArgs func(memoryMB int) []string

	// multiFileRunArgs builds the interpreter argv tail (flags + entrypoint
	// reference) for a MULTI-FILE run, given the entrypoint's absolute in-jail path
	// (/sandbox/src/<entry>). It replaces runArgs when the request carries files[].
	// The single-file path (runArgs) is left untouched so its behaviour is
	// byte-for-byte unchanged; multi-file is a separate, additive shape.
	multiFileRunArgs func(entryJailPath string) []string

	// minTimeoutMs/minMemoryMB are optional per-language floors on the clamped
	// request limits (G6), raised in the service layer and never above the global
	// ceilings. 0 = no floor. No interpreted language needs one today; the seam
	// mirrors compiledLangSpec, where the JVM's baseline overhead does.
	minTimeoutMs int
	minMemoryMB  int
}

// pythonSpec runs CPython in isolated mode. Behaviour-identical to the original
// dedicated Python runtime — only the shape (data, not code) changed.
var pythonSpec = languageSpec{
	name:       "python",
	sourceFile: "main.py",
	binNames:   []string{"python3"},
	// -I: isolated mode — ignore env vars and the user site, and keep the cwd off
	// sys.path, so a submission cannot import planted modules.
	runArgs:      []string{"-I", "main.py"},
	env:          []string{"PATH=/usr/local/bin:/usr/bin:/bin", "PYTHONUNBUFFERED=1"},
	versionArgs:  []string{"--version"}, // "Python 3.12.3"
	parseVersion: secondField,           // -> "3.12.3"
	memErrSubstr: "MemoryError",
	// CPython runs fine under a hard RLIMIT_AS; keep the deterministic virtual cap.
	capAddressSpace: true,
	// Multi-file: a controlled runpy wrapper. `-I main.py` is isolated mode, which
	// removes the SCRIPT'S directory from sys.path — so a sibling `import helper`
	// would fail. Instead of relaxing isolation (which would re-open the user-site
	// and env-var import surface `-I` closes), keep `-I`/`-B` and inject exactly one
	// hardcoded in-jail path (/sandbox/src) with runpy. The inserted path is fixed
	// here, never caller-controlled, so a submission cannot point imports elsewhere.
	multiFileRunArgs: pythonRunpyArgs,
}

// javascriptSpec runs a single Node file. It inherits the exact same jail as
// Python; the only differences are the interpreter argv, the source filename, and
// version parsing.
var javascriptSpec = languageSpec{
	name:       "javascript",
	sourceFile: "main.js",
	binNames:   []string{"node", "nodejs"},
	// --disable-proto=throw removes the __proto__ accessor, closing a
	// prototype-pollution escalation path. The file is run directly: single-file
	// execution, no npm and no node_modules lookup.
	runArgs:      []string{"--disable-proto=throw", "main.js"},
	env:          []string{"PATH=/usr/local/bin:/usr/bin:/bin"},
	versionArgs:  []string{"--version"}, // "v20.11.0"
	parseVersion: trimLeadingV,          // -> "20.11.0"
	// No memory substring: Node's OOM ("JavaScript heap out of memory") is a V8
	// abort classified as runtime_error today. Deterministic memory classification
	// for every language is F-E (R6), not a per-language string here.

	// Node CANNOT run under a tight RLIMIT_AS (V8's virtual cage), so skip the
	// address-space cap and bound the V8 old-space heap to the run's budget
	// instead; the container/cgroup memory limit is the RSS backstop.
	capAddressSpace: false,
	memoryArgs:      nodeHeapArgs,
	// Multi-file: run the entrypoint by absolute in-jail path. `--` ends option
	// parsing so a filename can never be read as a Node flag; sibling require()/import
	// of the other materialized files resolves relative to the entrypoint on disk.
	multiFileRunArgs: nodeRunArgs,
}

// pythonRunpyArgs builds the CPython multi-file argv tail: isolated + no-bytecode
// mode, then a runpy wrapper that runs the entrypoint as __main__ with exactly
// the source root on sys.path. entryRel is the entrypoint relative to cwd
// ("src/main.py"); the sys.path entry is the hardcoded source-root name, never
// the caller's path, so imports are confined to the submitted tree.
func pythonRunpyArgs(entryRel string) []string {
	wrapper := "import runpy, sys; sys.path.insert(0, " + strconv.Quote(srcRootName) +
		"); runpy.run_path(" + strconv.Quote(entryRel) + ", run_name=\"__main__\")"
	return []string{"-I", "-B", "-c", wrapper}
}

// nodeRunArgs builds the Node multi-file argv tail: the __proto__ hardening flag,
// `--` to stop flag parsing, then the entrypoint path (relative to cwd). sibling
// require()/import resolves relative to the entrypoint on disk.
func nodeRunArgs(entryRel string) []string {
	return []string{"--disable-proto=throw", "--", entryRel}
}

// nodeHeapArgs bounds V8's old-space heap to the run's memory budget. This is
// Node's memory control in place of RLIMIT_AS: a heap bomb hits this cap and V8
// aborts (contained) instead of the run failing to start under a virtual cap.
func nodeHeapArgs(memoryMB int) []string {
	return []string{"--max-old-space-size=" + strconv.Itoa(memoryMB)}
}

// secondField returns the second whitespace-separated token, e.g.
// "Python 3.12.3" -> "3.12.3". Empty when the output has fewer than two fields.
func secondField(out string) string {
	f := strings.Fields(out)
	if len(f) >= 2 {
		return f[1]
	}
	return ""
}

// trimLeadingV strips node's leading "v", e.g. "v20.11.0\n" -> "20.11.0".
func trimLeadingV(out string) string {
	return strings.TrimPrefix(strings.TrimSpace(out), "v")
}
