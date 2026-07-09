package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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
	h := LimitConcurrency(1, handler)

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
	h := LimitConcurrency(0, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/run", nil))
	if !called || rec.Code != http.StatusOK {
		t.Fatalf("max<=0 must be a pass-through, got called=%v code=%d", called, rec.Code)
	}
}
