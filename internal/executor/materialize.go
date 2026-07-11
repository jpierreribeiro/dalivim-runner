package executor

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// MaterializedFile is one file written to disk, with its in-jail-relative path
// (forward slashes) and byte size. The compile/run flows use these to build the
// argv (e.g. the sorted list of translation units for gcc).
type MaterializedFile struct {
	Path string // normalized, relative, forward-slash (e.g. "src/util.c")
	Size int
}

// MaterializeFiles writes a validated files[] payload into root using a
// TRAVERSAL-RESISTANT primitive (os.Root, backed by openat2 with RESOLVE_BENEATH
// on Linux), so a malicious path can never write, follow a symlink, or create a
// parent outside root — even against a hostile pre-existing tree. This is the
// highest-severity step in the codebase (the request path is attacker-controlled
// and the write happens on the host before the jail execs), so it fails CLOSED
// at every stage.
//
// The algorithm follows the plan: re-validate (defense in depth over the
// transport's fail-fast check), then for each file — create parent dirs beneath
// root, create the file with O_CREATE|O_EXCL (never follow or clobber an
// existing object), write, and lstat-verify it is a regular file owned by us.
// root must already exist and be an empty directory the runner owns.
func MaterializeFiles(root string, files []runnerapi.RunFile, p FilePolicy) ([]MaterializedFile, error) {
	// Re-validate independently of the transport: MaterializeFiles must be safe to
	// call on its own, so it never trusts that validatePaths already ran. It
	// re-checks the on-disk-safety rules (paths, sizes, extensions), not
	// entrypoint semantics — that is the transport's concern.
	clean, _, err := validatePaths(files, p)
	if err != nil {
		return nil, err
	}

	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open job root: %w", err)
	}
	defer r.Close()

	out := make([]MaterializedFile, 0, len(clean))
	for _, f := range clean {
		rel := path.Clean(f.Path) // already normalized by validateFiles
		if dir := path.Dir(rel); dir != "." {
			// os.Root.MkdirAll refuses to traverse a symlink or escape root; each
			// parent is created by the runner, never adopted from a supplied symlink.
			if err := r.MkdirAll(filepath.FromSlash(dir), 0o700); err != nil {
				return nil, fmt.Errorf("mkdir %q: %w", dir, err)
			}
		}
		// O_EXCL is the anti-clobber guarantee: if anything already exists at this
		// path (a symlink, hardlink, FIFO, socket, device, or a duplicate), the
		// create fails rather than following or overwriting it.
		fh, err := r.OpenFile(filepath.FromSlash(rel), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create %q: %w", rel, err)
		}
		if _, err := fh.WriteString(f.Content); err != nil {
			_ = fh.Close()
			return nil, fmt.Errorf("write %q: %w", rel, err)
		}
		if err := fh.Close(); err != nil {
			return nil, fmt.Errorf("close %q: %w", rel, err)
		}
		// Post-write verification: the object we just created must be a regular
		// file owned by the runner uid. lstat (not stat) so a symlink is caught
		// rather than followed. Under os.Root this is belt-and-suspenders, but the
		// plan mandates it as the final fail-closed check.
		if err := verifyRegularOwned(r, rel); err != nil {
			return nil, err
		}
		out = append(out, MaterializedFile{Path: rel, Size: len(f.Content)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// verifyRegularOwned lstat-checks that rel under r is a regular file owned by the
// current uid. Fails closed on anything else (symlink, special file, wrong
// owner).
func verifyRegularOwned(r *os.Root, rel string) error {
	fi, err := r.Lstat(filepath.FromSlash(rel))
	if err != nil {
		return fmt.Errorf("lstat %q: %w", rel, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("materialized %q is not a regular file (mode %v)", rel, fi.Mode())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if int(st.Uid) != os.Getuid() {
			return fmt.Errorf("materialized %q is not owned by the runner uid", rel)
		}
	}
	return nil
}

// materializeSource creates the per-run source root (workDir/src) and writes the
// request's validated files[] into it with the traversal-resistant API. The base
// per-language policy (allowlists) is used for the defense-in-depth re-check; the
// numeric caps were already enforced by the service, so their absence here cannot
// widen anything (the grammar/extension checks are cap-independent).
func materializeSource(workDir string, req runnerapi.RunRequest) ([]MaterializedFile, error) {
	// The defense-in-depth re-check uses the policy for the request's MODE (G9): a
	// mode=test tree is validated against the test allowlist (which admits
	// conftest.py), a run tree against the run allowlist — never the wrong one, so
	// materialization can never widen or narrow what the service already accepted.
	registry := filePolicies
	if req.Mode == modeTest {
		registry = testFilePolicies
	}
	p, ok := registry[req.Language]
	if !ok {
		return nil, fmt.Errorf("no file policy for language %q", req.Language)
	}
	srcRoot := filepath.Join(workDir, srcRootName)
	if err := os.MkdirAll(srcRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create source root: %w", err)
	}
	return MaterializeFiles(srcRoot, req.Files, p)
}

// srcRootName is the subdirectory of the per-run workdir that holds materialized
// student source, i.e. the in-jail /sandbox/src tree. Keeping source under a
// named subdir (not the workdir root) separates it from compiler output
// (/sandbox/bin, /sandbox/classes) the build step writes alongside it.
const srcRootName = "src"

// jailSrc returns the in-jail ABSOLUTE path of a materialized file, e.g.
// jailSrc("util/util.c") -> "/sandbox/src/util/util.c". Compiled languages use
// this: their build and run happen in TWO separate jails that must agree on one
// fixed path, so relative-to-cwd would be ambiguous. The component is already
// validated (grammar-restricted, no leading '-'/'@'), so it is safe as an argv
// element.
func jailSrc(rel string) string {
	return strings.TrimRight(srcJailDir(), "/") + "/" + rel
}

// srcJailDir is the in-jail source root, /sandbox/src.
func srcJailDir() string { return "/sandbox/" + srcRootName }

// srcRel returns the path of a materialized file RELATIVE to the per-run working
// directory, e.g. srcRel("main.py") -> "src/main.py". Interpreted languages use
// this: they run in a single jail whose cwd is the working directory root
// (/sandbox under nsjail, the workdir under the netns backend), so a cwd-relative
// path resolves correctly under either backend — which also makes multi-file
// interpreted runs unit-testable without nsjail.
func srcRel(rel string) string { return srcRootName + "/" + rel }
