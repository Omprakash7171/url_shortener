package middleware

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"urlshortener/internal/metrics"
)

type rateLimiter struct {
	rdb       redis.UniversalClient
	keyBase   string
	perMinute int
}

// RateLimit applies a fixed-window per-IP limit (defaults to per-minute). When
// the limit is exceeded it writes a 429 `rate_limited` error envelope. Redis
// failures fail open (the request is allowed) and are counted, mirroring the
// cache fails-open design.
func RateLimit(rdb redis.UniversalClient, keyBase string, perMinute int) func(http.Handler) http.Handler {
	if perMinute <= 0 {
		perMinute = 60
	}
	rl := &rateLimiter{rdb: rdb, keyBase: keyBase, perMinute: perMinute}
	return rl.middleware
}

func (rl *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed, retryAfter, err := rl.allow(r)
		if err != nil {
			metrics.RateLimitFailOpen.Inc()
			next.ServeHTTP(w, r)
			return
		}
		if !allowed {
			reqID := RequestIDFrom(r.Context())
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":       "rate_limited",
					"message":    "too many requests",
					"request_id": reqID,
				},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (rl *rateLimiter) allow(r *http.Request) (bool, int, error) {
	window := time.Now().Unix() / 60
	key := rl.keyBase + "rl:ip:" + clientIP(r.RemoteAddr) + ":" + strconv.FormatInt(window, 10)

	ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
	defer cancel()

	count, err := rl.rdb.Incr(ctx, key).Result()
	if err != nil {
		return false, 0, err
	}
	if count == 1 {
		_ = rl.rdb.Expire(ctx, key, 61*time.Second).Err()
	}
	if count > int64(rl.perMinute) {
		retryAfter := 60 - int(time.Now().Unix()%60)
		if retryAfter < 1 {
			retryAfter = 1
		}
		return false, retryAfter, nil
	}
	return true, 0, nil
}

func clientIP(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	return strings.TrimSpace(host)
}