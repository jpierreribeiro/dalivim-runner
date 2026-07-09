package httpapi

import (
	"crypto/subtle"
	"net/http"

	"github.com/jpierreribeiro/dalivim-runner/internal/metrics"
)

// RequireToken is the zero-trust gate for this internal service. Because /run is
// a remote-code-execution surface, every request must present the pre-shared
// service token as the X-Runner-Token header; only the main API, which holds the
// same secret, can dispatch executions. The comparison is constant-time to avoid
// leaking the secret through response timing.
//
// When token is empty the gate is a pass-through — this is reachable only in
// development, where config.Load permits an empty token; in production startup
// fails closed if the token is unset.
func RequireToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" {
			got := r.Header.Get("X-Runner-Token")
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// LimitConcurrency bounds how many executions run at once. Each run is CPU- and
// memory-hungry, so unbounded concurrency lets a burst exhaust the container and
// take the service down. When every slot is taken the limiter fails fast with
// 503 (and Retry-After) instead of queueing, so the caller learns immediately
// and can shed load or fall back to another backend. Placed INSIDE RequireToken
// so only authenticated callers can consume a slot — an unauthenticated flood
// cannot starve the gateway of capacity.
//
// max <= 0 disables the limit (pass-through). m records the inflight gauge and
// the overload counter (nil-safe).
func LimitConcurrency(max int, m *metrics.Metrics, next http.Handler) http.Handler {
	if max <= 0 {
		return next
	}
	slots := make(chan struct{}, max)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			m.IncInflight()
			defer func() { <-slots; m.DecInflight() }()
			next.ServeHTTP(w, r)
		default:
			m.Overload()
			w.Header().Set("Retry-After", "1")
			http.Error(w, "runner at capacity", http.StatusServiceUnavailable)
		}
	})
}
