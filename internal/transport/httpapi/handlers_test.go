package httpapi

import (
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
	sb, err := sandbox.Configure("off", "off")
	if err != nil {
		t.Fatalf("configure sandbox: %v", err)
	}
	svc := executor.NewService(
		executor.Limits{DefaultTimeout: 3000, MaxTimeoutMs: 10000, DefaultMemory: 128, MaxMemoryMB: 512},
		executor.NewPython(sb, 64*1024, 256, 64),
	)
	return New(svc, Config{Addr: ":0", Token: token, MaxSourceBytes: 200_000}).Handler()
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

func TestRun_RejectsEmptySource(t *testing.T) {
	h := testServer(t, "")
	rec := post(h, "/run", "", `{"language":"python","source_code":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty source, got %d", rec.Code)
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

func TestHealthz_NoAuth(t *testing.T) {
	h := testServer(t, "sekret")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("healthz should be 200 ok without a token, got %d %q", rec.Code, rec.Body.String())
	}
}
