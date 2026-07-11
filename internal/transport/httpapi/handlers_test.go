package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/jpierreribeiro/dalivim-runner/internal/executor"
	"github.com/jpierreribeiro/dalivim-runner/internal/sandbox"
	"github.com/jpierreribeiro/dalivim-runner/pkg/runnerapi"
)

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available; skipping runner execution test")
	}
}

// testServer builds a real server (isolation off) with the given token.
func testServer(t *testing.T, token string) http.Handler {
	t.Helper()
	sb, err := sandbox.Configure("off", "off", "off", "")
	if err != nil {
		t.Fatalf("configure sandbox: %v", err)
	}
	svc := executor.NewService(
		executor.Limits{
			DefaultTimeout: 3000, MaxTimeoutMs: 10000, DefaultMemory: 128, MaxMemoryMB: 512,
			Files: executor.FileCaps{MaxFiles: 50, MaxFileBytes: 262_144, MaxFilesBytes: 1_048_576, MaxPathBytes: 180, MaxPathDepth: 8},
		},
		executor.NewPython(sb, 64*1024, 256, 64),
	)
	return New(svc, Config{Addr: ":0", Token: token, MaxSourceBytes: 200_000, MaxStdinBytes: 1_000_000, MaxFilesBytes: 1_048_576, MaxBatch: 100, MaxBatchStdinBytes: 4_000_000}).Handler()
}

func post(h http.Handler, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-Runner-Token", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func get(h http.Handler, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("X-Runner-Token", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRun_RejectsEmptySource(t *testing.T) {
	h := testServer(t, "")
	rec := post(h, "/run", "", `{"language":"python","source_code":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty source, got %d", rec.Code)
	}
}

// TestLanguages_Endpoint pins G12: GET /languages is unauthenticated (open even
// when a service token is set), lists the registered languages with their flags,
// reports the effective limits, and leaks no secret.
func TestLanguages_Endpoint(t *testing.T) {
	h := testServer(t, "secret") // token set: /languages must still be reachable without it
	rec := get(h, "/languages", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for GET /languages, got %d", rec.Code)
	}
	var resp struct {
		Languages []struct {
			ID        string `json:"id"`
			Kind      string `json:"kind"`
			MultiFile bool   `json:"multifile"`
			Batch     bool   `json:"batch"`
		} `json:"languages"`
		Limits struct {
			MaxTimeoutMs   int `json:"max_timeout_ms"`
			MaxMemoryMB    int `json:"max_memory_mb"`
			MaxSourceBytes int `json:"max_source_bytes"`
			MaxBatch       int `json:"max_batch"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode /languages: %v", err)
	}
	if len(resp.Languages) != 1 || resp.Languages[0].ID != "python" {
		t.Fatalf("expected python in catalog, got %+v", resp.Languages)
	}
	if resp.Languages[0].Kind != "interpreted" || !resp.Languages[0].MultiFile || !resp.Languages[0].Batch {
		t.Fatalf("python capability flags wrong: %+v", resp.Languages[0])
	}
	if resp.Limits.MaxTimeoutMs != 10000 || resp.Limits.MaxMemoryMB != 512 ||
		resp.Limits.MaxSourceBytes != 200_000 || resp.Limits.MaxBatch != 100 {
		t.Fatalf("effective limits wrong: %+v", resp.Limits)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("token leaked into /languages payload")
	}
}

// TestLanguages_WrongMethod pins that path+method routing gives a 405 for POST.
func TestLanguages_WrongMethod(t *testing.T) {
	h := testServer(t, "")
	rec := post(h, "/languages", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for POST /languages, got %d", rec.Code)
	}
}

// TestRun_Base64BinaryRoundTrip is the headline for the base64 I/O mode: a program
// emitting invalid UTF-8 bytes survives the JSON boundary intact with
// encoding=base64, and is corrupted (U+FFFD) in the default text path — proving the
// mode fixes the exact loss it exists for.
func TestRun_Base64BinaryRoundTrip(t *testing.T) {
	requirePython(t)
	h := testServer(t, "")
	src := "import sys; sys.stdout.buffer.write(bytes([0,255,254,65,10]))"
	want := string([]byte{0x00, 0xff, 0xfe, 0x41, 0x0a})

	b64body, _ := json.Marshal(map[string]string{"language": "python", "encoding": "base64", "source_code": src})
	rec := post(h, "/run", "", string(b64body))
	if rec.Code != http.StatusOK {
		t.Fatalf("base64 run: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var res runnerapi.RunResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	got, err := base64.StdEncoding.DecodeString(res.Stdout)
	if err != nil || string(got) != want {
		t.Fatalf("base64 stdout did not round-trip: stdout=%q decoded=% x err=%v", res.Stdout, got, err)
	}

	txtbody, _ := json.Marshal(map[string]string{"language": "python", "source_code": src})
	rec2 := post(h, "/run", "", string(txtbody))
	var res2 runnerapi.RunResult
	_ = json.Unmarshal(rec2.Body.Bytes(), &res2)
	if res2.Stdout == want {
		t.Fatalf("expected the default text path to corrupt binary output, but it round-tripped exactly")
	}
}

func TestRun_Base64InvalidStdin(t *testing.T) {
	h := testServer(t, "")
	body, _ := json.Marshal(map[string]string{"language": "python", "encoding": "base64", "source_code": "print(1)", "stdin": "!!! not base64 !!!"})
	rec := post(h, "/run", "", string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid base64 stdin, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestRun_UnsupportedEncoding(t *testing.T) {
	h := testServer(t, "")
	body, _ := json.Marshal(map[string]string{"language": "python", "encoding": "rot13", "source_code": "print(1)"})
	rec := post(h, "/run", "", string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsupported encoding, got %d", rec.Code)
	}
}

func TestRun_RejectsMissingLanguage(t *testing.T) {
	h := testServer(t, "")
	rec := post(h, "/run", "", `{"source_code":"print(1)"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing language, got %d", rec.Code)
	}
}

func TestRun_RejectsGetMethod(t *testing.T) {
	h := testServer(t, "")
	req := httptest.NewRequest(http.MethodGet, "/run", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET, got %d", rec.Code)
	}
}

func TestRun_UnsupportedLanguage(t *testing.T) {
	h := testServer(t, "")
	rec := post(h, "/run", "", `{"language":"ruby","source_code":"puts 1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsupported language, got %d", rec.Code)
	}
}

func TestRun_SuccessEnvelope(t *testing.T) {
	requirePython(t)
	h := testServer(t, "")
	rec := post(h, "/run", "", `{"language":"python","source_code":"print('hi')","timeout_ms":3000,"memory_mb":128}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var res runnerapi.RunResult
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.Status != runnerapi.StatusSuccess || res.Stdout != "hi\n" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.RuntimeName != "python" || res.PythonVersion == "" {
		t.Fatalf("missing provenance: %+v", res)
	}
}

// TestRunPythonCompat_Alias verifies the deprecated /run/python path still works
// (no language field needed) so the pre-extraction gateway keeps functioning.
func TestRunPythonCompat_Alias(t *testing.T) {
	requirePython(t)
	h := testServer(t, "")
	rec := post(h, "/run/python", "", `{"source_code":"print('legacy')","timeout_ms":3000,"memory_mb":128}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var res runnerapi.RunResult
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.Status != runnerapi.StatusSuccess || res.Stdout != "legacy\n" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// TestRun_StdinCap pins G1.3: stdin has its own size bound, independent of the
// source budget. Over the cap → 400 (a bad request, not a run outcome); at/under
// the cap the request is accepted (reaches the runtime).
func TestRun_StdinCap(t *testing.T) {
	sb, err := sandbox.Configure("off", "off", "off", "")
	if err != nil {
		t.Fatalf("configure sandbox: %v", err)
	}
	svc := executor.NewService(
		executor.Limits{DefaultTimeout: 3000, MaxTimeoutMs: 10000, DefaultMemory: 128, MaxMemoryMB: 512},
		executor.NewPython(sb, 64*1024, 256, 64),
	)
	// Small stdin cap so the test body stays tiny; source cap stays generous to
	// prove the two limits are independent.
	h := New(svc, Config{Addr: ":0", MaxSourceBytes: 200_000, MaxStdinBytes: 100}).Handler()

	big, _ := json.Marshal(runnerapi.RunRequest{Language: "python", SourceCode: "print(1)", Stdin: strings.Repeat("x", 101)})
	if rec := post(h, "/run", "", string(big)); rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized stdin must be 400, got %d", rec.Code)
	}

	requirePython(t)
	ok, _ := json.Marshal(runnerapi.RunRequest{Language: "python", SourceCode: "print(1)", Stdin: strings.Repeat("x", 100), TimeoutMs: 3000, MemoryMB: 128})
	if rec := post(h, "/run", "", string(ok)); rec.Code != http.StatusOK {
		t.Fatalf("stdin at the cap must be accepted, got %d", rec.Code)
	}
}

func TestRun_TokenRequired(t *testing.T) {
	h := testServer(t, "sekret")
	body := `{"language":"python","source_code":"print(1)","timeout_ms":1000,"memory_mb":128}`

	// Missing token → 401 (rejected before running anything).
	if rec := post(h, "/run", "", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token must be 401, got %d", rec.Code)
	}
	// Wrong token → 401.
	if rec := post(h, "/run", "nope", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token must be 401, got %d", rec.Code)
	}
	// Correct token → accepted.
	requirePython(t)
	if rec := post(h, "/run", "sekret", body); rec.Code != http.StatusOK {
		t.Fatalf("correct token must be accepted, got %d", rec.Code)
	}
}

// fixedRuntime is a stub Runtime that returns a preset result, so the transport's
// status→HTTP mapping can be tested without a real interpreter.
type fixedRuntime struct{ res runnerapi.RunResult }

func (fixedRuntime) Language() string { return "python" }
func (fixedRuntime) Version() string  { return "0.0.0" }
func (f fixedRuntime) Run(context.Context, runnerapi.RunRequest) runnerapi.RunResult {
	return f.res
}

func serverWithRuntime(rt executor.Runtime) http.Handler {
	svc := executor.NewService(
		executor.Limits{DefaultTimeout: 3000, MaxTimeoutMs: 10000, DefaultMemory: 128, MaxMemoryMB: 512},
		rt,
	)
	return New(svc, Config{Addr: ":0", MaxSourceBytes: 200_000}).Handler()
}

// TestRun_InternalErrorIsFailover pins RUNNER_AUDIT_AND_CONTRACT §2.4.1: the
// runner's OWN per-run infra failure (internal_error) must surface as 503 +
// Retry-After so the Gateway can fall back, not a terminal 200.
func TestRun_InternalErrorIsFailover(t *testing.T) {
	h := serverWithRuntime(fixedRuntime{res: runnerapi.RunResult{Status: runnerapi.StatusInternalError, Stderr: "sandbox down"}})
	rec := post(h, "/run", "", `{"language":"python","source_code":"x"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("internal_error must be 503 for failover, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("503 must carry a Retry-After header")
	}
	// The body still carries the RunResult for a Gateway that logs it.
	var res runnerapi.RunResult
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if res.Status != runnerapi.StatusInternalError {
		t.Fatalf("body should still carry internal_error, got %q", res.Status)
	}
}

// TestRun_StudentOutcomesStay200 confirms deterministic student-code outcomes are
// NOT turned into failover 5xx — only the runner's own infra failure is.
func TestRun_StudentOutcomesStay200(t *testing.T) {
	for _, status := range []string{
		runnerapi.StatusSuccess, runnerapi.StatusRuntimeError,
		runnerapi.StatusTimeout, runnerapi.StatusMemoryExceeded,
	} {
		h := serverWithRuntime(fixedRuntime{res: runnerapi.RunResult{Status: status}})
		rec := post(h, "/run", "", `{"language":"python","source_code":"x"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %q must stay 200, got %d", status, rec.Code)
		}
	}
}

// TestReadyz_ReflectsPosture pins G4.3: /readyz is 503 when nsjail is required but
// not the active backend, and 200 when the requirement is relaxed — reporting the
// resolved posture either way.
func TestReadyz_ReflectsPosture(t *testing.T) {
	svc := executor.NewService(executor.Limits{}, fixedRuntime{})

	// Requires nsjail, but backend is "netns" => not ready (503).
	degraded := New(svc, Config{Backend: "netns", ReadyRequiresNsjail: true}).Handler()
	rec := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	degraded.ServeHTTP(rr, rec)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("degraded posture must be 503, got %d", rr.Code)
	}
	var body map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body["ready"] != false || body["backend"] != "netns" {
		t.Fatalf("readyz body should report posture, got %v", body)
	}

	// nsjail active => ready (200), and the memory-accounting posture is reported so
	// the escape corpus can tell whether the RLIMIT_AS-incompatible runtimes are
	// contained on a memory bomb here (cgroup engaged) or only on-target.
	ok := New(svc, Config{Backend: "nsjail", MemoryAccounting: "cgroup-v2:/sys/fs/cgroup/dalivim", ReadyRequiresNsjail: true}).Handler()
	rr = httptest.NewRecorder()
	ok.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("nsjail posture must be 200, got %d", rr.Code)
	}
	body = nil
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body["memory_accounting"] != "cgroup-v2:/sys/fs/cgroup/dalivim" {
		t.Fatalf("readyz must report the memory_accounting posture, got %v", body["memory_accounting"])
	}

	// Requirement relaxed => ready even on netns.
	relaxed := New(svc, Config{Backend: "netns", ReadyRequiresNsjail: false}).Handler()
	rr = httptest.NewRecorder()
	relaxed.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("relaxed requirement must be 200, got %d", rr.Code)
	}
}

// TestMetrics_GatedAndRecords pins G4.1: /metrics needs the token and, after a
// run, exposes the series.
func TestMetrics_GatedAndRecords(t *testing.T) {
	svc := executor.NewService(
		executor.Limits{DefaultTimeout: 3000, MaxTimeoutMs: 10000, DefaultMemory: 128, MaxMemoryMB: 512},
		fixedRuntime{res: runnerapi.RunResult{Status: runnerapi.StatusSuccess, DurationMs: 42}},
	)
	h := New(svc, Config{Token: "sekret", MaxSourceBytes: 200_000}).Handler()

	// Unauthenticated /metrics is rejected.
	if rec := get(h, "/metrics", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/metrics without token must be 401, got %d", rec.Code)
	}
	// Drive one run, then scrape.
	post(h, "/run", "sekret", `{"language":"python","source_code":"x"}`)
	rec := get(h, "/metrics", "sekret")
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics with token must be 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `runner_runs_total{language="python",status="success"} 1`) {
		t.Fatalf("metrics did not record the run:\n%s", rec.Body.String())
	}
}

// TestRequestID_Echo pins G4.2's trace stitch: an inbound X-Request-ID is echoed;
// when absent one is minted.
func TestRequestID_Echo(t *testing.T) {
	svc := executor.NewService(executor.Limits{}, fixedRuntime{res: runnerapi.RunResult{Status: runnerapi.StatusSuccess}})
	h := New(svc, Config{MaxSourceBytes: 200_000}).Handler()

	req := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(`{"language":"python","source_code":"x"}`))
	req.Header.Set("X-Request-ID", "trace-abc")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if got := rr.Header().Get("X-Request-ID"); got != "trace-abc" {
		t.Fatalf("inbound request id must be echoed, got %q", got)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(`{"language":"python","source_code":"x"}`)))
	if rr.Header().Get("X-Request-ID") == "" {
		t.Fatal("a request id must be minted when absent")
	}
}

// TestRun_MultiFileSuccess drives a files[] submission end-to-end through the
// transport: validation, materialization, and a sibling import, returning 200.
func TestRun_MultiFileSuccess(t *testing.T) {
	requirePython(t)
	h := testServer(t, "")
	body := `{"language":"python","files":[` +
		`{"path":"main.py","content":"from helper import v\nprint(v)\n"},` +
		`{"path":"helper.py","content":"v = 99\n"}],"timeout_ms":8000,"memory_mb":128}`
	rec := post(h, "/run", "", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var res runnerapi.RunResult
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Status != runnerapi.StatusSuccess || strings.TrimSpace(res.Stdout) != "99" {
		t.Fatalf("multi-file run failed: %+v", res)
	}
}

// TestRun_MultiFileRejections pins the 400s the transport must return for a
// malformed multi-file submission (the executor's ValidationError → 400 mapping).
func TestRun_MultiFileRejections(t *testing.T) {
	h := testServer(t, "")
	cases := map[string]string{
		"both source and files": `{"language":"python","source_code":"x","files":[{"path":"main.py","content":"x"}]}`,
		"neither":               `{"language":"python"}`,
		"path traversal":        `{"language":"python","files":[{"path":"../evil.py","content":"x"}]}`,
		"absolute path":         `{"language":"python","files":[{"path":"/etc/passwd","content":"x"}]}`,
		"forbidden file":        `{"language":"python","files":[{"path":"setup.py","content":"x"}]}`,
		"forbidden extension":   `{"language":"python","files":[{"path":"evil.sh","content":"x"}]}`,
		"missing entrypoint":    `{"language":"python","entrypoint":"nope.py","files":[{"path":"main.py","content":"x"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if rec := post(h, "/run", "", body); rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHealthz_NoAuth(t *testing.T) {
	h := testServer(t, "sekret")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("healthz should be 200 ok without a token, got %d %q", rec.Code, rec.Body.String())
	}
}

// TestRun_BatchEnvelope drives a stdins[] request end-to-end (G6): 200 with the
// batch envelope — shared provenance, index-aligned raw results, no aborted flag.
func TestRun_BatchEnvelope(t *testing.T) {
	requirePython(t)
	h := testServer(t, "")
	body, _ := json.Marshal(runnerapi.RunRequest{
		Language:   "python",
		SourceCode: "import sys\nprint('got:' + sys.stdin.read().strip())\n",
		Stdins:     []string{"a", "b"},
		TimeoutMs:  8000, MemoryMB: 128,
	})
	rec := post(h, "/run", "", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("batch must be 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var res runnerapi.BatchResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode batch envelope: %v", err)
	}
	if res.Status != runnerapi.BatchStatusOK || res.RuntimeName != "python" || res.Aborted {
		t.Fatalf("envelope wrong: %+v", res)
	}
	if len(res.Results) != 2 || res.Results[0].Stdout != "got:a\n" || res.Results[1].Stdout != "got:b\n" {
		t.Fatalf("results not index-aligned: %+v", res.Results)
	}
}

// TestRun_BatchCaps pins the transport-level batch bounds (G6): input count,
// per-element stdin size (same cap as a single run), and the summed budget —
// each a 400 before anything executes.
func TestRun_BatchCaps(t *testing.T) {
	sb, err := sandbox.Configure("off", "off", "off", "")
	if err != nil {
		t.Fatalf("configure sandbox: %v", err)
	}
	svc := executor.NewService(
		executor.Limits{DefaultTimeout: 3000, MaxTimeoutMs: 10000, DefaultMemory: 128, MaxMemoryMB: 512},
		executor.NewPython(sb, 64*1024, 256, 64),
	)
	h := New(svc, Config{Addr: ":0", MaxSourceBytes: 200_000, MaxStdinBytes: 100, MaxBatch: 2, MaxBatchStdinBytes: 150}).Handler()

	cases := map[string]runnerapi.RunRequest{
		"too many stdins":       {Language: "python", SourceCode: "print(1)", Stdins: []string{"a", "b", "c"}},
		"element over the cap":  {Language: "python", SourceCode: "print(1)", Stdins: []string{strings.Repeat("x", 101)}},
		"total over the budget": {Language: "python", SourceCode: "print(1)", Stdins: []string{strings.Repeat("x", 80), strings.Repeat("x", 80)}},
	}
	for name, req := range cases {
		body, _ := json.Marshal(req)
		if rec := post(h, "/run", "", string(body)); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d (%s)", name, rec.Code, rec.Body.String())
		}
	}
}

// TestRun_BatchStdinExclusive pins the shape rule: stdin and stdins are
// mutually exclusive (the executor's ValidationError → 400 mapping).
func TestRun_BatchStdinExclusive(t *testing.T) {
	h := testServer(t, "")
	body := `{"language":"python","source_code":"print(1)","stdin":"a","stdins":["b"]}`
	if rec := post(h, "/run", "", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("stdin+stdins must be 400, got %d (%s)", rec.Code, rec.Body.String())
	}
}
