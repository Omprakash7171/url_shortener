// Package cache is the Redis-backed caching layer (cache-aside, fail-open).
package cache

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"

	"urlshortener/internal/metrics"
	"urlshortener/internal/model"
)

const (
	// cacheTTL bounds how long a resolved URL is kept. Short by design: the
	// hot path (redirects) dominates and invalidation on delete is best-effort.
	cacheTTL = 60 * time.Second
	// opTimeout caps every Redis operation so a stuck peer (slow DNS, dead
	// connection) fails open quickly instead of stalling the request.
	opTimeout = 200 * time.Millisecond
)

type URLCache struct {
	rdb     redis.UniversalClient
	keyBase string
}

func NewURLCache(rdb redis.UniversalClient, keyBase string) *URLCache {
	return &URLCache{rdb: rdb, keyBase: keyBase}
}

func (c *URLCache) URLKey(code string) string {
	return c.keyBase + "url:" + code
}

// Get returns the cached URL for code. A cache miss returns (nil, nil). Any
// client-level error (Redis down, decode failure) is returned so callers can
// decide to fail open.
func (c *URLCache) Get(ctx context.Context, code string) (*model.URL, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	raw, err := c.rdb.Get(ctx, c.URLKey(code)).Result()
	if err == redis.Nil {
		metrics.CacheMisses.WithLabelValues("urllookup").Inc()
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var u model.URL
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		return nil, err
	}
	metrics.CacheHits.WithLabelValues("urllookup").Inc()
	return &u, nil
}

func (c *URLCache) Set(ctx context.Context, code string, u *model.URL) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	raw, err := json.Marshal(u)
	if err != nil {
		return err
	}
	ttl := cacheTTL
	if u.ExpiresAt != nil {
		if remain := time.Until(*u.ExpiresAt); remain < ttl {
			ttl = remain
		}
	}
	if ttl <= 0 {
		return nil
	}
	return c.rdb.Set(ctx, c.URLKey(code), raw, ttl).Err()
}

func (c *URLCache) Delete(ctx context.Context, code string) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	return c.rdb.Del(ctx, c.URLKey(code)).Err()
}