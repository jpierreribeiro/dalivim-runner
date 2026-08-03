package executor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

// The object-graph (trace v2 / A0) rides ALONGSIDE the v1 {repr,type} locals:
// each step gains a `heap` of typed boxes and each frame a `vars` map of
// inline-primitive-or-ref. These tests parse those additive fields and assert
// the three properties a faithful Python-Tutor diagram needs — identity/aliasing,
// cycles, and the same hostile-value discipline (never invoke __repr__) — plus
// the heap-object bound.

type graphVal struct {
	Prim string `json:"prim"`
	Text string `json:"text"`
	Ref  string `json:"ref"`
}

type graphBox struct {
	Kind      string              `json:"kind"`
	Cls       string              `json:"cls"`
	Items     []graphVal          `json:"items"`
	Entries   [][]graphVal        `json:"entries"`
	Fields    map[string]graphVal `json:"fields"`
	Truncated bool                `json:"truncated"`
}

type graphStep struct {
	Step  int `json:"step"`
	Line  int `json:"line"`
	Stack []struct {
		Func string              `json:"func"`
		Vars map[string]graphVal `json:"vars"`
	} `json:"stack"`
	Heap map[string]graphBox `json:"heap"`
}

type graphReport struct {
	Steps []graphStep `json:"steps"`
}

func decodeGraph(t *testing.T, res runnerapi.RunResult) graphReport {
	t.Helper()
	var g graphReport
	if err := json.Unmarshal([]byte(res.TraceReport), &g); err != nil {
		t.Fatalf("trace_report is not valid JSON: %v", err)
	}
	return g
}

// lastStepWithVars returns the innermost frame's vars from the last step whose
// innermost frame defines every named variable — i.e. after they are all bound.
func lastStepWithVars(g graphReport, names ...string) (map[string]graphVal, map[string]graphBox, bool) {
	for i := len(g.Steps) - 1; i >= 0; i-- {
		s := g.Steps[i]
		if len(s.Stack) == 0 {
			continue
		}
		vars := s.Stack[len(s.Stack)-1].Vars
		ok := true
		for _, n := range names {
			if _, has := vars[n]; !has {
				ok = false
				break
			}
		}
		if ok {
			return vars, s.Heap, true
		}
	}
	return nil, nil, false
}

// TestPythonTraceGraph_AliasingSharesOneBox: two names bound to the same list
// must serialize to the SAME ref id, and the box they point at grows with the
// list (identity + mutation, which a flat repr cannot express).
func TestPythonTraceGraph_AliasingSharesOneBox(t *testing.T) {
	requirePython(t)
	res, _ := runTraceMode(t, newPythonTrace(t, 2_000_000, 2500), traceReq(
		"a = [1, 2]\nb = a\na.append(3)\nc = [1, 2, 3]\n"))
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %s (stderr=%s)", res.Status, tail(res.Stderr, 200))
	}
	g := decodeGraph(t, res)
	vars, heap, ok := lastStepWithVars(g, "a", "b", "c")
	if !ok {
		t.Fatalf("no step bound a, b and c together")
	}
	if vars["a"].Ref == "" || vars["a"].Ref != vars["b"].Ref {
		t.Fatalf("a and b must share one box: a=%+v b=%+v", vars["a"], vars["b"])
	}
	if vars["c"].Ref == vars["a"].Ref {
		t.Fatalf("c is a distinct list and must NOT share a's box")
	}
	box := heap[vars["a"].Ref]
	if box.Kind != "list" || len(box.Items) != 3 {
		t.Fatalf("a's box should be the grown list [1,2,3], got %+v", box)
	}
}

// TestPythonTraceGraph_CycleIsAnArrowBackToTheSameBox: a self-referential object
// must resolve to a box whose field points at its own id, and the walk must
// terminate (no infinite expansion, no crash).
func TestPythonTraceGraph_CycleIsAnArrowBackToTheSameBox(t *testing.T) {
	requirePython(t)
	res, _ := runTraceMode(t, newPythonTrace(t, 2_000_000, 2500), traceReq(
		"class Node:\n    def __init__(self):\n        self.next = None\n"+
			"n = Node()\nn.next = n\nx = 1\n"))
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %s (stderr=%s)", res.Status, tail(res.Stderr, 200))
	}
	g := decodeGraph(t, res)
	vars, heap, ok := lastStepWithVars(g, "n", "x")
	if !ok {
		t.Fatalf("no step bound n and x together")
	}
	ref := vars["n"].Ref
	if ref == "" {
		t.Fatalf("n must be a ref, got %+v", vars["n"])
	}
	box := heap[ref]
	if box.Kind != "object" || box.Cls != "Node" {
		t.Fatalf("n should be an object box of class Node, got %+v", box)
	}
	if box.Fields["next"].Ref != ref {
		t.Fatalf("n.next must point back at n's own id %q, got %+v", ref, box.Fields["next"])
	}
}

// TestPythonTraceGraph_HostileObjectNeverInvokesRepr: an object with a throwing
// __repr__/__str__ must still serialize as a plain object box (class + read
// __dict__ fields) without the harness ever calling those methods.
func TestPythonTraceGraph_HostileObjectNeverInvokesRepr(t *testing.T) {
	requirePython(t)
	res, _ := runTraceMode(t, newPythonTrace(t, 2_000_000, 2500), traceReq(
		"class Evil:\n"+
			"    def __init__(self):\n        self.tag = 'DATA_OK'\n"+
			"    def __repr__(self):\n        raise RuntimeError('PWNED_MARKER')\n"+
			"    def __str__(self):\n        raise RuntimeError('PWNED_MARKER')\n"+
			"e = Evil()\nx = 1\n"))
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %s (stderr=%s)", res.Status, tail(res.Stderr, 200))
	}
	if strings.Contains(res.TraceReport, "PWNED_MARKER") {
		t.Fatalf("a hostile __repr__/__str__ was invoked: %s", tail(res.TraceReport, 300))
	}
	g := decodeGraph(t, res)
	vars, heap, ok := lastStepWithVars(g, "e", "x")
	if !ok {
		t.Fatalf("no step bound e and x together")
	}
	box := heap[vars["e"].Ref]
	if box.Kind != "object" || box.Cls != "Evil" {
		t.Fatalf("hostile object must be a class-labelled box, got %+v", box)
	}
	if box.Fields["tag"].Text != "'DATA_OK'" {
		t.Fatalf("structure is read from __dict__, not repr: %+v", box.Fields["tag"])
	}
}

// TestPythonTraceGraph_HeapObjectCapBounded: a program that builds far more than
// MAX_HEAP_OBJECTS distinct objects must have its per-step heap bounded, so a
// pathological graph cannot blow the report up.
func TestPythonTraceGraph_HeapObjectCapBounded(t *testing.T) {
	requirePython(t)
	res, _ := runTraceMode(t, newPythonTrace(t, 2_000_000, 2500), traceReq(
		"big = [[i] for i in range(1000)]\nx = 1\n"))
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %s (stderr=%s)", res.Status, tail(res.Stderr, 200))
	}
	g := decodeGraph(t, res)
	for _, s := range g.Steps {
		if len(s.Heap) > 200 {
			t.Fatalf("per-step heap must be capped at MAX_HEAP_OBJECTS=200, got %d at step %d", len(s.Heap), s.Step)
		}
	}
}
