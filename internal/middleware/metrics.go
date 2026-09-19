package middleware

import (
	"net/http"
	"time"

	"urlshortener/internal/metrics"
)

// Metrics observes every request's duration into the shared Prometheus histogram.
func Metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		metrics.HTTPRequests.Observe(time.Since(start).Seconds())
	})
}