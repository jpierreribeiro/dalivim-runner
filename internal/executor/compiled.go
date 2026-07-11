package executor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jpierreribeiro/dalivim-runner/internal/sandbox"
	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// compiledLangSpec describes one COMPILED language (C, C++): a compile command
// and a run command, each a token vector with two placeholders — {src} (the
// source path inside the jail) and {out} (the artifact path inside the jail).
// Like the interpreted registry it is CLOSED BY DESIGN: a new compiled language
// is a deliberate entry here plus a constructor, never request-driven.
type compiledLangSpec struct {
	name       string   // wire identifier, e.g. "c"
	sourceFile string   // basename the source is written to, e.g. "main.c"
	compile    []string // {out}/{src}-templated compiler argv; runs in the compile jail
	link       []string // link libraries appended AFTER {src} (e.g. "-lm"); order matters
	run        []string // {out}-templated run argv; runs in the stricter run jail
	binNames   []string // compiler lookup candidates (for version provenance)

	// compileEnv is extra environment for the COMPILE jail, appended to the base
	// PATH+TMPDIR (e.g. Go's isolated GOCACHE/GOPATH under the size-capped /tmp).
	compileEnv []string
	// runEnv is the environment for the RUN jail: the shared determinism pin
	// (G7: LANG/LC_ALL/TZ — see determinismEnv) plus per-language extras (C/C++
	// disable glibc rseq for the seccomp allowlist; Go pins GOMAXPROCS).
	runEnv []string
	// capAddressSpace applies a hard RLIMIT_AS (== MemoryMB) to the run jail. True
	// for C/C++ (a static glibc binary tolerates it). FALSE for Go: the Go runtime
	// reserves a huge virtual arena at startup and dies under any RLIMIT_AS ("failed
	// to reserve page summary memory"), exactly like V8 — so Go is bounded by the
	// cgroup memory.max only, despite being a static binary.
	capAddressSpace bool
	// staticAllowlistOK is whether this language MAY run under the tight static
	// seccomp allowlist. True for C/C++. FALSE for Go: its scheduler needs clone and
	// a wider syscall set than the C allowlist names, so Go stays on the denylist
	// (still strong: ro-rootfs + empty netns + no_new_privs + cgroup).
	staticAllowlistOK bool

	// versionArgs/parseVersion detect the compiler version for provenance (gcc
	// -dumpfullversion vs `go version`); parseVersion turns the raw output into the
	// reported runtime_version.
	versionArgs  []string
	parseVersion func(string) string

	// compileTmpfsMB sizes the compile jail's /tmp when the toolchain needs more
	// scratch than nsjail's default (Go's GOCACHE). 0 keeps the default.
	compileTmpfsMB int

	// compileArgv0Absolute means compile[0] is already an absolute path to execve
	// (e.g. /bin/sh for Go's cache-seeding prelude), so the compile builder must NOT
	// replace it with the resolved compiler binary.
	compileArgv0Absolute bool

	// artifact is the basename the compile phase must produce, validated to exist
	// (and size-capped) before the run jail launches. "" defaults to "bin" (the
	// C/C++/Go static binary); Java's is "Main.class".
	artifact string

	// runBin, when set, is resolved to an absolute path and used as the run argv[0]
	// (the VM launcher — Java: ["java"]). Empty for static languages, whose run
	// argv[0] IS the artifact path ({out}), already absolute.
	runBin []string

	// runFullRootfs keeps the read-only host rootfs in the RUN jail instead of the
	// minimal one. FALSE for a static artifact (C/C++/Go: minimal rootfs, no
	// toolchain — D-4). TRUE for a VM language (Java): the JVM is dynamically linked
	// and needs its runtime libraries, so it runs on the same full-rootfs posture as
	// the interpreted languages (contained by ro-rootfs + empty netns + cgroup +
	// denylist), not the tighter static jail.
	runFullRootfs bool

	// memErrSubstr is an OOM stderr marker for the no-cgroup fallback classification
	// (Java: "OutOfMemoryError"), mirroring the interpreted path. Empty relies on the
	// cgroup OOM event alone.
	memErrSubstr string

	// multiFile, when non-nil, provides this language's MULTI-FILE (files[]) build
	// and run construction (G3). It is wired in compiled_multifile.go's init from
	// the base spec so the single-file flow (the {src}/{out} templates above) stays
	// untouched. nil means the language is single-file only.
	multiFile *multiFileBuild

	// multiFileCompileEnv is extra compile-jail environment applied only to a
	// multi-file build (Go's offline module policy: GOPROXY=off, …). It never
	// touches the single-file path, so legacy behaviour is unchanged.
	multiFileCompileEnv []string

	// minTimeoutMs/minMemoryMB are optional per-language floors on the clamped
	// request limits (G6), raised in the service layer and never above the global
	// ceilings. 0 = no floor. Java sets a memory floor: its fixed non-heap overhead
	// means a small budget fails for EVERY program, not just greedy ones.
	minTimeoutMs int
	minMemoryMB  int
}

var cSpec = compiledLangSpec{
	name:       "c",
	sourceFile: "main.c",
	// -static: no dynamic loader at runtime, so the run jail can be a minimal
	// rootfs with no libc present — the artifact needs nothing but itself.
	compile: []string{"gcc", "-O2", "-static", "-o", "{out}", "{src}"},
	// -lm links libm so ordinary C using <math.h> (sqrt/pow/sin…) resolves instead
	// of failing as a bogus compile_error. It MUST come after {src}: gcc resolves
	// libraries left-to-right, so a lib before the object that needs it is dropped.
	// libm only — no networking/dynamic-loading libs (would break the static run
	// jail and its seccomp allowlist).
	link:     []string{"-lm"},
	run:      []string{"{out}"},
	binNames: []string{"gcc", "cc"},
	// Disable glibc's rseq registration so __libc_start_main doesn't issue the rseq
	// syscall — nsjail 3.4's kafel cannot name rseq for the static allowlist, and
	// rseq is a pure perf optimisation. Harmless under the denylist too.
	runEnv:            determinismEnv("GLIBC_TUNABLES=glibc.pthread.rseq=0"),
	capAddressSpace:   true,
	staticAllowlistOK: true,
	versionArgs:       []string{"-dumpfullversion"},
	parseVersion:      strings.TrimSpace,
}

var cppSpec = compiledLangSpec{
	name:       "cpp",
	sourceFile: "main.cpp",
	// No explicit link libs: the g++ driver folds -lm in and links libstdc++.
	compile:           []string{"g++", "-O2", "-static", "-std=c++20", "-o", "{out}", "{src}"},
	run:               []string{"{out}"},
	binNames:          []string{"g++"},
	runEnv:            determinismEnv("GLIBC_TUNABLES=glibc.pthread.rseq=0"),
	capAddressSpace:   true,
	staticAllowlistOK: true,
	versionArgs:       []string{"-dumpfullversion"},
	parseVersion:      strings.TrimSpace,
}

var goSpec = compiledLangSpec{
	name:       "go",
	sourceFile: "main.go",
	// `go build` of a single file needs no go.mod (module/multi-file is G3).
	// CGO_ENABLED=0 yields a pure-static binary (no libc link), so the run jail
	// stays minimal-rootfs like C/C++.
	//
	// Every run gets a FRESH tmpfs /tmp, so GOCACHE would be cold on every compile —
	// a cold single-file build recompiles the stdlib deps and takes ~14 s on one
	// CPU, blowing the compile budget. So the compile seeds a writable /tmp/gocache
	// from the image's pre-warmed read-only /opt/gocache (Dockerfile), which turns a
	// warm build into ~0.3 s. The copy runs in the jail via /bin/sh; the argv is
	// absolute so nsjail execve's it directly (compileArgv0Absolute).
	// cp -r (not -a): the jail-private uid can't preserve root ownership, and -a
	// would exit non-zero trying, breaking the && chain. -r copies content with the
	// files owned by the jail uid and the dirs writable, which is what go build needs.
	// -trimpath MUST match the multi-file build (compiled_multifile.go) AND the
	// Dockerfile warm cache: it is part of Go's build-cache key, so a mismatch is a
	// total cache miss → cold stdlib rebuild → compile timeout.
	compile:  []string{"/bin/sh", "-c", "cp -r /opt/gocache /tmp/gocache && exec go build -trimpath -o {out} {src}"},
	run:      []string{"{out}"},
	binNames: []string{"go"},
	compileEnv: []string{
		"CGO_ENABLED=0",
		"GOCACHE=/tmp/gocache", // seeded from /opt/gocache by the compile prelude
		"GOPATH=/tmp/gopath",
		"GOTOOLCHAIN=local", // no network toolchain fetch (the jail has no egress)
		"GOENV=off",         // no $HOME dependency the compile jail doesn't have
	},
	compileArgv0Absolute: true,
	// GOMAXPROCS=1 keeps the scheduler to one OS thread — fewer clone/thread churn
	// under the denylist and the per-run pids cap, plenty for judged programs.
	runEnv:            determinismEnv("GOMAXPROCS=1"),
	capAddressSpace:   false, // Go dies under RLIMIT_AS; cgroup memory.max bounds it
	staticAllowlistOK: false, // Go's scheduler needs a wider syscall set → denylist
	versionArgs:       []string{"version"},
	parseVersion:      parseGoVersion,
	// The seeded GOCACHE (~40 MB) plus incremental build output lives on /tmp; give
	// the compile jail a roomy tmpfs so it doesn't hit "no space left".
	compileTmpfsMB: 256,
}

// javaSpec is the VM-compiled shape (G2.2): javac compiles Main.java to bytecode,
// then the JVM runs it — the run step is `java -cp {dir} Main`, not "exec the
// artifact". Java is dynamically linked with a huge syscall surface, so it runs on
// the DENYLIST and the FULL rootfs (like the interpreted languages), not the tight
// static jail; and the JVM reserves a large virtual space, so it opts out of
// RLIMIT_AS and is bounded by -Xmx + the cgroup.
//
// Entrypoint convention: the student's public class must be `Main`, written to
// Main.java (javac ties the filename to the public class name). Multi-class
// single-file is fine; multiple public classes / packages are G3.
var javaSpec = compiledLangSpec{
	name:       "java",
	sourceFile: "Main.java",
	artifact:   "Main.class",
	// javac writes .class files into {dir} (the per-run /sandbox, writable at
	// compile). {src} is Main.java.
	compile:  []string{"javac", "-d", "{dir}", "{src}"},
	binNames: []string{"javac"},
	// The JVM runs the compiled class. -Xmx{mem}m bounds the heap (the cgroup is the
	// authoritative OOM); SerialGC + ActiveProcessorCount=1 keep threads/GC minimal
	// for a judged program; -XX:-UsePerfData avoids the /tmp hsperfdata write (and
	// its getpwuid on a jail uid with no passwd entry).
	run:               []string{"-XX:+UseSerialGC", "-XX:-UsePerfData", "-XX:ActiveProcessorCount=1", "-Xmx{mem}m", "-cp", "{dir}", "Main"},
	runBin:            []string{"java"},
	runEnv:            determinismEnv(),
	runFullRootfs:     true,  // the JVM is dynamically linked — needs its runtime libs
	capAddressSpace:   false, // the JVM reserves a large virtual space; RLIMIT_AS kills startup
	staticAllowlistOK: false, // widest syscall surface of any target → denylist
	memErrSubstr:      "OutOfMemoryError",
	versionArgs:       []string{"-version"},
	parseVersion:      parseJavaVersion,
	// The request's memory_mb becomes BOTH the -Xmx heap and the cgroup
	// memory.max, but JVM non-heap overhead (metaspace, code cache, GC structs,
	// thread stacks) sits on top of the heap — a python-sized budget (say 64 MB)
	// dies at startup or spuriously OOMs regardless of the program. Floor at the
	// global default (128 MB).
	minMemoryMB: 128,
	// Timeout floor for the JVM's cold start. Ordinary programs finish well inside
	// the 3 s default, but a MEMORY BOMB must reach its OutOfMemoryError / cgroup
	// OOM to be classified memory_exceeded — and the JVM spends its first ~1 s
	// starting up before the submitted code allocates anything. A tight requested
	// timeout (e.g. 500 ms) would otherwise trip the wall-clock deadline first and
	// misclassify the bomb as `timeout`. Floor at 2 s so cold-start + the allocation
	// that trips -Xmx/memory.max both fit, independent of the requested timeout.
	// (Still under the 3 s default, so ordinary runs are unaffected.)
	minTimeoutMs: 2000,
}

// parseGoVersion pulls the bare version out of `go version` output
// ("go version go1.26 linux/amd64" → "1.26"), degrading to the trimmed raw
// string if the shape is unexpected.
func parseGoVersion(out string) string {
	for _, f := range strings.Fields(out) {
		if v, ok := strings.CutPrefix(f, "go"); ok && len(v) > 0 && v[0] >= '0' && v[0] <= '9' {
			return v
		}
	}
	return strings.TrimSpace(out)
}

// parseJavaVersion pulls the version out of `javac -version` ("javac 21.0.10" →
// "21.0.10"), or the first dotted-number token, degrading to the trimmed raw
// string.
func parseJavaVersion(out string) string {
	for _, f := range strings.Fields(out) {
		if len(f) > 0 && f[0] >= '0' && f[0] <= '9' && strings.Contains(f, ".") {
			return f
		}
	}
	return strings.TrimSpace(out)
}

// defaultArtifact is the compiled artifact basename when a spec leaves it unset:
// the C/C++/Go static binary. It is written by the compile jail and read
// (read-only) by the run jail.
const defaultArtifact = "bin"

// compiledRuntime executes a compiled language in TWO separate jails: an
// untrusted compile jail (the compiler is itself hostile input — template/macro
// bombs, recursive includes) that writes a static artifact, then a stricter,
// minimal-rootfs run jail that executes only that artifact. It implements the
// same Runtime interface as interpreted languages, so dispatch never changes.
type compiledRuntime struct {
	spec             compiledLangSpec
	sandbox          sandbox.Sandbox
	compilerBin      string // absolute compiler path (nsjail execve's argv[0] directly)
	runBin           string // absolute VM launcher path for run argv[0] (Java); "" for static
	artifact         string // artifact basename produced/validated (spec.artifact or "bin")
	version          string
	outputLimit      int
	maxProcesses     int
	maxFileSizeMB    int
	compileMemoryMB  int                    // RLIMIT_AS/cgroup for the compile phase
	maxArtifactBytes int                    // reject artifacts larger than this (compile bombs)
	runSeccomp       sandbox.SeccompProfile // seccomp profile for the run jail
	maxReportBytes   int                    // cap on the returned test report (mode=test, G9)
}

// CompiledConfig carries the compile-phase knobs (execution knobs are shared with
// interpreted runtimes and passed positionally). The compile timeout is NOT here:
// like the run timeout it is clamped in the service layer and arrives on the
// request (req.CompileTimeoutMs), so there is one place limits are enforced.
type CompiledConfig struct {
	OutputLimit      int
	MaxProcesses     int
	MaxFileSizeMB    int
	CompileMemoryMB  int
	MaxArtifactBytes int
	RunSeccomp       sandbox.SeccompProfile
	MaxReportBytes   int // cap on the returned test report (mode=test, G9)
}

// NewC / NewCpp build the C and C++ runtimes on the shared sandbox.
func NewC(sb sandbox.Sandbox, cfg CompiledConfig) *compiledRuntime {
	return newCompiled(cSpec, sb, cfg)
}
func NewCpp(sb sandbox.Sandbox, cfg CompiledConfig) *compiledRuntime {
	return newCompiled(cppSpec, sb, cfg)
}

func newCompiled(spec compiledLangSpec, sb sandbox.Sandbox, cfg CompiledConfig) *compiledRuntime {
	bin := resolveBin(spec.binNames)
	// A language that cannot take the tight static allowlist is pinned to the
	// denylist regardless of RUNNER_STATIC_SECCOMP, so enabling the allowlist for
	// C/C++ never SIGSYS-kills a Go run for a syscall the C set omits.
	runSeccomp := cfg.RunSeccomp
	if !spec.staticAllowlistOK {
		runSeccomp = sandbox.SeccompDenylist
	}
	artifact := spec.artifact
	if artifact == "" {
		artifact = defaultArtifact
	}
	var runBin string
	if len(spec.runBin) > 0 {
		runBin = resolveBin(spec.runBin) // VM launcher (java); nsjail does no PATH search
	}
	return &compiledRuntime{
		spec:             spec,
		sandbox:          sb,
		compilerBin:      bin,
		runBin:           runBin,
		artifact:         artifact,
		version:          detectVersion(bin, languageSpec{versionArgs: spec.versionArgs, parseVersion: spec.parseVersion}),
		outputLimit:      cfg.OutputLimit,
		maxProcesses:     cfg.MaxProcesses,
		maxFileSizeMB:    cfg.MaxFileSizeMB,
		compileMemoryMB:  cfg.CompileMemoryMB,
		maxArtifactBytes: cfg.MaxArtifactBytes,
		runSeccomp:       runSeccomp,
		maxReportBytes:   cfg.MaxReportBytes,
	}
}

// NewGo builds the Go runtime. It reuses the two-jail compiled flow but runs on
// the denylist (not the static allowlist) and without RLIMIT_AS — see goSpec.
func NewGo(sb sandbox.Sandbox, cfg CompiledConfig) *compiledRuntime {
	return newCompiled(goSpec, sb, cfg)
}

// NewJava builds the Java (VM-compiled) runtime: javac to bytecode, then the JVM
// runs it on the full-rootfs denylist jail, -Xmx+cgroup bounded — see javaSpec.
func NewJava(sb sandbox.Sandbox, cfg CompiledConfig) *compiledRuntime {
	return newCompiled(javaSpec, sb, cfg)
}

func (r *compiledRuntime) Language() string { return r.spec.name }
func (r *compiledRuntime) Version() string  { return r.version }

// LimitFloors exposes the spec's per-language limit floors (G6); zero values
// mean the service's clamped limits are used as-is.
func (r *compiledRuntime) LimitFloors() Floors {
	return Floors{TimeoutMs: r.spec.minTimeoutMs, MemoryMB: r.spec.minMemoryMB}
}

// Run performs compile→run. A compile failure (non-zero exit or compile timeout)
// short-circuits to compile_error WITHOUT executing anything; only a validated
// artifact reaches the run jail.
func (r *compiledRuntime) Run(ctx context.Context, req runnerapi.RunRequest) runnerapi.RunResult {
	if req.Mode == modeTest {
		// mode=test (G9) is a single-toolchain-jail shape (compile+run in one jail),
		// wholly separate from the two-jail compile→run below, which stays unchanged.
		return r.runTest(ctx, req)
	}
	workDir, err := os.MkdirTemp("", "dalivim-build-*")
	if err != nil {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "could not create build dir"}
	}
	defer os.RemoveAll(workDir)

	// Materialize the program and resolve the compile/run plan. Single-file
	// source_code takes the exact legacy path (one fixed-name file + the {src}/{out}
	// templates); a files[] request materializes the validated tree under
	// /sandbox/src and uses the language's multi-file builders.
	plan, err := r.plan(workDir, req)
	if err != nil {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: err.Error()}
	}

	compileStart := time.Now()
	res, ok := r.compile(ctx, req, workDir, plan)
	compileMs := int(time.Since(compileStart).Milliseconds())
	if !ok {
		res.CompileMs = compileMs
		return res // compile_error or internal_error — nothing was executed
	}
	out := r.execute(ctx, req, workDir, plan)
	out.CompileMs = compileMs
	return out
}

// RunBatch is the compiled-language batch path (G6) — where the batch win
// concentrates: the artifact is compiled ONCE and then executed once per stdin
// against the same workDir. Each iteration is a fresh nsjail invocation with its
// own timeout ctx, output buffers, and cgroup (execute() builds all of that per
// call), so nothing carries between inputs except the read-only artifact.
func (r *compiledRuntime) RunBatch(ctx context.Context, req runnerapi.RunRequest, totalBudgetMs int) runnerapi.BatchResult {
	start := time.Now() // batch epoch: the compile counts against the total budget

	workDir, err := os.MkdirTemp("", "dalivim-build-*")
	if err != nil {
		return runnerapi.BatchResult{Status: runnerapi.StatusInternalError, Results: []runnerapi.RunResult{}}
	}
	defer os.RemoveAll(workDir)

	plan, err := r.plan(workDir, req)
	if err != nil {
		return runnerapi.BatchResult{Status: runnerapi.StatusInternalError, Results: []runnerapi.RunResult{}}
	}

	compileStart := time.Now()
	res, ok := r.compile(ctx, req, workDir, plan)
	compileMs := int(time.Since(compileStart).Milliseconds())
	if !ok {
		// compile_error (or internal_error) fails the WHOLE batch once — nothing
		// was executed, so there are no per-input results.
		return runnerapi.BatchResult{
			Status:        res.Status,
			CompileMs:     compileMs,
			CompileOutput: res.CompileOutput,
			Results:       []runnerapi.RunResult{},
		}
	}

	results, aborted := batchLoop(ctx, req.Stdins, start, totalBudgetMs, func(ctx context.Context, stdin string) runnerapi.RunResult {
		rq := req
		rq.Stdin = stdin
		return r.execute(ctx, rq, workDir, plan)
	})
	return runnerapi.BatchResult{
		Status:    runnerapi.BatchStatusOK,
		CompileMs: compileMs,
		Results:   results,
		Aborted:   aborted,
	}
}

// buildPlan is the resolved, language-specific compile/run recipe for one request
// — the seam that lets compile()/execute() stay identical across single-file and
// multi-file. All paths are absolute in-jail paths; runTemplate may still carry
// the {out}/{dir}/{mem} placeholders execute() substitutes.
type buildPlan struct {
	compileArgv     []string // compile jail argv (argv[0] is the bare tool name unless compileArgv0Absolute)
	artifactRel     string   // artifact path relative to workDir, validated after compile
	runTemplate     []string // run jail argv, {out}/{dir}/{mem}-templated
	extraCompileEnv []string // appended to the compile env (multi-file Go offline policy)
}

// plan materializes the submission into workDir and returns its buildPlan. The
// single-file branch is byte-for-byte the original behaviour; the multi-file
// branch drives the language's multiFile builders over the validated tree.
func (r *compiledRuntime) plan(workDir string, req runnerapi.RunRequest) (buildPlan, error) {
	if len(req.Files) == 0 {
		if err := os.WriteFile(filepath.Join(workDir, r.spec.sourceFile), []byte(req.SourceCode), 0o600); err != nil {
			return buildPlan{}, errors.New("could not write source")
		}
		// Link libraries go AFTER {src} so left-to-right symbol resolution works
		// (see spec.link). {dir} is the per-run jail workdir (Java's -d / -cp target).
		argv := subst(append(append([]string{}, r.spec.compile...), r.spec.link...),
			"{src}", sandbox.JailPath(r.spec.sourceFile), "{out}", sandbox.JailPath(r.artifact), "{dir}", sandbox.JailMount)
		return buildPlan{compileArgv: argv, artifactRel: r.artifact, runTemplate: r.spec.run}, nil
	}

	if r.spec.multiFile == nil {
		return buildPlan{}, errors.New("language does not support multi-file submissions")
	}
	files, err := materializeSource(workDir, req)
	if err != nil {
		return buildPlan{}, err
	}
	mf := r.spec.multiFile
	if mf.prep != nil {
		if err := mf.prep(workDir, req.Entrypoint); err != nil {
			return buildPlan{}, err
		}
	}
	return buildPlan{
		compileArgv:     mf.compileArgv(files, req.Entrypoint),
		artifactRel:     mf.artifactRel(req.Entrypoint),
		runTemplate:     mf.runTemplate(req.Entrypoint),
		extraCompileEnv: r.spec.multiFileCompileEnv,
	}, nil
}

// compile runs the compile jail. It returns (result, false) to short-circuit on
// compile_error/internal_error, or (zero, true) when a valid artifact exists.
func (r *compiledRuntime) compile(ctx context.Context, req runnerapi.RunRequest, workDir string, plan buildPlan) (runnerapi.RunResult, bool) {
	// req.CompileTimeoutMs is already clamped to [default, ceiling] by the service
	// (mirrors req.TimeoutMs for the run phase).
	timeout := req.CompileTimeoutMs
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
	defer cancel()

	argv := append([]string{}, plan.compileArgv...)
	if !r.spec.compileArgv0Absolute {
		argv[0] = r.compilerBin // absolute compiler path; nsjail does no PATH search
	}

	// The Go toolchain (`go build`) is itself a Go program and dies under any
	// RLIMIT_AS the same way its output does, so the compile jail skips the
	// address-space cap for a language that opts out and leans on the cgroup
	// memory.max (compileMemoryMB) instead.
	compileAS := 0
	if r.spec.capAddressSpace {
		compileAS = r.compileMemoryMB
	}
	cmd, acct := r.sandbox.Command(cctx, sandbox.Spec{
		Argv:           argv,
		WorkDir:        workDir,
		TimeoutMs:      timeout,
		AddressSpaceMB: compileAS,
		MemoryMB:       r.compileMemoryMB,
		MaxProcesses:   r.maxProcesses,
		MaxFileSizeMB:  r.maxFileSizeMB,
		TmpfsSizeMB:    r.spec.compileTmpfsMB,
		Writable:       true, // the compiler writes its artifact into /sandbox
		MinimalRootfs:  false,
	})
	if acct != nil {
		defer acct.Close()
	}
	// PATH includes the Go toolchain dir; TMPDIR so gcc/go intermediates land in
	// the size-capped tmpfs /tmp. Per-language compileEnv adds e.g. Go's isolated
	// GOCACHE/GOPATH; extraCompileEnv adds the multi-file-only offline module policy.
	cmd.Env = append([]string{"PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin", "TMPDIR=/tmp"}, r.spec.compileEnv...)
	cmd.Env = append(cmd.Env, plan.extraCompileEnv...)
	stderr := &limitedBuffer{limit: r.outputLimit}
	cmd.Stderr = stderr
	cmd.Stdout = &limitedBuffer{limit: r.outputLimit}

	runErr := cmd.Run()

	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return runnerapi.RunResult{
			Status:        runnerapi.StatusCompileError,
			CompileOutput: stderr.String() + "\n[compile timed out]",
		}, false
	}
	if runErr != nil {
		return runnerapi.RunResult{
			Status:        runnerapi.StatusCompileError,
			CompileOutput: stderr.String(),
		}, false
	}

	// Validate the artifact: it must exist and stay under the size cap (a compile
	// bomb that somehow linked huge). Otherwise it is our failure, not the code's.
	fi, err := os.Stat(filepath.Join(workDir, plan.artifactRel))
	if err != nil || fi.Size() == 0 {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "compile produced no artifact"}, false
	}
	if r.maxArtifactBytes > 0 && fi.Size() > int64(r.maxArtifactBytes) {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "compiled artifact exceeds size cap"}, false
	}
	return runnerapi.RunResult{}, true
}

// execute runs the compiled program in the run jail and classifies the outcome.
// Static languages (C/C++/Go) run the artifact directly in a minimal-rootfs jail;
// a VM language (Java) runs its launcher on the full-rootfs denylist jail.
func (r *compiledRuntime) execute(ctx context.Context, req runnerapi.RunRequest, workDir string, plan buildPlan) runnerapi.RunResult {
	rctx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
	defer cancel()
	// Cause-carrying cancel so an output flood kills the artifact and is told apart
	// from a deadline afterwards through context.Cause (G1.4).
	rctx, cancelCause := context.WithCancelCause(rctx)
	defer cancelCause(nil)

	// {out} is the artifact path (static run argv[0]); {dir} the jail workdir (Java's
	// -cp target); {mem} the heap budget for the VM's -Xmx flag.
	argv := subst(plan.runTemplate,
		"{out}", sandbox.JailPath(plan.artifactRel), "{dir}", sandbox.JailMount, "{mem}", strconv.Itoa(req.MemoryMB))
	if r.runBin != "" {
		argv[0] = r.runBin // absolute VM launcher (java); nsjail does no PATH search
	}

	// A glibc static binary (C/C++) tolerates a hard RLIMIT_AS; the Go runtime and
	// the JVM do not (they reserve a huge virtual space and die on startup), so they
	// opt out and are bounded by -Xmx (JVM) + the cgroup memory.max — see
	// capAddressSpace.
	runAS := 0
	if r.spec.capAddressSpace {
		runAS = req.MemoryMB
	}
	cmd, acct := r.sandbox.Command(rctx, sandbox.Spec{
		Argv:           argv,
		WorkDir:        workDir,
		TimeoutMs:      req.TimeoutMs,
		AddressSpaceMB: runAS,
		MemoryMB:       req.MemoryMB,
		MaxProcesses:   r.maxProcesses,
		MaxFileSizeMB:  r.maxFileSizeMB,
		Writable:       false,
		// Static artifacts get the minimal rootfs (no toolchain to re-invoke, D-4); a
		// dynamically-linked VM needs its runtime libs, so Java keeps the full rootfs
		// (same posture as the interpreted languages).
		MinimalRootfs: !r.spec.runFullRootfs,
		Seccomp:       r.runSeccomp, // tight static allowlist when enabled (default: denylist)
	})
	if acct != nil {
		defer acct.Close()
	}
	cmd.Stdin = strings.NewReader(req.Stdin)
	// The run env is per-language on top of the shared determinism pin (G7:
	// LANG/LC_ALL/TZ): C/C++ disable glibc rseq (see runEnv); Go pins GOMAXPROCS;
	// Java adds nothing. Never nil — an empty non-nil slice keeps the child from
	// inheriting the runner's environment.
	if cmd.Env = r.spec.runEnv; cmd.Env == nil {
		cmd.Env = []string{}
	}

	onFlood := func() { cancelCause(errOutputLimit) }
	stdout := &limitedBuffer{limit: r.outputLimit, onLimit: onFlood}
	stderr := &limitedBuffer{limit: r.outputLimit, onLimit: onFlood}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	runErr := cmd.Run()
	duration := int(time.Since(start).Milliseconds())

	res := runnerapi.RunResult{
		Stdout:          encodeStream(req.Encoding, stdout.String()),
		Stderr:          encodeStream(req.Encoding, stderr.String()),
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
		DurationMs:      duration,
		MemoryKB:        memoryKB(acct, cmd),
		Signal:          signalName(cmd),
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}

	switch {
	case errors.Is(rctx.Err(), context.DeadlineExceeded):
		res.Status = runnerapi.StatusTimeout
	case runErr == nil:
		res.Status = runnerapi.StatusSuccess
	case acct != nil && acct.OOMKilled():
		res.Status = runnerapi.StatusMemoryExceeded
	case r.spec.memErrSubstr != "" && strings.Contains(res.Stderr, r.spec.memErrSubstr):
		// No-cgroup fallback: -Xmx bounded the heap and the VM threw OutOfMemoryError.
		res.Status = runnerapi.StatusMemoryExceeded
	case errors.Is(context.Cause(rctx), errOutputLimit):
		res.Status = runnerapi.StatusOutputLimitExceeded
	default:
		res.Status = runnerapi.StatusRuntimeError
	}
	return res
}

// runTest is the compiled-language mode=test entry point (G9). Unlike run mode's
// two jails, a toolchain's `test` subcommand compiles AND runs in ONE jail, so
// this materializes the submission, synthesizes the module file, and launches the
// test command in a single full-rootfs, toolchain-present, writable-/tmp jail.
func (r *compiledRuntime) runTest(ctx context.Context, req runnerapi.RunRequest) runnerapi.RunResult {
	tc, ok := compiledTestCommands[r.spec.name]
	if !ok {
		// Defensive: the service only routes a test-supported language here.
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "test mode not supported for this language"}
	}
	workDir, err := os.MkdirTemp("", "dalivim-test-*")
	if err != nil {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "could not create sandbox dir"}
	}
	defer os.RemoveAll(workDir)

	if err := r.prepareTest(workDir, req, tc); err != nil {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: err.Error()}
	}
	return r.executeTest(ctx, req, workDir, tc)
}

// prepareTest materializes the submission under the source root (/sandbox/src, the
// module root) and runs the toolchain's module-file prep (synthesize go.mod). A
// files[] tree is materialized against the TEST policy (materializeSource is
// mode-aware); a single source_code test is written to the toolchain's test
// filename (go: main_test.go, so `go test` discovers it).
func (r *compiledRuntime) prepareTest(workDir string, req runnerapi.RunRequest, tc *compiledTestCommand) error {
	if len(req.Files) == 0 {
		srcDir := filepath.Join(workDir, srcRootName)
		if err := os.MkdirAll(srcDir, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(srcDir, tc.sourceFile), []byte(req.SourceCode), 0o600); err != nil {
			return errors.New("could not write source")
		}
	} else if _, err := materializeSource(workDir, req); err != nil {
		return err
	}
	if tc.prep != nil {
		return tc.prep(workDir)
	}
	return nil
}

// executeTest launches the single-jail test command and classifies the outcome
// (G9). The report is on stdout (go test -json), so the stdout buffer is sized to
// the report cap; /sandbox stays READ-ONLY (the toolchain writes to the writable
// /tmp), the toolchain rootfs is present, and the run stays on the denylist —
// never the static allowlist — because the toolchain touches a wide syscall
// surface.
func (r *compiledRuntime) executeTest(ctx context.Context, req runnerapi.RunRequest, workDir string, tc *compiledTestCommand) runnerapi.RunResult {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
	defer cancel()
	ctx, cancelCause := context.WithCancelCause(ctx)
	defer cancelCause(nil)

	cmd, acct := r.sandbox.Command(ctx, sandbox.Spec{
		Argv:      tc.argv,
		WorkDir:   workDir,
		TimeoutMs: req.TimeoutMs,
		// Go opts out of RLIMIT_AS (it reserves a huge virtual arena); the cgroup
		// memory.max is the authoritative bound — same posture as the Go run jail.
		AddressSpaceMB: 0,
		MemoryMB:       req.MemoryMB,
		MaxProcesses:   r.maxProcesses,
		MaxFileSizeMB:  r.maxFileSizeMB,
		TmpfsSizeMB:    tc.tmpfsMB, // roomy /tmp for the seeded GOCACHE + build output
		Writable:       false,      // /sandbox read-only: the toolchain writes to /tmp
		MinimalRootfs:  false,      // the toolchain must be present to compile+run
		Seccomp:        sandbox.SeccompDenylist,
	})
	if acct != nil {
		defer acct.Close()
	}
	cmd.Stdin = strings.NewReader("") // test mode ignores stdin
	cmd.Env = tc.env

	onFlood := func() { cancelCause(errOutputLimit) }
	// The report rides stdout (go test -json), so size that buffer to the report
	// cap, not the smaller stdout cap; stderr keeps the ordinary output cap.
	reportLimit := r.maxReportBytes
	if reportLimit <= 0 {
		reportLimit = r.outputLimit
	}
	stdout := &limitedBuffer{limit: reportLimit, onLimit: onFlood}
	stderr := &limitedBuffer{limit: r.outputLimit, onLimit: onFlood}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	_ = cmd.Run()
	duration := int(time.Since(start).Milliseconds())

	res := runnerapi.RunResult{
		Stdout:          encodeStream(req.Encoding, stdout.String()),
		Stderr:          encodeStream(req.Encoding, stderr.String()),
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
		DurationMs:      duration,
		MemoryKB:        memoryKB(acct, cmd),
		Signal:          signalName(cmd),
		ReportFormat:    tc.reportFormat,
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}

	report, produced, reportTrunc := readTestReport(workDir, "", stdout.raw(), r.maxReportBytes)
	res.TestReport = report
	// The report is the stdout stream, so its truncation is the stdout buffer's.
	res.TestReportTruncated = reportTrunc || stdout.truncated

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Status = runnerapi.StatusTimeout
	case acct != nil && acct.OOMKilled():
		res.Status = runnerapi.StatusMemoryExceeded
	case errors.Is(context.Cause(ctx), errOutputLimit):
		res.Status = runnerapi.StatusOutputLimitExceeded
	case tc.buildFailMarker != "" && strings.Contains(report, tc.buildFailMarker):
		// The toolchain failed to COMPILE the submission (go test -json emits
		// "Action":"build-fail" and exits 1, same as a test failure). Route it to
		// compile_error — go's run-mode status for a build failure — rather than
		// tests_failed, using the structured marker, not a stderr guess.
		res.Status = runnerapi.StatusCompileError
	default:
		if st := classifyTestExit(res.ExitCode, tc.testsFailedExit, produced); st != "" {
			res.Status = st
		} else {
			res.Status = runnerapi.StatusRuntimeError
		}
	}
	return res
}

// subst returns a copy of argv with each placeholder token replaced. pairs are
// old,new,old,new… ({src}, {out}, {dir}, {mem} — see the callers).
func subst(argv []string, pairs ...string) []string {
	repl := strings.NewReplacer(pairs...)
	cp := make([]string, len(argv))
	for i, a := range argv {
		cp[i] = repl.Replace(a)
	}
	return cp
}

// signalName returns the "SIGxxx" name of the signal that killed the process, or
// "" when it exited normally. Disambiguates SIGSEGV crashes, SIGKILL (OOM/limit)
// and SIGSYS (seccomp) from a clean non-zero exit.
func signalName(cmd *exec.Cmd) string {
	if cmd.ProcessState == nil {
		return ""
	}
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return ""
	}
	if name, known := signalNames[ws.Signal()]; known {
		return name
	}
	return "signal " + strconv.Itoa(int(ws.Signal()))
}

var signalNames = map[syscall.Signal]string{
	syscall.SIGSEGV: "SIGSEGV",
	syscall.SIGKILL: "SIGKILL",
	syscall.SIGABRT: "SIGABRT",
	syscall.SIGFPE:  "SIGFPE",
	syscall.SIGBUS:  "SIGBUS",
	syscall.SIGSYS:  "SIGSYS",
	syscall.SIGILL:  "SIGILL",
	syscall.SIGTERM: "SIGTERM",
	syscall.SIGINT:  "SIGINT",
	syscall.SIGXCPU: "SIGXCPU",
	syscall.SIGXFSZ: "SIGXFSZ",
}
