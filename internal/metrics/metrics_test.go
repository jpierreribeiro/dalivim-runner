package metrics

import (
	"strings"
	"testing"
)

func TestMetrics_RunSeriesAndLabels(t *testing.T) {
	m := New()
	m.ObserveRun("python", "success", 40, 0)
	m.ObserveRun("python", "success", 120, 0)
	m.ObserveRun("c", "compile_error", 5, 900)
	m.ObserveRun("go", "timeout", 3000, 300)
	m.ObserveRun("java", "memory_exceeded", 200, 250)
	m.ObserveRun("python", "output_limit_exceeded", 15, 0)

	out := m.Render()

	// {language,status} counter, sorted and correctly counted.
	mustContain(t, out, `runner_runs_total{language="python",status="success"} 2`)
	mustContain(t, out, `runner_runs_total{language="c",status="compile_error"} 1`)
	// status-specific counters.
	mustContain(t, out, `runner_timeout_total{language="go"} 1`)
	mustContain(t, out, `runner_oom_total{language="java"} 1`)
	mustContain(t, out, `runner_output_limit_exceeded_total 1`)
	// run-duration histogram: python's 40ms + 15ms are <= the 0.05s bucket (2 obs),
	// its 120ms lands higher, so count is 3.
	mustContain(t, out, `runner_run_duration_seconds_bucket{language="python",le="0.05"} 2`)
	mustContain(t, out, `runner_run_duration_seconds_bucket{language="python",le="0.25"} 3`)
	mustContain(t, out, `runner_run_duration_seconds_count{language="python"} 3`)
	// compile-duration histogram is only emitted for runs with compileMs>0.
	mustContain(t, out, `runner_compile_duration_seconds_count{language="c"} 1`)
	if strings.Contains(out, `runner_compile_duration_seconds_count{language="python"}`) {
		t.Fatal("python has no compile phase; must not emit a compile histogram")
	}
	// TYPE/HELP headers present (valid exposition).
	mustContain(t, out, "# TYPE runner_runs_total counter")
	mustContain(t, out, "# TYPE runner_run_duration_seconds histogram")
}

func TestMetrics_OverloadAndInflight(t *testing.T) {
	m := New()
	m.IncInflight()
	m.IncInflight()
	m.DecInflight()
	m.Overload()
	m.Overload()
	out := m.Render()
	mustContain(t, out, "runner_inflight 1")
	mustContain(t, out, "runner_overload_total 2")
}

// TestMetrics_HistogramBucketing pins the le-inclusive cumulative bucketing.
func TestMetrics_HistogramBucketing(t *testing.T) {
	h := newHistogram([]float64{0.1, 1})
	for _, v := range []float64{0.05, 0.1, 0.5, 5} { // -> b0, b0, b1, +Inf
		h.observe(v)
	}
	if h.counts[0] != 2 { // <= 0.1
		t.Fatalf("bucket le=0.1 count = %d, want 2", h.counts[0])
	}
	if h.counts[1] != 1 { // (0.1, 1]
		t.Fatalf("bucket le=1 count = %d, want 1", h.counts[1])
	}
	if h.counts[2] != 1 { // +Inf overflow
		t.Fatalf("+Inf bucket count = %d, want 1", h.counts[2])
	}
}

// TestMetrics_NilSafe confirms the nil receiver is a no-op (dev / disabled).
func TestMetrics_NilSafe(t *testing.T) {
	var m *Metrics
	m.ObserveRun("python", "success", 1, 0)
	m.IncInflight()
	m.Overload()
	if m.Render() != "" {
		t.Fatal("nil metrics must render empty")
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("exposition missing %q\n---\n%s", needle, haystack)
	}
}
