package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRequireTokens_RotationSet pins G8.2: every token in the set authorizes (so
// old+new overlap during a rotation), an unknown token is rejected, and an empty
// set is a dev-only pass-through.
func TestRequireTokens_RotationSet(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := RequireTokens([]string{"old-secret", "new-secret"}, ok)

	call := func(tok string) int {
		req := httptest.NewRequest(http.MethodPost, "/run", nil)
		if tok != "" {
			req.Header.Set("X-Runner-Token", tok)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := call("old-secret"); code != http.StatusOK {
		t.Fatalf("the old token must still authorize during rotation, got %d", code)
	}
	if code := call("new-secret"); code != http.StatusOK {
		t.Fatalf("the new token must authorize, got %d", code)
	}
	if code := call("wrong"); code != http.StatusUnauthorized {
		t.Fatalf("an unknown token must be 401, got %d", code)
	}
	if code := call(""); code != http.StatusUnauthorized {
		t.Fatalf("a missing token must be 401, got %d", code)
	}

	// Empty set: pass-through (dev only — config fails closed in prod).
	passthrough := RequireTokens(nil, ok)
	rec := httptest.NewRecorder()
	passthrough.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/run", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("an empty token set must be a pass-through, got %d", rec.Code)
	}
}

// TestLimitConcurrency_ShedsLoadWith503 verifies the limiter admits up to max
// executions, sheds the overflow with 503 + Retry-After (rather than queueing),
// and frees the slot once a run completes.
func TestLimitConcurrency_ShedsLoadWith503(t *testing.T) {
	started := make(chan struct{}, 2) // buffered so admitted runs never block the send
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	h := LimitConcurrency(1, nil, handler)

	// Occupy the only slot with an in-flight request.
	firstDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/run", nil))
		firstDone <- rec.Code
	}()
	<-started // the slot is now held

	// A second request while saturated must be shed immediately.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/run", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("saturated limiter must return 503, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("503 should carry a Retry-After header")
	}

	// Release the in-flight run; it completes and frees the slot.
	close(release)
	if code := <-firstDone; code != http.StatusOK {
		t.Fatalf("first request should complete 200, got %d", code)
	}

	// Slot free again → a fresh request is admitted, not shed. (release is closed,
	// so the handler returns without blocking.)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/run", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("after release the slot should be reusable, got %d", rec2.Code)
	}
}

// TestLimitConcurrency_DisabledWhenNonPositive confirms max<=0 is a pass-through
// (the limiter must not silently drop traffic when unconfigured).
func TestLimitConcurrency_DisabledWhenNonPositive(t *testing.T) {
	called := false
	h := LimitConcurrency(0, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/run", nil))
	if !called || rec.Code != http.StatusOK {
		t.Fatalf("max<=0 must be a pass-through, got called=%v code=%d", called, rec.Code)
	}
}
