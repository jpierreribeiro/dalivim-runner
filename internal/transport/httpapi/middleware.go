package httpapi

import (
	"crypto/subtle"
	"net/http"
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
