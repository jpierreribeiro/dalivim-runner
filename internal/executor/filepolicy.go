package executor

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// FilePolicy is the per-language rulebook the multi-file validator enforces
// against an attacker-controlled files[] payload. Every field is a hard bound or
// an allowlist; the policy fails CLOSED — anything a rule does not explicitly
// permit is rejected. The numeric caps come from configuration; the extension
// allowlist and the forbidden name/component sets are fixed per language and
// start deliberately restrictive (G3 adds no dependency, build-script, or
// package-manager surface).
type FilePolicy struct {
	// Language is the wire identifier this policy governs (for error text).
	Language string

	// Numeric caps (from config). Zero means "unbounded" for the byte/count caps
	// but the service always sets them, so a zero here is a misconfiguration, not
	// a bypass — the validator still applies the structural path rules.
	MaxFiles      int
	MaxFileBytes  int
	MaxFilesBytes int
	MaxPathBytes  int
	MaxPathDepth  int

	// AllowedExts is the closed set of file extensions (lower-cased, dot-prefixed)
	// permitted for this language, e.g. {".c", ".h"}. A file whose extension is
	// not present is rejected. Extensionless names (Makefile, Dockerfile) have ""
	// which is never in the set, so they are rejected too.
	AllowedExts map[string]struct{}

	// CompileExts is the subset of AllowedExts that are compilation units the
	// build step must feed to the compiler (e.g. ".c" for C; ".cpp/.cc/.cxx" for
	// C++; ".java"; ".go"). Empty for interpreted languages. At least one file
	// with a compile extension is required for a compiled language.
	CompileExts map[string]struct{}

	// ForbiddenNames is a set of exact basenames rejected even though their
	// extension is allowed — the build-system / package-manager manifests we must
	// never process (package.json, setup.py, go.mod, …). This is the defense the
	// extension allowlist alone cannot provide (setup.py has an allowed .py ext).
	ForbiddenNames map[string]struct{}

	// ForbiddenComponents is a set of exact path components rejected anywhere in a
	// path even though they satisfy the grammar (vendor, node_modules) — directories
	// that would smuggle dependencies past the offline policy.
	ForbiddenComponents map[string]struct{}

	// DefaultEntry is the entrypoint used when the request omits one: a relative
	// path (main.c, main.go, main.py, main.js, main.cpp) or, for Java, a class
	// name (Main).
	DefaultEntry string

	// EntryIsClass is true for Java only: the entrypoint is a fully-qualified
	// class name validated by javaClassRe, not a path that must exist in files[].
	EntryIsClass bool
}

// pathGrammar is the conservative whole-path allowlist derived from the plan
// (Law 5): an ASCII alphanumeric-or-underscore first char, then that plus a tiny
// set of safe punctuation. It rejects in one shot: absolute paths (leading '/'),
// a leading '.' ('.', '..', hidden files), a leading '-' or '@' (flag/argfile
// injection), backslashes, control chars, NUL, spaces, and every non-ASCII byte
// (so no Unicode bidi/confusable tricks). Per-component checks below tighten it
// further; this is the coarse first gate.
//
// Underscore is added to the plan's literal `[A-Za-z0-9._/-]` charset: real
// submissions are full of it (snake_case filenames like my_helper.py, and Python
// dunder files __init__.py / __main__.py), and '_' carries no path or shell
// meaning, so it is safe to allow, including as a leading component char.
var pathGrammar = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]*$`)

// componentGrammar validates a single path component (a slash-free segment). The
// first char must be alphanumeric-or-underscore — which alone rejects "." and
// ".." (relative traversal), "-evil" / "@args" (flag/argfile injection), and
// every dotfile (".git", ".env") wherever it appears in the path, not just at the
// root — while still admitting __init__.py. The rest is the same safe charset
// minus the slash.
var componentGrammar = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// javaClassRe validates a fully-qualified Java class entrypoint
// ("Main", "com.example.Main"): identifier segments joined by dots, each a legal
// Java identifier start + parts. It is deliberately NOT a path — Java's
// entrypoint is a class name resolved from the classpath, so it must never be
// treated as a filesystem path.
var javaClassRe = regexp.MustCompile(`^([A-Za-z_$][A-Za-z0-9_$]*)(\.[A-Za-z_$][A-Za-z0-9_$]*)*$`)

// ValidationError is a rejected-request error the transport maps to HTTP 400. It
// carries a stable machine code and a human message; the code lets a caller
// branch without string-matching the message.
type ValidationError struct {
	Code string // stable slug, e.g. "invalid_path", "too_many_files"
	Msg  string // human-readable detail
}

func (e *ValidationError) Error() string { return e.Msg }

func invalid(code, format string, a ...any) *ValidationError {
	return &ValidationError{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// filePolicies is the CLOSED per-language registry, mirroring the interpreted /
// compiled runtime registries: adding a language is a deliberate edit here, never
// request-driven. The numeric caps are filled in from config by policyFor.
var filePolicies = map[string]FilePolicy{
	"python": {
		Language:            "python",
		AllowedExts:         exts(".py"),
		ForbiddenNames:      names("setup.py", "pyproject.toml", "conftest.py"),
		ForbiddenComponents: names("__pycache__"),
		DefaultEntry:        "main.py",
	},
	"javascript": {
		Language:            "javascript",
		AllowedExts:         exts(".js", ".mjs", ".cjs", ".json"),
		ForbiddenNames:      names("package.json", "package-lock.json", "yarn.lock", "pnpm-lock.yaml"),
		ForbiddenComponents: names("node_modules"),
		DefaultEntry:        "main.js",
	},
	"lua": {
		Language:     "lua",
		AllowedExts:  exts(".lua"),
		DefaultEntry: "main.lua",
	},
	"c": {
		Language:     "c",
		AllowedExts:  exts(".c", ".h"),
		CompileExts:  exts(".c"),
		DefaultEntry: "main.c",
	},
	"cpp": {
		Language:     "cpp",
		AllowedExts:  exts(".cpp", ".cc", ".cxx", ".hpp", ".hh", ".h"),
		CompileExts:  exts(".cpp", ".cc", ".cxx"),
		DefaultEntry: "main.cpp",
	},
	"go": {
		Language:            "go",
		AllowedExts:         exts(".go"),
		CompileExts:         exts(".go"),
		ForbiddenNames:      names("go.mod", "go.sum", "go.work", "go.work.sum"),
		ForbiddenComponents: names("vendor"),
		DefaultEntry:        "main.go",
	},
	"java": {
		Language:     "java",
		AllowedExts:  exts(".java"),
		CompileExts:  exts(".java"),
		DefaultEntry: "Main",
		EntryIsClass: true,
	},
}

// testFilePolicies is the SEPARATE closed policy registry for mode=test (G9). A
// test submission is {student files + hidden test files}, so the policy must
// admit the framework's fixtures the RUN-mode policy deliberately forbids — for
// python, conftest.py (banned in run mode) — while keeping every
// manifest/dependency/component ban intact. It is a distinct map, never a
// relaxation of the run-mode policy: run mode is unchanged. A language absent
// here does not support test mode (mode=test is rejected before this is reached).
//
// Test mode has no single entrypoint (the framework DISCOVERS tests), so
// DefaultEntry/EntryIsClass are unused; validateTestFiles skips entrypoint
// resolution entirely.
var testFilePolicies = map[string]FilePolicy{
	"python": {
		Language:    "python",
		AllowedExts: exts(".py"),
		// conftest.py is now ADMITTED (pytest's fixture/hook file) — the whole point
		// of the test policy. setup.py / pyproject.toml stay banned: a test run must
		// never process a build/dependency manifest.
		ForbiddenNames:      names("setup.py", "pyproject.toml"),
		ForbiddenComponents: names("__pycache__"),
	},
	"go": {
		Language:    "go",
		AllowedExts: exts(".go"),
		// A test submission is .go files (student + *_test.go). At least one .go is
		// required (CompileExts). go.mod is FORBIDDEN — the runner synthesizes the
		// module itself (a caller-supplied module file is never processed); vendor is
		// forbidden like run mode. Same bans as the run policy, minus the entrypoint.
		CompileExts:         exts(".go"),
		ForbiddenNames:      names("go.mod", "go.sum", "go.work", "go.work.sum"),
		ForbiddenComponents: names("vendor"),
	},
	"javascript": {
		Language:    "javascript",
		AllowedExts: exts(".js", ".mjs", ".cjs", ".json"),
		// A JS test submission is {student modules + hidden *.test.js files} — all
		// plain .js, so the run-mode extension allowlist already fits. Same
		// manifest/lockfile bans (a test run must never process a package manifest),
		// and node_modules stays forbidden (no dependency fetch/vendor — node's
		// built-in --test runner needs none). Minus the entrypoint: node --test
		// DISCOVERS its tests.
		ForbiddenNames:      names("package.json", "package-lock.json", "yarn.lock", "pnpm-lock.yaml"),
		ForbiddenComponents: names("node_modules"),
	},
}

// policyFor returns the per-language FilePolicy with the configured numeric caps
// applied, and whether the language is known.
func policyFor(language string, caps FileCaps) (FilePolicy, bool) {
	return policyForMode(language, modeRun, caps)
}

// policyForMode returns the per-language FilePolicy for the given execution mode
// (G9) with the configured numeric caps applied, and whether the language is
// known for that mode. Run mode reads filePolicies; test mode reads the separate
// testFilePolicies, so a test submission's fixtures are validated against the
// test allowlist and a run submission is completely unaffected.
func policyForMode(language, mode string, caps FileCaps) (FilePolicy, bool) {
	registry := filePolicies
	if mode == modeTest {
		registry = testFilePolicies
	}
	p, ok := registry[language]
	if !ok {
		return FilePolicy{}, false
	}
	p.MaxFiles = caps.MaxFiles
	p.MaxFileBytes = caps.MaxFileBytes
	p.MaxFilesBytes = caps.MaxFilesBytes
	p.MaxPathBytes = caps.MaxPathBytes
	p.MaxPathDepth = caps.MaxPathDepth
	return p, true
}

// FileCaps are the configured numeric bounds threaded from config into every
// per-language policy (the per-language allowlists are fixed in code).
type FileCaps struct {
	MaxFiles      int
	MaxFileBytes  int
	MaxFilesBytes int
	MaxPathBytes  int
	MaxPathDepth  int
}

// validateFiles enforces the whole file-policy against a request's files[] and
// returns the canonical, path-sorted file list plus the resolved entrypoint. It
// performs NO disk I/O — it is pure request validation, so the transport can
// reject a bad request (400) long before any materialization.
//
// It layers entrypoint resolution over validatePaths (the on-disk-safety core,
// which materialization re-runs independently). It assumes req.Files is non-empty
// (the caller routes single-file source_code separately).
func validateFiles(req runnerapi.RunRequest, p FilePolicy) ([]runnerapi.RunFile, string, error) {
	out, seen, err := validatePaths(req.Files, p)
	if err != nil {
		return nil, "", err
	}
	entry, err := resolveEntrypoint(req.Entrypoint, seen, p)
	if err != nil {
		return nil, "", err
	}
	return out, entry, nil
}

// validateTestFiles is the mode=test counterpart of validateFiles (G9): it runs
// the identical path-safety core (grammar, caps, extension/name allowlists,
// duplicate rejection) against the test policy, but resolves NO entrypoint — a
// test suite has no single main; the framework discovers its own tests. It
// returns the canonical, path-sorted file list. No disk I/O; materialization
// re-runs validatePaths over the same input against the same (test) policy.
func validateTestFiles(req runnerapi.RunRequest, p FilePolicy) ([]runnerapi.RunFile, error) {
	out, _, err := validatePaths(req.Files, p)
	return out, err
}

// validatePaths is the path-safety core: count/size caps, the full path grammar
// and per-component rules, the extension/name allowlists, duplicate rejection,
// and the "at least one compile unit" rule for compiled languages. It returns the
// canonical path-sorted file list and the set of normalized paths (for
// entrypoint existence checks). No disk I/O — materialization re-runs exactly
// this over the same input as defense in depth, so it never trusts a prior call.
func validatePaths(files []runnerapi.RunFile, p FilePolicy) ([]runnerapi.RunFile, map[string]struct{}, error) {
	if len(files) == 0 {
		return nil, nil, invalid("no_files", "files[] must contain at least one file")
	}
	if p.MaxFiles > 0 && len(files) > p.MaxFiles {
		return nil, nil, invalid("too_many_files", "too many files: %d (max %d)", len(files), p.MaxFiles)
	}

	seen := make(map[string]struct{}, len(files))
	total := 0
	hasCompileUnit := len(p.CompileExts) == 0 // interpreted languages need no compile unit
	for _, f := range files {
		if p.MaxFileBytes > 0 && len(f.Content) > p.MaxFileBytes {
			return nil, nil, invalid("file_too_large", "file %q is %d bytes (max %d)", truncPath(f.Path), len(f.Content), p.MaxFileBytes)
		}
		total += len(f.Content)
		if p.MaxFilesBytes > 0 && total > p.MaxFilesBytes {
			return nil, nil, invalid("files_too_large", "total file content exceeds %d bytes", p.MaxFilesBytes)
		}
		clean, err := validatePath(f.Path, p)
		if err != nil {
			return nil, nil, err
		}
		if _, dup := seen[clean]; dup {
			return nil, nil, invalid("duplicate_path", "duplicate path after normalization: %q", clean)
		}
		seen[clean] = struct{}{}
		if _, ok := p.CompileExts[strings.ToLower(path.Ext(clean))]; ok {
			hasCompileUnit = true
		}
	}
	if !hasCompileUnit {
		return nil, nil, invalid("no_compile_unit", "no compilable source file for language %q", p.Language)
	}

	// Canonical, path-sorted copy so materialization and the compile argv are
	// deterministic regardless of request order.
	out := make([]runnerapi.RunFile, len(files))
	copy(out, files)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, seen, nil
}

// validatePath applies the full path grammar and per-component rules to one
// caller-supplied path, returning the normalized (slash-cleaned) form. Every
// return is fail-closed: an unrecognized shape is rejected, never "fixed".
func validatePath(raw string, p FilePolicy) (string, error) {
	if raw == "" {
		return "", invalid("invalid_path", "empty path")
	}
	if p.MaxPathBytes > 0 && len(raw) > p.MaxPathBytes {
		return "", invalid("invalid_path", "path exceeds %d bytes: %q", p.MaxPathBytes, truncPath(raw))
	}
	// The coarse whole-path grammar rejects absolute paths, backslashes, control
	// chars, NUL, spaces, and all non-ASCII in one pass.
	if !pathGrammar.MatchString(raw) {
		return "", invalid("invalid_path", "path fails the allowed grammar: %q", truncPath(raw))
	}
	// path.Clean must be a no-op: any "." / ".." / "//" component would change it,
	// and the per-component check below also rejects them — belt and suspenders so
	// a normalization discrepancy can never slip through.
	clean := path.Clean(raw)
	if clean != raw {
		return "", invalid("invalid_path", "path is not in normal form: %q", truncPath(raw))
	}
	comps := strings.Split(clean, "/")
	if p.MaxPathDepth > 0 && len(comps) > p.MaxPathDepth {
		return "", invalid("invalid_path", "path is too deep (%d, max %d): %q", len(comps), p.MaxPathDepth, truncPath(raw))
	}
	for _, c := range comps {
		if c == "" {
			return "", invalid("invalid_path", "empty path component in %q", truncPath(raw))
		}
		if !componentGrammar.MatchString(c) {
			return "", invalid("invalid_path", "illegal path component %q in %q", c, truncPath(raw))
		}
		if _, bad := p.ForbiddenComponents[c]; bad {
			return "", invalid("forbidden_path", "forbidden path component %q", c)
		}
	}

	base := comps[len(comps)-1]
	if _, bad := p.ForbiddenNames[base]; bad {
		return "", invalid("forbidden_file", "forbidden file %q for language %q", base, p.Language)
	}
	if _, ok := p.AllowedExts[strings.ToLower(path.Ext(base))]; !ok {
		return "", invalid("forbidden_extension", "file %q has a disallowed extension for language %q", base, p.Language)
	}
	return clean, nil
}

// resolveEntrypoint fills the default entrypoint when omitted and validates it.
// For a path-based language the entrypoint must be one of the (already
// normalized) submitted files; for Java it must be a syntactically valid class
// name and is NOT looked up in files[].
func resolveEntrypoint(entry string, files map[string]struct{}, p FilePolicy) (string, error) {
	if entry == "" {
		entry = p.DefaultEntry
	}
	if p.EntryIsClass {
		if !javaClassRe.MatchString(entry) {
			return "", invalid("invalid_entrypoint", "entrypoint %q is not a valid Java class name", entry)
		}
		return entry, nil
	}
	// Path entrypoint: validate its shape the same way, then require it to exist.
	clean, err := validatePath(entry, p)
	if err != nil {
		return "", invalid("invalid_entrypoint", "entrypoint %q: %s", entry, err.Error())
	}
	if _, ok := files[clean]; !ok {
		return "", invalid("missing_entrypoint", "entrypoint %q is not among the submitted files", entry)
	}
	return clean, nil
}

// truncPath bounds a path in an error message so a hostile 180-byte path cannot
// bloat logs; the grammar already forbids control chars, so the value is safe to
// echo.
func truncPath(s string) string {
	const max = 80
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// exts builds a set from dot-prefixed extensions.
func exts(list ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(list))
	for _, e := range list {
		m[e] = struct{}{}
	}
	return m
}

// names builds a set of exact basenames / components.
func names(list ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(list))
	for _, n := range list {
		m[n] = struct{}{}
	}
	return m
}
