package executor

import (
	"sort"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/internal/sandbox"
)

// TestService_Catalog pins G12: the discovery catalog reports each registered
// language's kind and capability flags, is sorted by id, and its id set is
// EXACTLY the dispatch registry (so adding a language can't silently omit it from
// discovery). Built with real runtimes (sandbox off) so kinds resolve; versions
// are not asserted (the toolchain may be absent in CI).
func TestService_Catalog(t *testing.T) {
	sb, err := sandbox.Configure("off", "off", "off", "")
	if err != nil {
		t.Fatalf("configure sandbox: %v", err)
	}
	svc := NewService(
		Limits{DefaultTimeout: 3000, MaxTimeoutMs: 10000, DefaultMemory: 128, MaxMemoryMB: 512},
		NewPython(sb, 64*1024, 256, 64, 4_000_000),
		NewC(sb, CompiledConfig{OutputLimit: 64 * 1024, MaxProcesses: 256, MaxFileSizeMB: 64}),
	)

	cat := svc.Catalog()
	if len(cat) != 2 {
		t.Fatalf("expected 2 languages, got %d: %+v", len(cat), cat)
	}
	// Sorted by id: "c" before "python".
	if cat[0].ID != "c" || cat[1].ID != "python" {
		t.Fatalf("catalog not sorted by id: %+v", cat)
	}

	byID := map[string]LanguageInfo{}
	for _, l := range cat {
		byID[l.ID] = l
	}
	if byID["c"].Kind != "compiled" {
		t.Fatalf("c kind = %q, want compiled", byID["c"].Kind)
	}
	if byID["python"].Kind != "interpreted" {
		t.Fatalf("python kind = %q, want interpreted", byID["python"].Kind)
	}
	for _, l := range cat {
		if !l.MultiFile {
			t.Fatalf("%s should report multifile support", l.ID)
		}
		if !l.Batch { // both interpreted and compiled runtimes implement batchRunner
			t.Fatalf("%s should report batch support", l.ID)
		}
	}

	// No drift: the catalog id set must equal the run-dispatch registry.
	langs := svc.Languages()
	sort.Strings(langs)
	if len(langs) != len(cat) {
		t.Fatalf("catalog/registry size mismatch: %d vs %d", len(cat), len(langs))
	}
	for i, l := range cat {
		if l.ID != langs[i] {
			t.Fatalf("catalog id %q != registry %q at %d", l.ID, langs[i], i)
		}
	}
}
