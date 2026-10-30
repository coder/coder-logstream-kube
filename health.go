package main

import (
	"net/http"
)

// livenessHandler returns a handler that reports whether the log work loop has
// made progress within livenessTimeout. It returns 200 when the loop is healthy
// and 503 when it is stalled. The stalled func must be safe to call concurrently
// with the work loop; logQueuer.stalled satisfies this.
func livenessHandler(stalled func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stalled() {
			http.Error(w, "log work loop is stalled", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
}
