package executor

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jpierreribeiro/dalivim-runner/internal/sandbox"
)

// multiFileBuild is a compiled language's MULTI-FILE build/run recipe (G3). It is
// the compiled analogue of languageSpec.multiFileRunArgs: pure construction of
// argv and artifact paths from a validated file set and entrypoint, with the
// containment (jails, rlimits, seccomp) still owned by the sandbox. All paths it
// emits are absolute in-jail paths built from already-validated components, so no
// argv element can be read as a flag or escape the jail.
type multiFileBuild struct {
	// prep runs on the host after materialization and before the compile jail:
	// create output directories (Java's -d target) or synthesize a build file
	// (Go's go.mod). workDir is the per-run host dir; entry the resolved entrypoint.
	prep func(workDir, entry string) error

	// compileArgv builds the compile jail argv over the sorted materialized files.
	// argv[0] is the bare tool name (compile() resolves it to an absolute path)
	// unless the base spec sets compileArgv0Absolute (Go's /bin/sh prelude).
	compileArgv func(files []MaterializedFile, entry string) []string

	// artifactRel is the artifact path (relative to workDir) the build must produce
	// and the runner validates before the run jail starts.
	artifactRel func(entry string) string

	// runTemplate is the run jail argv ({mem}-templated for the JVM heap flag).
	// argv[0] is replaced with the resolved run binary when the base spec sets
	// runBin (Java's launcher).
	runTemplate func(entry string) []string
}

// init wires each compiled language's multi-file builders from its base spec, so
// the single-file templates stay the single source of the compiler flags. Adding
// a compiled language is still a deliberate edit here, never request-driven.
func init() {
	cSpec.multiFile = cFamilyMultiFile(cSpec.compile, cSpec.link, cSpec.name)
	cppSpec.multiFile = cFamilyMultiFile(cppSpec.compile, cppSpec.link, cppSpec.name)

	goSpec.multiFile = goMultiFile()
	// Multi-file Go builds a synthesized module, so pin the offline module policy:
	// no proxy, no checksum DB, no workspace. A student import of an external
	// package fails closed ("module lookup disabled by GOPROXY=off") instead of
	// reaching for the (absent) network.
	goSpec.multiFileCompileEnv = []string{"GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOFLAGS=-mod=mod"}

	javaSpec.multiFile = javaMultiFile()
}

// cFamilyMultiFile builds the C/C++ multi-file recipe from the base single-file
// compile template, so -O2/-static/-std stay defined once. It compiles ALL
// translation units together with -I/sandbox/src for header resolution and links
// the base spec's libs; the artifact is the same static binary as single-file.
func cFamilyMultiFile(compileTmpl, link []string, language string) *multiFileBuild {
	return &multiFileBuild{
		compileArgv: func(files []MaterializedFile, _ string) []string {
			args := make([]string, 0, len(compileTmpl)+len(files)+len(link)+1)
			for _, tok := range compileTmpl {
				if tok == "{src}" { // drop the single-file source slot
					continue
				}
				args = append(args, tok)
			}
			args = subst(args, "{out}", sandbox.JailPath(defaultArtifact), "{dir}", sandbox.JailMount)
			// -I the source root so `#include "util.h"` resolves across the tree.
			args = append(args, "-I"+srcJailDir())
			args = append(args, compileUnits(files, language)...)
			args = append(args, link...) // link libs AFTER the objects (symbol order)
			return args
		},
		artifactRel: func(string) string { return defaultArtifact },
		runTemplate: func(string) []string { return []string{sandbox.JailPath(defaultArtifact)} },
	}
}

// goModContent is the synthesized module file for a multi-file Go submission. The
// module path is local and meaningless (no network), and the go directive is a
// floor the local toolchain satisfies. A user-provided go.mod is policy-forbidden,
// so this is always the module root.
const goModContent = "module sandbox.local/submission\n\ngo 1.23\n"

// goMultiFile builds the Go multi-file recipe: synthesize go.mod at the source
// root, then `go build` the entrypoint's package directory (never ./...). The
// /bin/sh prelude seeds GOCACHE from the pre-warmed image cache exactly as the
// single-file Go build does.
func goMultiFile() *multiFileBuild {
	return &multiFileBuild{
		prep: func(workDir, _ string) error {
			return os.WriteFile(filepath.Join(workDir, srcRootName, "go.mod"), []byte(goModContent), 0o600)
		},
		compileArgv: func(_ []MaterializedFile, entry string) []string {
			pkgDir := srcJailDir()
			if d := path.Dir(entry); d != "." {
				pkgDir = srcJailDir() + "/" + d
			}
			// Build the entrypoint's package directory only. `cd` into it (safe:
			// pkgDir is built from a validated, grammar-restricted entrypoint) so the
			// build defines one clear executable, unlike `go build ./...`.
			sh := "cp -r /opt/gocache /tmp/gocache && cd " + shellQuote(pkgDir) +
				" && exec go build -trimpath -o " + sandbox.JailPath(defaultArtifact) + " ."
			return []string{"/bin/sh", "-c", sh}
		},
		artifactRel: func(string) string { return defaultArtifact },
		runTemplate: func(string) []string { return []string{sandbox.JailPath(defaultArtifact)} },
	}
}

// javaClassesDir is the compile output / classpath directory (relative to workDir
// and, joined onto /sandbox, in-jail).
const javaClassesDir = "classes"

// javaMultiFile builds the Java multi-file recipe: javac all sources into
// /sandbox/classes, then run the entrypoint class off that classpath. The
// entrypoint is a class name (validated as such), never a path.
func javaMultiFile() *multiFileBuild {
	return &multiFileBuild{
		prep: func(workDir, _ string) error {
			return os.MkdirAll(filepath.Join(workDir, javaClassesDir), 0o700)
		},
		compileArgv: func(files []MaterializedFile, _ string) []string {
			args := []string{"javac", "-encoding", "UTF-8", "-d", sandbox.JailPath(javaClassesDir)}
			return append(args, compileUnits(files, "java")...)
		},
		artifactRel: func(entry string) string {
			// com.acme.Main -> classes/com/acme/Main.class
			return filepath.Join(javaClassesDir, filepath.FromSlash(strings.ReplaceAll(entry, ".", "/"))+".class")
		},
		// Mirror the single-file run shape (see javaSpec.run): argv[0] is replaced by
		// the resolved java launcher in execute(); the entrypoint class is last.
		runTemplate: func(entry string) []string {
			return []string{"-XX:+UseSerialGC", "-XX:-UsePerfData", "-XX:ActiveProcessorCount=1", "-Xmx{mem}m", "-cp", sandbox.JailPath(javaClassesDir), entry}
		},
	}
}

// compileUnits returns the sorted absolute in-jail paths of the files that are
// compilation units for the language (e.g. .c for C, .java for Java), so the
// compile argv is deterministic. Header/support files are materialized but not
// passed on the command line (they are found via -I / the classpath).
func compileUnits(files []MaterializedFile, language string) []string {
	p := filePolicies[language]
	out := make([]string, 0, len(files))
	for _, f := range files {
		if _, ok := p.CompileExts[strings.ToLower(path.Ext(f.Path))]; ok {
			out = append(out, jailSrc(f.Path))
		}
	}
	sort.Strings(out)
	return out
}

// shellQuote single-quotes s for safe interpolation into the Go build prelude's
// /bin/sh -c string. The value is already grammar-restricted (no quotes possible),
// but quoting is defense in depth so a future looser path can never break out.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
