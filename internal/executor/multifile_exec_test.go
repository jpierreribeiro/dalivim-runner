package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/internal/sandbox"
	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// multiFileService builds a Service (netns backend, isolation off) wiring the
// interpreted runtimes, so a test drives the FULL path — validation +
// normalization in Service.Run, then execution — exactly as the transport does.
func multiFileService(t *testing.T) *Service {
	t.Helper()
	sb, err := sandbox.Configure("off", "off", "off", "")
	if err != nil {
		t.Fatalf("configure sandbox: %v", err)
	}
	return NewService(
		Limits{
			DefaultTimeout: 8000, MaxTimeoutMs: 10000, DefaultMemory: 128, MaxMemoryMB: 512,
			Files: FileCaps{MaxFiles: 50, MaxFileBytes: 262_144, MaxFilesBytes: 1_048_576, MaxPathBytes: 180, MaxPathDepth: 8},
		},
		NewPython(sb, 64*1024, 256, 64, 4_000_000),
		NewNode(sb, 64*1024, 256, 64, 4_000_000),
		NewLua(sb, 64*1024, 256, 64, 4_000_000),
	)
}

// TestPython_MultiFileSiblingImport is the headline G3 acceptance for Python: a
// main module importing a sibling module works through the controlled runpy
// wrapper (sys.path has ONLY the source root, injected by the runner — not an
// accidental cwd entry). Runs under the netns backend, no nsjail needed.
func TestPython_MultiFileSiblingImport(t *testing.T) {
	requirePython(t)
	res, err := multiFileService(t).Run(context.Background(), runnerapi.RunRequest{
		Language: "python",
		Files: []runnerapi.RunFile{
			{Path: "main.py", Content: "from helper import greet\nprint(greet('world'))\n"},
			{Path: "helper.py", Content: "def greet(name):\n    return 'hello ' + name\n"},
		},
		TimeoutMs: 8000, MemoryMB: 128,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != runnerapi.StatusSuccess || res.Stdout != "hello world\n" {
		t.Fatalf("sibling import failed: status=%q stdout=%q stderr=%q", res.Status, res.Stdout, res.Stderr)
	}
}

// TestPython_MultiFilePackageImport exercises a package subdirectory (a dir with
// an __init__.py imported from main), proving the whole materialized tree is on
// disk and importable via the source root.
func TestPython_MultiFilePackageImport(t *testing.T) {
	requirePython(t)
	res, err := multiFileService(t).Run(context.Background(), runnerapi.RunRequest{
		Language: "python",
		Files: []runnerapi.RunFile{
			{Path: "main.py", Content: "from pkg.util import n\nprint(n)\n"},
			{Path: "pkg/__init__.py", Content: ""},
			{Path: "pkg/util.py", Content: "n = 42\n"},
		},
		TimeoutMs: 8000, MemoryMB: 128,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != runnerapi.StatusSuccess || strings.TrimSpace(res.Stdout) != "42" {
		t.Fatalf("package import failed: status=%q stdout=%q stderr=%q", res.Status, res.Stdout, res.Stderr)
	}
}

// TestPython_MultiFileEntrypoint runs a non-default entrypoint and confirms the
// wrapper executes exactly that file.
func TestPython_MultiFileEntrypoint(t *testing.T) {
	requirePython(t)
	res, err := multiFileService(t).Run(context.Background(), runnerapi.RunRequest{
		Language:   "python",
		Entrypoint: "app.py",
		Files: []runnerapi.RunFile{
			{Path: "app.py", Content: "import lib\nprint(lib.v)\n"},
			{Path: "lib.py", Content: "v = 7\n"},
		},
		TimeoutMs: 8000, MemoryMB: 128,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != runnerapi.StatusSuccess || strings.TrimSpace(res.Stdout) != "7" {
		t.Fatalf("entrypoint run failed: status=%q stdout=%q stderr=%q", res.Status, res.Stdout, res.Stderr)
	}
}

// TestNode_MultiFileSiblingRequire is the headline G3 acceptance for JavaScript: a
// main module require()-ing a sibling works because the files are materialized on
// disk and require resolves relative to the entrypoint. 8s budget (Node/V8 cold
// start under -race on contended CI).
func TestNode_MultiFileSiblingRequire(t *testing.T) {
	requireNode(t)
	res, err := multiFileService(t).Run(context.Background(), runnerapi.RunRequest{
		Language: "javascript",
		Files: []runnerapi.RunFile{
			{Path: "main.js", Content: "const {greet} = require('./helper');\nconsole.log(greet('world'));\n"},
			{Path: "helper.js", Content: "module.exports.greet = (n) => 'hello ' + n;\n"},
		},
		TimeoutMs: 8000, MemoryMB: 128,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != runnerapi.StatusSuccess || res.Stdout != "hello world\n" {
		t.Fatalf("sibling require failed: status=%q stdout=%q stderr=%q", res.Status, res.Stdout, res.Stderr)
	}
}

// TestLua_MultiFileSiblingRequire is the G3 acceptance for Lua: a main script
// require()-ing a sibling module resolves because package.path is pinned to the
// materialized source root (never a caller path), mirroring Python's runpy wrapper.
func TestLua_MultiFileSiblingRequire(t *testing.T) {
	requireLua(t)
	res, err := multiFileService(t).Run(context.Background(), runnerapi.RunRequest{
		Language: "lua",
		Files: []runnerapi.RunFile{
			{Path: "main.lua", Content: "local h = require('helper')\nprint(h.greet('world'))\n"},
			{Path: "helper.lua", Content: "local M = {}\nfunction M.greet(n) return 'hello ' .. n end\nreturn M\n"},
		},
		TimeoutMs: 8000, MemoryMB: 128,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != runnerapi.StatusSuccess || res.Stdout != "hello world\n" {
		t.Fatalf("sibling require failed: status=%q stdout=%q stderr=%q", res.Status, res.Stdout, res.Stderr)
	}
}

// TestMultiFile_SingleFileStillWorks is the regression guard: a source_code
// request through the same service is unchanged (no files[] path taken).
func TestMultiFile_SingleFileStillWorks(t *testing.T) {
	requirePython(t)
	res, err := multiFileService(t).Run(context.Background(), runnerapi.RunRequest{
		Language: "python", SourceCode: "print('single')", TimeoutMs: 8000, MemoryMB: 128,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != runnerapi.StatusSuccess || res.Stdout != "single\n" {
		t.Fatalf("single-file regression: status=%q stdout=%q", res.Status, res.Stdout)
	}
}

// TestMultiFile_ValidationErrors confirms the service rejects malformed
// submissions with a *ValidationError (the transport maps these to 400), never
// running anything.
func TestMultiFile_ValidationErrors(t *testing.T) {
	svc := multiFileService(t)
	cases := []struct {
		name string
		req  runnerapi.RunRequest
		code string
	}{
		{"both", runnerapi.RunRequest{Language: "python", SourceCode: "x", Files: []runnerapi.RunFile{{Path: "main.py", Content: "x"}}}, "both_source_and_files"},
		{"neither", runnerapi.RunRequest{Language: "python"}, "no_program"},
		{"traversal", runnerapi.RunRequest{Language: "python", Files: []runnerapi.RunFile{{Path: "../evil.py", Content: "x"}}}, "invalid_path"},
		{"forbidden", runnerapi.RunRequest{Language: "python", Files: []runnerapi.RunFile{{Path: "setup.py", Content: "x"}}}, "forbidden_file"},
		{"bad entry", runnerapi.RunRequest{Language: "python", Entrypoint: "nope.py", Files: []runnerapi.RunFile{{Path: "main.py", Content: "x"}}}, "missing_entrypoint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Run(context.Background(), tc.req)
			if code(err) != tc.code {
				t.Fatalf("expected %s, got %v", tc.code, err)
			}
		})
	}
}
