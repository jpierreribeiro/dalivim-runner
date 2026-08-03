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
	// unlimitedAddressSpace lifts RLIMIT_AS outright (both phases) for a runtime that
	// cannot start under nsjail's 4 GB DEFAULT either — capAddressSpace:false only
	// declines to add OUR cap, it does not remove nsjail's. The CoreCLR is the one
	// such runtime: under 4 GB it aborts at GC heap init (0x8007000E) before Main.
	// Real memory stays bounded by the per-run cgroup memory.max.
	unlimitedAddressSpace bool
	// maxOpenFiles raises RLIMIT_NOFILE above nsjail's default of 32 (both phases).
	// A runtime that maps one file per assembly (the CoreCLR) exhausts 32 mid-load
	// and reports it as a bogus "Could not load file or assembly". 0 keeps the
	// default, which is right for every other language here.
	maxOpenFiles int
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

// rustSpec is the fourth static-compiled language (G13), the cleanest since Go: a
// single-file, std-only program compiled to a STATIC musl binary that runs in the
// minimal-rootfs jail. Unlike Go there is NO cold-stdlib rebuild — the musl std
// ships precompiled as rlibs with the target — so a single-file build is ~0.3 s and
// no /opt cache pre-warm is needed.
//
// The musl target is static by construction (crt-static): rustc self-links via its
// bundled rust-lld and self-contained musl (verified: no external cc/musl-gcc is
// invoked), so the artifact is a static-PIE that needs nothing but itself. No cargo,
// no crates, no network — single-crate std is the whole surface, like single-file Go.
var rustSpec = compiledLangSpec{
	name:       "rust",
	sourceFile: "main.rs",
	// -O optimises; --edition 2021 pins the language edition (determinism). The musl
	// target makes the binary fully static → the run jail keeps the minimal rootfs.
	compile:  []string{"rustc", "--edition", "2021", "-O", "--target", "x86_64-unknown-linux-musl", "-o", "{out}", "{src}"},
	run:      []string{"{out}"},
	binNames: []string{"rustc"},
	runEnv:   determinismEnv(),
	// Rust uses the system allocator (musl malloc), NOT a Go/V8-style virtual arena,
	// so a hard RLIMIT_AS bounds it deterministically — capAddressSpace TRUE (like C).
	// Verified: a hello-world runs fine under RLIMIT_AS 256 MB.
	capAddressSpace: true,
	// Start on the DENYLIST: Rust std touches futex/getrandom/sigaltstack/rt_sig*/poll
	// — wider than the C/C++ static allowlist names. Denylist is already strong
	// (ro-rootfs + empty netns + no_new_privs + cgroup); tune an allowlist later via
	// RUNNER_STATIC_SECCOMP=complain (ADDING-A-LANGUAGE.md).
	staticAllowlistOK: false,
	// No-cgroup fallback OOM marker: under a hard RLIMIT_AS a Rust allocation failure
	// aborts with "memory allocation of N bytes failed" on stderr (verified). On the
	// target the cgroup OOM event is the authoritative memory_exceeded signal.
	memErrSubstr: "memory allocation of",
	versionArgs:  []string{"--version"}, // "rustc 1.83.0 (…)"
	parseVersion: secondField,           // -> "1.83.0"
	// rustc links the whole musl std into the artifact; give the compile jail a roomy
	// /tmp for the link intermediates (nsjail's default tmpfs is only a few MB).
	compileTmpfsMB: 128,
}

// odinSpec is Odin (https://odin-lang.org), a static-compiled (Shape B) language.
// Odin compiles native code via LLVM; `odin build <file> -file` builds a single
// file (package main + a `main :: proc()` entrypoint) to a native binary.
//
// UNVERIFIED ON-TARGET (needs the CI Docker smoke, like every compiled spec):
// this spec is written from Odin's documented CLI, not a local compile — the dev
// image has no Odin toolchain and the release download is proxy-blocked here. Two
// posture choices are made CONSERVATIVELY and must be confirmed on the target:
//   - runFullRootfs:true — a default `odin build` links libc DYNAMICALLY, so the
//     run jail keeps the full read-only rootfs (like the JVM), not the minimal
//     static one. A follow-up can pass `-extra-linker-flags:"-static"` to produce
//     a static binary and tighten to minimal-rootfs, IF Odin's runtime tolerates
//     a fully static libc link.
//   - capAddressSpace:true — Odin uses an ordinary OS allocator (no Go/JVM-style
//     virtual arena), so a hard RLIMIT_AS should bound it like C/Rust. Verify a
//     hello-world runs under RLIMIT_AS before trusting it; flip to false (cgroup
//     only) if it aborts at startup.
//   - denylist (staticAllowlistOK:false) — safest first posture; tune later with
//     RUNNER_STATIC_SECCOMP=complain per docs/future/ADDING-A-LANGUAGE.md.
var odinSpec = compiledLangSpec{
	name:       "odin",
	sourceFile: "main.odin",
	// -file builds the single source file (not a package dir); -out names the
	// artifact. Both {src} and {out} are in-jail /sandbox paths templated by the
	// compile builder. Optimization left at Odin's default to keep compile fast.
	compile:       []string{"odin", "build", "{src}", "-file", "-out:{out}"},
	run:           []string{"{out}"},
	binNames:      []string{"odin"},
	runEnv:        determinismEnv(),
	runFullRootfs: true, // dynamic libc link — see the on-target note above
	// FALSE, decided on target — and the reason is the COMPILER, not the program.
	// The artifact itself tolerates a hard RLIMIT_AS (a hello-world runs fine under
	// 128 MB), but this flag gates BOTH phases, and `odin build` embeds LLVM: under
	// the compile jail's cap it dies with
	//   src/common_memory.cpp(323): Panic: Out of Virtual Memory, oh no...
	// which surfaced as EVERY odin request returning compile_error. Same shape as
	// `go build`, which opts out for the same reason. The run is therefore bounded
	// by the cgroup memory.max (F-E/R6), like Go/JVM/CoreCLR.
	capAddressSpace: false,
	// Also decided on target: nsjail's DEFAULT RLIMIT_NOFILE is 32, and the Odin
	// compiler opens the core collection file by file. Under 32 it fails part-way
	// through, blaming whichever core file it happened to be on —
	//   /opt/odin/core/io/util.odin(5:1) Syntax Error: Unknown error whilst reading file
	// with a DIFFERENT file each run, which reads like a corrupt install and is not
	// one. Same trap the CoreCLR hit (see csharpSpec.maxOpenFiles).
	maxOpenFiles:      1024,
	staticAllowlistOK: false, // denylist first (LLVM runtime syscall surface)
	versionArgs:       []string{"version"},
	parseVersion:      parseOdinVersion,
	// LLVM link intermediates want more scratch than nsjail's few-MB default /tmp.
	compileTmpfsMB: 128,
}

// parseOdinVersion turns `odin version` output ("odin version dev-2024-05:abc" or
// "odin version 0.13.0") into the bare version token after the word "version".
func parseOdinVersion(out string) string {
	fields := strings.Fields(out)
	for i, f := range fields {
		if f == "version" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	if len(fields) > 0 {
		return fields[len(fields)-1]
	}
	return ""
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

// typescriptSpec is TypeScript (G14). TypeScript is not a runtime — it is a
// COMPILE step that produces JavaScript — so it maps mechanically onto Shape C
// (like Java): compile to an intermediate (tsc -> main.js), then run it on a "VM",
// here the existing Node run posture (javascriptSpec). No new sandbox, no new run
// shape — the run jail is JavaScript's, verbatim.
//
// The compiler is `node /opt/typescript/bin/tsc` (tsc is itself a Node program):
// compileArgv0Absolute is FALSE so compile[0] ("node") is resolved to the image's
// absolute node path, exactly like the compilerBin the other specs execve. The tsc
// flags are PINNED by the runner (never the caller) — the type-check strictness and
// target are a runner guarantee, the analogue of the sqlite/pytest flag pinning:
//
//	--strict          full type-checking (a type error is the whole point of TS)
//	--noEmitOnError   emit NOTHING on a type error -> no main.js artifact -> the
//	                  compile phase short-circuits to compile_error, nothing runs
//	--skipLibCheck    do not type-check the bundled .d.ts internals. REQUIRED:
//	                  @types/node references `undici-types` (a transitive types dep
//	                  we deliberately do not bundle), and without this its own
//	                  fetch/worker_threads.d.ts fail to resolve it (TS2307) and
//	                  block emit for EVERY program. skipLibCheck skips checking the
//	                  library declarations, not the student's code — an implicit-any
//	                  or wrong-type in the submission is still a compile_error.
//	--esModuleInterop let `import fs from "fs"` (default import of a CommonJS module)
//	                  type-check and run, the common student ergonomics.
//	--target/--module/--lib ES2020 + commonjs   pinned language level; Node 18 runs
//	                  the emitted CommonJS directly.
//	--types node --typeRoots /opt/ts-types   resolve the bundled @types/node so the
//	                  Node globals (console, process, Buffer, require, the fs/net
//	                  modules) type-check — without this even `console.log` is a type
//	                  error under --lib ES2020. The types are pure .d.ts (no runtime
//	                  code), so they add no execution surface.
//	--outDir {dir} {src}   emit main.js into the per-run /sandbox next to main.ts
//	                  (validated as the artifact before the run jail launches).
//
// The RUN step is the JavaScript posture: node runs the emitted main.js on the
// FULL rootfs denylist jail (node is dynamically linked, like the JVM), V8's heap
// bounded by --max-old-space-size (RLIMIT_AS refused — capAddressSpace FALSE, like
// Node/Go/JVM), the cgroup memory.max the authoritative OOM. Single-file, std-only:
// no npm, no node_modules, no crate/module resolution off the tree, offline.
var typescriptSpec = compiledLangSpec{
	name:       "typescript",
	sourceFile: "main.ts",
	compile: []string{"node", "/opt/typescript/bin/tsc",
		"--strict", "--noEmitOnError", "--skipLibCheck", "--esModuleInterop",
		"--target", "ES2020", "--module", "commonjs", "--lib", "ES2020",
		"--types", "node", "--typeRoots", "/opt/ts-types",
		"--outDir", "{dir}", "{src}"},
	// compile[0] ("node") is resolved to the image's absolute node path (like the
	// other compilers); tsc itself is a literal script arg, not argv[0].
	binNames: []string{"node", "nodejs"},
	artifact: "main.js", // tsc emits main.js from main.ts into {dir}; validated after compile
	// RUN = the JavaScript posture: node executes the emitted CommonJS. --disable-proto
	// closes a prototype-pollution path; --max-old-space-size bounds V8's heap to the
	// run budget (RLIMIT_AS refused). runBin resolves node's absolute launcher.
	run:               []string{"--disable-proto=throw", "--max-old-space-size={mem}", "{out}"},
	runBin:            []string{"node", "nodejs"},
	runEnv:            determinismEnv("PATH=/usr/local/bin:/usr/bin:/bin"),
	runFullRootfs:     true,  // node is dynamically linked — keep the full rootfs (like the JVM)
	capAddressSpace:   false, // V8 reserves a virtual cage; bound the heap + cgroup, no RLIMIT_AS
	staticAllowlistOK: false, // node's syscall surface is wide → denylist (like the interpreted JS jail)
	// No memErrSubstr: Node's OOM ("JavaScript heap out of memory") is a V8 abort,
	// classified runtime_error, exactly like javascriptSpec; the cgroup memory.max is
	// the authoritative memory_exceeded signal on-target.
	versionArgs:  []string{"/opt/typescript/bin/tsc", "--version"}, // node runs tsc: "Version 5.9.3"
	parseVersion: secondField,                                      // -> "5.9.3"
}

// csharpSpec is C# (.NET, G15), the second VM-compiled language after Java. It
// slots into Shape C: compile the source to an IL assembly (Roslyn `csc`), run the
// IL on the CoreCLR (`dotnet exec`), reusing the generalised compiledRuntime and
// Java's run posture — full-rootfs denylist jail, no RLIMIT_AS (the CLR reserves a
// large virtual space, like the JVM/V8), cgroup-authoritative memory, a startup
// memory floor. Validated end-to-end against the real SDK (8.0.422 / runtime 8.0.28)
// before coding.
//
// The genuinely hard part is not the shape (Java proved it) but staying OFFLINE:
// .NET's reflex is to restore NuGet packages over the network. csc-direct sidesteps
// MSBuild/restore ENTIRELY — it is just the compiler, invoked against the shared
// framework's reference assemblies (a pre-baked response file, /opt/cs/refs.rsp),
// producing Main.dll. `dotnet exec` then hosts it with a PROGRAM-INDEPENDENT
// runtimeconfig.json (baked once at image build; it depends only on the target
// framework, so the compile prelude just copies it next to Main.dll). No cargo/npm
// analogue, no crate/package restore, no network — the shared framework assemblies
// shipped in the image are the whole surface, like single-file Java.
//
// The compile is a /bin/sh prelude (compileArgv0Absolute) so the `csc && cp
// runtimeconfig` chain runs in one jail: csc emits Main.dll, then the baked
// runtimeconfig is copied to Main.runtimeconfig.json beside it (dotnet exec looks
// for <assembly>.runtimeconfig.json). The csc.dll path globs the single SDK.
var csharpSpec = compiledLangSpec{
	name:       "csharp",
	sourceFile: "Main.cs",
	// csc-direct: `dotnet exec <sdk>/Roslyn/bincore/csc.dll -nostdlib @refs.rsp` (the
	// ref pack referenced via the baked response file) -> Main.dll, then copy the baked
	// runtimeconfig beside it. -optimize+ for release; -nostdlib because the ref pack
	// (refs.rsp) supplies System.Runtime/Console/… explicitly. {out}/{src}/{dir} are
	// substituted to in-jail /sandbox paths; /opt/dotnet + /opt/cs are on the ro rootfs.
	compile: []string{"/bin/sh", "-c",
		"/opt/dotnet/dotnet exec $(echo /opt/dotnet/sdk/*/Roslyn/bincore/csc.dll) -nologo -optimize+ -nostdlib @/opt/cs/refs.rsp -out:{out} {src} && cp /opt/cs/Main.runtimeconfig.json {dir}/Main.runtimeconfig.json"},
	compileArgv0Absolute: true,
	artifact:             "Main.dll",
	// RUN on the CoreCLR: `dotnet exec Main.dll` (dotnet finds Main.runtimeconfig.json
	// beside it and the shared framework via DOTNET_ROOT). runBin is the dotnet muxer.
	run:      []string{"exec", "{out}"},
	runBin:   []string{"dotnet"},
	binNames: []string{"dotnet"},
	// Compile env: DOTNET_ROOT for the CLR hosting csc; a writable HOME/CLI_HOME on the
	// size-capped /tmp; offline/quiet/no-diagnostics. capAddressSpace:false means the
	// compile jail (the CLR running csc reserves a virtual arena, like `go build`/javac)
	// skips RLIMIT_AS and leans on the cgroup — see execute()/compile().
	compileEnv: []string{
		"DOTNET_ROOT=/opt/dotnet", "HOME=/tmp", "DOTNET_CLI_HOME=/tmp",
		"DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1", "DOTNET_EnableDiagnostics=0",
		// .NET 8's W^X JIT double-maps its code heap through a FILE it ftruncates to 2 TB
		// (a sparse file). RLIMIT_FSIZE (the jail's per-file cap) counts logical size, so
		// ANY cap blocks it -> SIGXFSZ -> a bogus compile_error (verified: exit 153).
		// Disable W^X: the JIT falls back to ordinary pages. The in-process hardening is
		// redundant here — the jail already contains the process (ro rootfs, empty netns,
		// seccomp denylist, cgroup). Both csc AND the run JIT need this, so it is on both.
		"DOTNET_EnableWriteXorExecute=0",
		// Invariant globalization: the runtime base has no libicu, and the CLR aborts
		// ("Couldn't find a valid ICU package") the moment globalization is invoked — csc
		// trips it just formatting an internal exception message. Invariant mode drops the
		// ICU dependency AND collapses every culture to the invariant one, which is exactly
		// the culture-independence the runner already pins (C.UTF-8, G7). On both phases.
		"DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1",
		// Roslyn's csc runs Server GC by default (one heap per core, each reserving a
		// large segment). Under the compile jail's tight budget it cannot lay those
		// heaps out and CoreCLR aborts before csc's Main even runs: "GC heap
		// initialization failed with error 0x8007000E" (E_OUTOFMEMORY). Force
		// Workstation GC (a single heap) so csc starts. The env var overrides the
		// value baked into csc.runtimeconfig.json.
		"DOTNET_gcServer=0",
		// nsjail sets --disable_proc, so /proc/meminfo is not visible; pin an explicit
		// GC heap hard limit (384 MiB, well inside RUNNER_COMPILE_MEMORY_MB=512) so heap
		// sizing is deterministic and /proc-independent; ample for a single-file compile.
		"DOTNET_GCHeapHardLimit=0x18000000",
	},
	// Run env: the shared determinism pin + DOTNET_ROOT (locate the shared framework),
	// writable HOME/TMPDIR on the jail's /tmp, telemetry/first-run off, and
	// DOTNET_EnableDiagnostics=0 so the CLR opens no /tmp diagnostic IPC socket. Never
	// inherit the runner's env (COMPlus_*/DOTNET_* must not leak in — the Java lesson).
	runEnv: determinismEnv(
		"PATH=/usr/local/bin:/usr/bin:/bin", "DOTNET_ROOT=/opt/dotnet",
		"HOME=/tmp", "TMPDIR=/tmp", "DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1",
		"DOTNET_EnableDiagnostics=0",
		// See compileEnv: the run JIT also file-backs W^X and would trip RLIMIT_FSIZE.
		"DOTNET_EnableWriteXorExecute=0",
		// See compileEnv: no libicu in the base; invariant mode (also the runner's
		// culture-independence guarantee). The run env DOES set LC_ALL=C.UTF-8 (the
		// determinism pin), which is precisely what would trigger the ICU load.
		"DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1",
		// See compileEnv: Workstation GC (single heap) so the CoreCLR starts under the
		// jail's constrained, /proc-less memory view instead of aborting at GC heap
		// init.
		"DOTNET_gcServer=0",
		// The run's heap ceiling, per request ({memhex} = memory_mb as hex bytes).
		// This is C#'s -Xmx: unlimitedAddressSpace removes RLIMIT_AS, which WAS the
		// only per-run bound when no cgroup is delegated, so the ceiling moves into
		// the runtime itself instead of vanishing. With a cgroup (F-E/R6, production)
		// memory.max still bounds real RSS and classifies via the kernel OOM event;
		// without one, the CLR fail-fasts with "Out of memory." and memErrSubstr
		// classifies it — the same two-path posture as Java. nsjail --disable_proc hides
		// /proc/meminfo, so an explicit limit is also what keeps GC sizing
		// deterministic rather than /proc-derived.
		"DOTNET_GCHeapHardLimit={memhex}",
	),
	runFullRootfs:   true,  // the CoreCLR is dynamically linked — full rootfs (like the JVM)
	capAddressSpace: false, // the CLR reserves a large virtual space; RLIMIT_AS kills startup
	// ...and declining OUR cap is not enough: nsjail's own 4 GB default still aborts
	// the CoreCLR at GC heap init (0x8007000E), in the compile phase too (csc is a
	// .NET program). Lift RLIMIT_AS outright; memory.max remains the real bound.
	unlimitedAddressSpace: true,
	// 32 descriptors (nsjail's default) is fewer than the CoreCLR needs to map the
	// framework: the loader runs out mid-load and blames the last assembly it wanted
	// ("Could not load file or assembly 'System.Console'"), which reads like a
	// missing-file bug and is not one.
	maxOpenFiles:      1024,
	staticAllowlistOK: false, // widest syscall surface (JIT mmap, clone) → denylist, like the JVM
	// No-cgroup fallback OOM classify. NOT "OutOfMemoryException": a GCHeapHardLimit
	// breach is a runtime FAIL-FAST, not a catchable exception — the CLR prints
	// exactly "Out of memory." to stderr and aborts (verified on target, exit 139,
	// for both an incremental bomb and one oversized allocation). Matching the
	// exception name instead would classify every C# OOM as a plain runtime_error.
	memErrSubstr: "Out of memory",
	// The CLR's non-heap overhead (JIT, metadata) sits on top of allocations, like the
	// JVM: floor an undersized budget so it starts at all. cgroup memory.max is the
	// authoritative bound (deploy-time proof, like Go/Java — not the cgroup=auto CI).
	minMemoryMB:  128,
	versionArgs:  []string{"--version"}, // `dotnet --version` -> "8.0.422"
	parseVersion: strings.TrimSpace,
	// csc via the CLR writes a little JIT/temp scratch; give /tmp a roomy tmpfs (the
	// output Main.dll itself goes to the writable /sandbox, not /tmp).
	compileTmpfsMB: 64,
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

// NewRust builds the Rust runtime (G13): rustc compiles a single-file program to a
// static musl binary run in the minimal-rootfs jail, on the denylist and with
// RLIMIT_AS (Rust tolerates it, unlike Go) — see rustSpec.
func NewRust(sb sandbox.Sandbox, cfg CompiledConfig) *compiledRuntime {
	return newCompiled(rustSpec, sb, cfg)
}

// NewOdin builds the Odin runtime (Shape B): `odin build -file` compiles the
// single source to a native binary run on the full-rootfs denylist jail — see
// odinSpec (posture is conservative and needs on-target validation).
func NewOdin(sb sandbox.Sandbox, cfg CompiledConfig) *compiledRuntime {
	return newCompiled(odinSpec, sb, cfg)
}

// NewJava builds the Java (VM-compiled) runtime: javac to bytecode, then the JVM
// runs it on the full-rootfs denylist jail, -Xmx+cgroup bounded — see javaSpec.
func NewJava(sb sandbox.Sandbox, cfg CompiledConfig) *compiledRuntime {
	return newCompiled(javaSpec, sb, cfg)
}

// NewTypeScript builds the TypeScript runtime (G14): tsc type-checks and emits
// main.js (a type error is compile_error, --noEmitOnError), then node runs the
// emitted JS on the full-rootfs denylist jail — the JavaScript run posture reused
// whole — see typescriptSpec.
func NewTypeScript(sb sandbox.Sandbox, cfg CompiledConfig) *compiledRuntime {
	return newCompiled(typescriptSpec, sb, cfg)
}

// NewCSharp builds the C# runtime (G15): csc-direct compiles Main.cs to an IL
// Main.dll offline (no NuGet restore), then the CoreCLR runs it via `dotnet exec`
// on the full-rootfs denylist jail — Java's VM run posture reused — see csharpSpec.
func NewCSharp(sb sandbox.Sandbox, cfg CompiledConfig) *compiledRuntime {
	return newCompiled(csharpSpec, sb, cfg)
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
	cmd, acct, err := r.sandbox.Command(cctx, sandbox.Spec{
		Argv:           argv,
		WorkDir:        workDir,
		TimeoutMs:      timeout,
		AddressSpaceMB: compileAS,
		// csc is itself a .NET program, so the compile jail needs the same two
		// CoreCLR concessions as the run jail — see the spec fields.
		UnlimitedAddressSpace: r.spec.unlimitedAddressSpace,
		MaxOpenFiles:          r.spec.maxOpenFiles,
		MemoryMB:              r.compileMemoryMB,
		MaxProcesses:          r.maxProcesses,
		MaxFileSizeMB:         r.maxFileSizeMB,
		TmpfsSizeMB:           r.spec.compileTmpfsMB,
		Writable:              true, // the compiler writes its artifact into /sandbox
		MinimalRootfs:         false,
	})
	if err != nil {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "sandbox containment unavailable"}, false
	}
	if acct != nil {
		defer acct.Close()
	}
	// PATH includes the Go toolchain dir; TMPDIR so gcc/go intermediates land in
	// the size-capped tmpfs /tmp. Per-language compileEnv adds e.g. Go's isolated
	// GOCACHE/GOPATH; extraCompileEnv adds the multi-file-only offline module policy.
	cmd.Env = append([]string{"PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin", "TMPDIR=/tmp"}, r.spec.compileEnv...)
	cmd.Env = append(cmd.Env, plan.extraCompileEnv...)
	outputLimit := effectiveOutputLimit(req.OutputLimitBytes, r.outputLimit)
	// Compiler diagnostics go to ONE combined buffer, because the stream a compiler
	// reports on is not universal: gcc/g++/go/javac write to stderr, but Roslyn's csc
	// writes to STDOUT. Capturing stderr alone handed the C# student an EMPTY
	// compile_output for a real type error. Sharing one buffer also bounds the pair
	// once (os/exec serialises writes when Stdout == Stderr, so this is race-free).
	diag := &limitedBuffer{limit: outputLimit}
	cmd.Stderr = diag
	cmd.Stdout = diag

	runErr := cmd.Run()

	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return runnerapi.RunResult{
			Status:        runnerapi.StatusCompileError,
			CompileOutput: diag.String() + "\n[compile timed out]",
		}, false
	}
	if runErr != nil {
		return runnerapi.RunResult{
			Status:        runnerapi.StatusCompileError,
			CompileOutput: diag.String(),
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
	cmd, acct, err := r.sandbox.Command(rctx, sandbox.Spec{
		Argv:      argv,
		WorkDir:   workDir,
		TimeoutMs: req.TimeoutMs,
		// The CoreCLR needs RLIMIT_AS lifted (nsjail's 4 GB default aborts GC heap
		// init) and more than 32 descriptors (one mmap per assembly) — see the spec.
		AddressSpaceMB:        runAS,
		UnlimitedAddressSpace: r.spec.unlimitedAddressSpace,
		MaxOpenFiles:          r.spec.maxOpenFiles,
		MemoryMB:              req.MemoryMB,
		MaxProcesses:          r.maxProcesses,
		MaxFileSizeMB:         r.maxFileSizeMB,
		Writable:              false,
		// Static artifacts get the minimal rootfs (no toolchain to re-invoke, D-4); a
		// dynamically-linked VM needs its runtime libs, so Java keeps the full rootfs
		// (same posture as the interpreted languages).
		MinimalRootfs: !r.spec.runFullRootfs,
		Seccomp:       r.runSeccomp, // tight static allowlist when enabled (default: denylist)
	})
	if err != nil {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "sandbox containment unavailable"}
	}
	if acct != nil {
		defer acct.Close()
	}
	cmd.Stdin = strings.NewReader(req.Stdin)
	// The run env is per-language on top of the shared determinism pin (G7:
	// LANG/LC_ALL/TZ): C/C++ disable glibc rseq (see runEnv); Go pins GOMAXPROCS;
	// Java adds nothing. Never nil — an empty non-nil slice keeps the child from
	// inheriting the runner's environment.
	//
	// {memhex} is the run's memory budget as a hex byte count, for a runtime whose
	// heap ceiling is an ENV var rather than an argv flag (the CoreCLR's
	// DOTNET_GCHeapHardLimit — the exact analogue of Java's -Xmx{mem}m in the run
	// argv). subst copies, so the spec's slice is never mutated.
	if cmd.Env = subst(r.spec.runEnv, "{memhex}", memHex(req.MemoryMB)); cmd.Env == nil {
		cmd.Env = []string{}
	}

	onFlood := func() { cancelCause(errOutputLimit) }
	outputLimit := effectiveOutputLimit(req.OutputLimitBytes, r.outputLimit)
	stdout := &limitedBuffer{limit: outputLimit, onLimit: onFlood}
	stderr := &limitedBuffer{limit: outputLimit, onLimit: onFlood}
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

	cmd, acct, err := r.sandbox.Command(ctx, sandbox.Spec{
		// {mem} => the run's memory budget (Java's -Xmx heap flag); go's argv has no
		// placeholder, so subst is a no-op there.
		Argv:      subst(tc.argv, "{mem}", strconv.Itoa(req.MemoryMB)),
		WorkDir:   workDir,
		TimeoutMs: req.TimeoutMs,
		// The Go runtime and the JVM both reserve a huge virtual arena and die under a
		// tight RLIMIT_AS, so both opt out; the cgroup memory.max (+ -Xmx for Java) is
		// the authoritative bound — same posture as their run jails.
		AddressSpaceMB: 0,
		MemoryMB:       req.MemoryMB,
		MaxProcesses:   r.maxProcesses,
		MaxFileSizeMB:  r.maxFileSizeMB,
		TmpfsSizeMB:    tc.tmpfsMB, // roomy /tmp for the seeded GOCACHE + build output
		// go: /sandbox read-only (its build output goes to /tmp, report on stdout).
		// java: /sandbox WRITABLE so javac's classes and the JUnit XML report are
		// host-visible for the hand-back (the jail /tmp is a private tmpfs).
		Writable:      tc.writable,
		MinimalRootfs: false, // the toolchain must be present to compile+run
		Seccomp:       sandbox.SeccompDenylist,
	})
	if err != nil {
		return runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "sandbox containment unavailable"}
	}
	if acct != nil {
		defer acct.Close()
	}
	cmd.Stdin = strings.NewReader("") // test mode ignores stdin
	cmd.Env = tc.env

	onFlood := func() { cancelCause(errOutputLimit) }
	outputLimit := effectiveOutputLimit(req.OutputLimitBytes, r.outputLimit)
	// The report rides stdout (go test -json), so size that buffer to the report
	// cap, not the smaller stdout cap; stderr keeps the ordinary output cap.
	reportLimit := r.maxReportBytes
	if reportLimit <= 0 {
		reportLimit = outputLimit
	}
	stdout := &limitedBuffer{limit: reportLimit, onLimit: onFlood}
	stderr := &limitedBuffer{limit: outputLimit, onLimit: onFlood}
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

	// go: the report IS the stdout stream (reportFile ""). java: it is a JUnit XML
	// file handed back over the writable /sandbox bind (reportFile set).
	report, produced, reportTrunc := readTestReport(workDir, tc.reportFile, stdout.raw(), r.maxReportBytes)
	res.TestReport = report
	res.TestReportTruncated = reportTrunc || (tc.reportFile == "" && stdout.truncated)

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Status = runnerapi.StatusTimeout
	case acct != nil && acct.OOMKilled():
		res.Status = runnerapi.StatusMemoryExceeded
	case errors.Is(context.Cause(ctx), errOutputLimit):
		res.Status = runnerapi.StatusOutputLimitExceeded
	case tc.compileFailExit != 0 && res.ExitCode == tc.compileFailExit:
		// The prelude's compile step failed and short-circuited with its sentinel
		// exit code (Java: javac `|| exit 42`), which the test launcher never returns
		// — so this is a compile_error, not a test failure or a runtime crash.
		res.Status = runnerapi.StatusCompileError
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
// memHex renders a megabyte budget as the hex BYTE count the CoreCLR's
// DOTNET_GCHeapHardLimit expects (128 -> "0x8000000"). A non-positive budget
// yields "" so the substitution degrades to an unset-looking value rather than a
// nonsensical 0-byte heap.
func memHex(mb int) string {
	if mb <= 0 {
		return ""
	}
	return "0x" + strconv.FormatInt(int64(mb)<<20, 16)
}

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
