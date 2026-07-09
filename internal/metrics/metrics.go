// Package metrics is a tiny, dependency-free Prometheus exposition for the
// runner's bounded metric set. It deliberately does NOT pull in
// prometheus/client_golang and its transitive tree — the runner is proud of its
// zero external dependencies and small attack surface, and the handful of series
// here (all with bounded {language,status} labels) is cheap to render by hand.
//
// Label safety is the point: only the closed language and status sets ever become
// labels. Student code, stdin, stdout, tokens, and other free-form strings NEVER
// touch a label (unbounded cardinality + PII).
package metrics

import (
	"sort"
	"strconv"
	"strings"
	"sync"
)

// runBuckets/compileBuckets are the histogram upper bounds in seconds. Runs are
// capped near the max wall timeout (~10s); compiles near the compile ceiling (~20s).
var (
	runBuckets     = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	compileBuckets = []float64{0.1, 0.5, 1, 2.5, 5, 10, 20}
)

// Metrics holds the runner's counters, gauges, and histograms. All methods are
// safe for concurrent use. The zero value is not usable — call New.
type Metrics struct {
	mu sync.Mutex

	runsTotal     map[[2]string]int64 // {language,status} -> count
	oomTotal      map[string]int64    // language -> count
	timeoutTotal  map[string]int64    // language -> count
	outputLimit   int64               // output_limit_exceeded runs (G1.4)
	overloadTotal int64               // 503 load-sheds
	inflight      int64               // current concurrency (gauge)

	runDur     map[string]*histogram // language -> run wall-time histogram
	compileDur map[string]*histogram // language -> compile-time histogram
}

// New builds an empty registry.
func New() *Metrics {
	return &Metrics{
		runsTotal:    map[[2]string]int64{},
		oomTotal:     map[string]int64{},
		timeoutTotal: map[string]int64{},
		runDur:       map[string]*histogram{},
		compileDur:   map[string]*histogram{},
	}
}

// ObserveRun records one completed run: the {language,status} counter, the run
// wall-time histogram, and the status-specific counters. durationMs/compileMs are
// milliseconds; compileMs<=0 (interpreted, or no compile) is skipped.
func (m *Metrics) ObserveRun(language, status string, durationMs, compileMs int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	m.runsTotal[[2]string{language, status}]++
	m.hist(m.runDur, language, runBuckets).observe(float64(durationMs) / 1000)
	if compileMs > 0 {
		m.hist(m.compileDur, language, compileBuckets).observe(float64(compileMs) / 1000)
	}
	switch status {
	case "memory_exceeded":
		m.oomTotal[language]++
	case "timeout":
		m.timeoutTotal[language]++
	case "output_limit_exceeded":
		m.outputLimit++
	}
}

// Overload records a 503 load-shed (the concurrency limiter rejected a run).
func (m *Metrics) Overload() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.overloadTotal++
	m.mu.Unlock()
}

// IncInflight / DecInflight move the concurrency gauge around an accepted run.
func (m *Metrics) IncInflight() { m.addInflight(1) }
func (m *Metrics) DecInflight() { m.addInflight(-1) }

func (m *Metrics) addInflight(d int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.inflight += d
	m.mu.Unlock()
}

// hist returns the per-language histogram, creating it on first use. Caller holds mu.
func (m *Metrics) hist(into map[string]*histogram, language string, buckets []float64) *histogram {
	h := into[language]
	if h == nil {
		h = newHistogram(buckets)
		into[language] = h
	}
	return h
}

// Render writes the current metrics in Prometheus text exposition format.
func (m *Metrics) Render() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	var b strings.Builder

	writeCounterHead(&b, "runner_runs_total", "Total runs by language and terminal status.")
	for _, k := range sortedPairKeys(m.runsTotal) {
		b.WriteString("runner_runs_total{language=" + quote(k[0]) + ",status=" + quote(k[1]) + "} ")
		b.WriteString(strconv.FormatInt(m.runsTotal[k], 10) + "\n")
	}

	writeLangCounter(&b, "runner_oom_total", "Runs ended by memory_exceeded, by language.", m.oomTotal)
	writeLangCounter(&b, "runner_timeout_total", "Runs ended by timeout, by language.", m.timeoutTotal)

	writeCounterHead(&b, "runner_output_limit_exceeded_total", "Runs killed for flooding output (G1.4).")
	b.WriteString("runner_output_limit_exceeded_total " + strconv.FormatInt(m.outputLimit, 10) + "\n")

	writeCounterHead(&b, "runner_overload_total", "Requests shed with 503 by the concurrency limiter.")
	b.WriteString("runner_overload_total " + strconv.FormatInt(m.overloadTotal, 10) + "\n")

	b.WriteString("# HELP runner_inflight Runs currently executing.\n# TYPE runner_inflight gauge\n")
	b.WriteString("runner_inflight " + strconv.FormatInt(m.inflight, 10) + "\n")

	writeHistograms(&b, "runner_run_duration_seconds", "Run wall time in seconds, by language.", m.runDur)
	writeHistograms(&b, "runner_compile_duration_seconds", "Compile phase time in seconds, by language.", m.compileDur)

	return b.String()
}

func writeCounterHead(b *strings.Builder, name, help string) {
	b.WriteString("# HELP " + name + " " + help + "\n# TYPE " + name + " counter\n")
}

func writeLangCounter(b *strings.Builder, name, help string, m map[string]int64) {
	writeCounterHead(b, name, help)
	for _, lang := range sortedKeys(m) {
		b.WriteString(name + "{language=" + quote(lang) + "} " + strconv.FormatInt(m[lang], 10) + "\n")
	}
}

func writeHistograms(b *strings.Builder, name, help string, hs map[string]*histogram) {
	b.WriteString("# HELP " + name + " " + help + "\n# TYPE " + name + " histogram\n")
	for _, lang := range sortedHistKeys(hs) {
		h := hs[lang]
		lbl := "language=" + quote(lang)
		cum := int64(0)
		for i, ub := range h.buckets {
			cum += h.counts[i]
			b.WriteString(name + "_bucket{" + lbl + ",le=" + quote(strconv.FormatFloat(ub, 'g', -1, 64)) + "} " + strconv.FormatInt(cum, 10) + "\n")
		}
		cum += h.counts[len(h.buckets)] // +Inf overflow bucket
		b.WriteString(name + "_bucket{" + lbl + ",le=\"+Inf\"} " + strconv.FormatInt(cum, 10) + "\n")
		b.WriteString(name + "_sum{" + lbl + "} " + strconv.FormatFloat(h.sum, 'g', -1, 64) + "\n")
		b.WriteString(name + "_count{" + lbl + "} " + strconv.FormatInt(cum, 10) + "\n")
	}
}

// histogram is a fixed-bucket cumulative histogram. counts has len(buckets)+1
// slots — the extra one is the +Inf overflow.
type histogram struct {
	buckets []float64
	counts  []int64
	sum     float64
}

func newHistogram(buckets []float64) *histogram {
	return &histogram{buckets: buckets, counts: make([]int64, len(buckets)+1)}
}

func (h *histogram) observe(v float64) {
	h.sum += v
	// SearchFloat64s returns the first index with buckets[i] >= v — exactly the
	// le-inclusive bucket for v; len(buckets) means the +Inf overflow slot.
	h.counts[sort.SearchFloat64s(h.buckets, v)]++
}

func quote(s string) string { return strconv.Quote(s) }

func sortedKeys(m map[string]int64) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func sortedHistKeys(m map[string]*histogram) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func sortedPairKeys(m map[[2]string]int64) [][2]string {
	ks := make([][2]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		if ks[i][0] != ks[j][0] {
			return ks[i][0] < ks[j][0]
		}
		return ks[i][1] < ks[j][1]
	})
	return ks
}
