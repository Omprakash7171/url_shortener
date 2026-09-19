package middleware

import "net/http"

// InstanceID stamps a response header naming the server instance so load
// distribution is observable (and provable in load/failure tests).
func InstanceID(id string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Instance", id)
			next.ServeHTTP(w, r)
		})
	}
}