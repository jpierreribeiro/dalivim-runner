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
	Heap      map[string]graphBox `json:"heap"`
	HeapDelta *struct {
		Set map[string]graphBox `json:"set"`
		Del []string            `json:"del"`
	} `json:"heap_delta"`
}

type graphReport struct {
	Steps        []graphStep `json:"steps"`
	HeapEncoding string      `json:"heap_encoding"`
}

// heapsOf rebuilds the full per-step heap from the wire form. With
// heap_encoding="delta" the first graph step carries the whole heap and each
// later step carries only {set,del} — or nothing at all, when the heap did not
// change. Every consumer has to do this, so the tests do it the same way the
// frontend does: what they assert on is what a client actually sees.
func heapsOf(g graphReport) []map[string]graphBox {
	out := make([]map[string]graphBox, len(g.Steps))
	var cur map[string]graphBox
	for i, s := range g.Steps {
		switch {
		case s.Heap != nil:
			cur = s.Heap
		case s.HeapDelta != nil:
			next := make(map[string]graphBox, len(cur))
			for k, v := range cur {
				next[k] = v
			}
			for k, v := range s.HeapDelta.Set {
				next[k] = v
			}
			for _, k := range s.HeapDelta.Del {
				delete(next, k)
			}
			cur = next
		case g.HeapEncoding != "delta":
			cur = nil // v1 step: no graph at all
		}
		out[i] = cur
	}
	return out
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
	heaps := heapsOf(g)
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
			return vars, heaps[i], true
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

// TestPythonTraceGraph_BuiltinSubclassIsNotStudentCode is the sibling of the
// hostile-__repr__ test, for the case that test does NOT reach: a subclass of a
// BUILT-IN. Every branch of safe_repr/_expand_object dispatches on `isinstance`,
// so `class Loud(int)` took the "safe" scalar path and the harness called the
// student's own `__str__` — the exact thing the file's header rules out.
//
// Measured before the fix, with this program: `LOG` held SIX entries by the time
// `n = len(LOG)` ran (the student's methods ran six times without the student
// calling them once), `a` was DRAWN AS 42 while holding 5, and a `__str__`
// returning 200 MB was materialised in full before _clip could cut it — an OOM
// under the cgroup for a program that runs fine in run mode.
//
// The fix is to render through the BUILT-IN's unbound slot (int.__repr__,
// list.__iter__, dict.items), which a subclass cannot override — safe AND still
// truthful, so the value keeps showing.
func TestPythonTraceGraph_BuiltinSubclassIsNotStudentCode(t *testing.T) {
	requirePython(t)
	res, _ := runTraceMode(t, newPythonTrace(t, 2_000_000, 2500), traceReq(
		"LOG = []\n"+
			"class Loud(int):\n"+
			"    def __str__(self):\n        LOG.append('PWNED_MARKER'); return '42'\n"+
			"    def __repr__(self):\n        LOG.append('PWNED_MARKER'); return '42'\n"+
			"class LoudList(list):\n"+
			"    def __iter__(self):\n        LOG.append('PWNED_MARKER'); return super().__iter__()\n"+
			"class LoudDict(dict):\n"+
			"    def items(self):\n        LOG.append('PWNED_MARKER'); return super().items()\n"+
			"a = Loud(5)\nb = LoudList([1, 2])\nd = LoudDict(x=1)\nn = len(LOG)\nz = 1\n"))
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %s (stderr=%s)", res.Status, tail(res.Stderr, 200))
	}
	if strings.Contains(res.TraceReport, "PWNED_MARKER") {
		t.Fatalf("a subclass's __str__/__iter__/items was invoked: %s", tail(res.TraceReport, 300))
	}
	g := decodeGraph(t, res)
	vars, heap, ok := lastStepWithVars(g, "a", "b", "d", "n")
	if !ok {
		t.Fatal("no step bound a, b, d and n together")
	}
	// The student's own count is the authoritative witness: the program appends to
	// LOG on every override call, so `n` is how many times the harness ran code it
	// promised never to run.
	if vars["n"].Text != "0" {
		t.Fatalf("the harness invoked student code %s time(s) while recording", vars["n"].Text)
	}
	// And the value must still be the TRUE one. Rendering `<Loud>` would also be
	// safe, but it would hide the number — going through int.__repr__ keeps both.
	if vars["a"].Text != "5" {
		t.Fatalf("Loud(5) must render as its real value 5, got %+v", vars["a"])
	}
	if box := heap[vars["b"].Ref]; box.Kind != "list" || len(box.Items) != 2 {
		t.Fatalf("a list subclass must still show its real cells, got %+v", box)
	}
	if box := heap[vars["d"].Ref]; box.Kind != "dict" || len(box.Entries) != 1 {
		t.Fatalf("a dict subclass must still show its real pairs, got %+v", box)
	}
}

// TestPythonTraceGraph_HeapCapIsReported: bater no teto de caixas não corta o
// TRACE, corta o DESENHO — a referência que sobra não alcança nada, e o aluno lê
// isso como uma variável sem valor. Era o único teto sem forma de chegar até ele:
// as caixas eram descartadas em silêncio.
func TestPythonTraceGraph_HeapCapIsReported(t *testing.T) {
	requirePython(t)
	// 50 variáveis × 30 listas cada: bem acima de MAX_HEAP_OBJECTS=200. (Um
	// `[[i] for i in range(1000)]` NÃO serve: o teto de itens por contêiner corta
	// em 30, e a caixa 201 nunca é pedida.)
	res, _ := runTraceMode(t, newPythonTrace(t, 2_000_000, 2500), traceReq(
		"for _i in range(50):\n    globals()['v%d' % _i] = [[j] for j in range(30)]\nx = 1\n"))
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %s (stderr=%s)", res.Status, tail(res.Stderr, 200))
	}
	var doc struct {
		Truncated struct {
			Heap bool `json:"heap"`
		} `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(res.TraceReport), &doc); err != nil {
		t.Fatalf("trace_report is not valid JSON: %v", err)
	}
	if !doc.Truncated.Heap {
		t.Fatal("hitting the per-step box cap must be reported in truncated.heap")
	}
	// E o teto continua valendo sobre o heap RECONSTRUÍDO.
	for i, h := range heapsOf(decodeGraph(t, res)) {
		if len(h) > 200 {
			t.Fatalf("step %d: %d boxes, cap is 200", i, len(h))
		}
	}
}

// E o inverso: um programa pequeno não pode acusar truncagem que não houve.
func TestPythonTraceGraph_HeapCapNotReportedWhenItFits(t *testing.T) {
	requirePython(t)
	res, _ := runTraceMode(t, newPythonTrace(t, 2_000_000, 2500), traceReq(
		"a = [1, 2]\nb = a\nprint(len(a))\n"))
	if strings.Contains(res.TraceReport, `"heap":true`) {
		t.Fatalf("a trace that fits must not claim the box cap was hit: %s", tail(res.TraceReport, 200))
	}
}

// TestTraceHarness_BuiltinSlotDispatch pins the RULE, not just one program: no
// branch may reach a value's own method. A future branch added with `v.items()`
// or `for x in v` would reopen the hole above without failing the test on the
// programs we happened to think of.
func TestTraceHarness_BuiltinSlotDispatch(t *testing.T) {
	for _, want := range []string{
		"int.__repr__(v)", "float.__repr__(v)", "str.__getitem__(v",
		"def _itens_base", "def _pares_base", "return dict.items(v)", "base.__iter__(v)",
	} {
		if !strings.Contains(traceHarnessPython, want) {
			t.Fatalf("a built-in's value must be read through its own slot (%s missing)", want)
		}
	}
	// The two expansion sites must go through the helpers, never through the
	// instance. `enumerate(v)` on a list subclass IS a call into student code.
	for _, proibido := range []string{"enumerate(v)", "enumerate(v.items())"} {
		if strings.Contains(traceHarnessPython, proibido) {
			t.Fatalf("%q dispatches on the instance and reaches an overridden method", proibido)
		}
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
	// The bound is on the RECONSTRUCTED heap: with deltas the cap could otherwise
	// be trivially satisfied per step while the accumulated heap grew unbounded.
	for i, h := range heapsOf(g) {
		if len(h) > 200 {
			t.Fatalf("per-step heap must be capped at MAX_HEAP_OBJECTS=200, got %d at step %d", len(h), i)
		}
	}
}

// TestTraceHarness_FunctionBox pins the closure diagram (A2). A function used to
// fall through to `opaque` — a grey box with nothing in it — which made the one
// topic the tutor most exists to explain invisible: `contador()` returning
// `incr` is unreadable unless the box shows WHAT incr remembers.
func TestTraceHarness_FunctionBox(t *testing.T) {
	if !strings.Contains(traceHarnessPython, `"kind": "function"`) {
		t.Fatal("the harness must emit a function box, not an opaque leaf")
	}
	if !strings.Contains(traceHarnessPython, "co_freevars") || !strings.Contains(traceHarnessPython, "cell_contents") {
		t.Fatal("a function box must carry the CAPTURED names — that is what makes a closure legible")
	}
	// Exact type check: reading __name__/__closure__ off an arbitrary object
	// would be calling into student code, which this harness never does.
	if !strings.Contains(traceHarnessPython, "type(v) is _FunctionType") {
		t.Fatal("function detection must be an exact type check, never duck-typing on student attributes")
	}
	// A captured-but-unbound cell is a real state, not an error to swallow.
	if !strings.Contains(traceHarnessPython, "ainda não definido") {
		t.Fatal("an empty closure cell must render as itself")
	}
	// The v1 variables table keeps its old filtering; only the graph gains the
	// student's own functions, and only those defined in the traced file.
	if !strings.Contains(traceHarnessPython, "keep_student_functions") {
		t.Fatal("only the graph may keep student functions at module level")
	}
	if !strings.Contains(traceHarnessPython, "co_filename != TARGET") {
		t.Fatal("an IMPORTED function must stay filtered — only the student's own is drawn")
	}
}

// TestTraceHarness_ReturnValue pins the "Return value" row. On a return event
// `arg` IS the value the frame handed back, and it was being discarded — the
// student watched a function finish and never saw its result, which is the half
// of a call that stepping exists to make concrete.
func TestTraceHarness_ReturnValue(t *testing.T) {
	if !strings.Contains(traceHarnessPython, `event == "return"`) || !strings.Contains(traceHarnessPython, `step["retval"]`) {
		t.Fatal("the harness must record the return value on a return event")
	}
	// It goes through the SAME bounded serializer as any other value: a huge or
	// cyclic return must not get a private, uncapped path.
	if !strings.Contains(traceHarnessPython, `step["retval"] = _value_ref(arg, idmap, heap, queue)`) {
		t.Fatal("the return value must use the bounded graph serializer, not a raw repr")
	}
}

// TestPythonTraceGraph_ObjectIdsStableAcrossSteps: an object's box id must not
// change while the object lives. The id map used to be rebuilt EVERY step, so it
// was dense per step: in this program `del a` renumbered every survivor (b went
// from id 2 to id 1), which silently re-pointed the diagram's boxes and made any
// step-to-step diff meaningless. Identity is the property the diagram exists to
// teach, so it is asserted directly.
func TestPythonTraceGraph_ObjectIdsStableAcrossSteps(t *testing.T) {
	requirePython(t)
	res, _ := runTraceMode(t, newPythonTrace(t, 2_000_000, 2500), traceReq(
		"a = [1]\nb = [2]\nc = [3]\ndel a\nb.append(9)\n"))
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %s (stderr=%s)", res.Status, tail(res.Stderr, 200))
	}
	g := decodeGraph(t, res)

	// b's id, taken the first time b exists, must be the same at the last step.
	idOf := func(step graphStep, name string) string {
		if len(step.Stack) == 0 {
			return ""
		}
		return step.Stack[len(step.Stack)-1].Vars[name].Ref
	}
	primeiro, ultimo := "", ""
	for _, s := range g.Steps {
		if id := idOf(s, "b"); id != "" {
			if primeiro == "" {
				primeiro = id
			}
			ultimo = id
		}
	}
	if primeiro == "" {
		t.Fatal("b never appeared in the graph vars")
	}
	if primeiro != ultimo {
		t.Fatalf("b's box id changed across steps (%s → %s): identity is not preserved", primeiro, ultimo)
	}
	// And c, declared after b, must keep an id distinct from b's throughout.
	for _, s := range g.Steps {
		if idc := idOf(s, "c"); idc != "" && idc == ultimo {
			t.Fatalf("c collided with b's id %s", idc)
		}
	}
}

// TestPythonTraceGraph_HeapTravelsAsDelta: the heap must NOT be repeated in full
// on every step. Measured before this change, a 120-iteration loop spent 58% of
// the report on the heap with 86% of steps repeating the previous one byte for
// byte — which is what pushes an ordinary trace into the 2 MB cap. The delta must
// still reconstruct to exactly what the full form would have said.
func TestPythonTraceGraph_HeapTravelsAsDelta(t *testing.T) {
	requirePython(t)
	res, _ := runTraceMode(t, newPythonTrace(t, 2_000_000, 2500), traceReq(
		"acc = []\nfor i in range(20):\n    acc.append(i)\n"))
	if res.Status != runnerapi.StatusSuccess {
		t.Fatalf("expected success, got %s (stderr=%s)", res.Status, tail(res.Stderr, 200))
	}
	g := decodeGraph(t, res)
	if g.HeapEncoding != "delta" {
		t.Fatalf("heap_encoding must announce the delta form, got %q", g.HeapEncoding)
	}
	cheios := 0
	for _, s := range g.Steps {
		if s.Heap != nil {
			cheios++
		}
	}
	if cheios != 1 {
		t.Fatalf("exactly one step may carry the full heap, got %d", cheios)
	}

	// The reconstruction must show the list growing one cell per iteration.
	heaps := heapsOf(g)
	var maior int
	for i, s := range g.Steps {
		if len(s.Stack) == 0 {
			continue
		}
		ref := s.Stack[len(s.Stack)-1].Vars["acc"].Ref
		if ref == "" {
			continue
		}
		box, ok := heaps[i][ref]
		if !ok {
			t.Fatalf("step %d: acc points at %s but the rebuilt heap has no such box", i, ref)
		}
		if len(box.Items) < maior {
			t.Fatalf("step %d: acc shrank (%d → %d); the delta lost a mutation", i, maior, len(box.Items))
		}
		maior = len(box.Items)
	}
	// 20 stays under MAX_ITEMS=30, so the box is complete and the count is exact.
	if maior != 20 {
		t.Fatalf("acc should have ended with 20 items, rebuilt %d", maior)
	}
}
