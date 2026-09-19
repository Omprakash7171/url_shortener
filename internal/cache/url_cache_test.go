package cache

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"urlshortener/internal/model"
)

func testCache(t *testing.T) *URLCache {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6389"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis %s not reachable: %v", addr, err)
	}
	t.Cleanup(func() { rdb.Close() })
	return NewURLCache(rdb, "test:" + "us:")
}

func TestGetMiss(t *testing.T) {
	c := testCache(t)
	u, err := c.Get(context.Background(), "nope")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if u != nil {
		t.Fatalf("expected nil on miss, got %+v", u)
	}
}

func TestSetGetRoundTrip(t *testing.T) {
	c := testCache(t)
	ctx := context.Background()
	in := &model.URL{ID: 42, Code: "abc", LongURL: "https://example.com/r", CreatedAt: time.Now().UTC()}
	if err := c.Set(ctx, "abc", in); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := c.Get(ctx, "abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.ID != 42 || got.LongURL != "https://example.com/r" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestDeleteRemovesEntry(t *testing.T) {
	c := testCache(t)
	ctx := context.Background()
	in := &model.URL{ID: 7, Code: "del", LongURL: "https://example.com/d"}
	if err := c.Set(ctx, "del", in); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Delete(ctx, "del"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if u, err := c.Get(ctx, "del"); err != nil || u != nil {
		t.Fatalf("expected miss after delete, got %+v err=%v", u, err)
	}
}

func TestSetWithPastExpiryDoesNotStore(t *testing.T) {
	c := testCache(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Minute)
	in := &model.URL{ID: 1, Code: "exp", LongURL: "https://example.com/e", ExpiresAt: &past}
	if err := c.Set(ctx, "exp", in); err != nil {
		t.Fatalf("Set: %v", err)
	}
	k, err := c.rdb.Exists(ctx, c.URLKey("exp")).Result()
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if k != 0 {
		t.Fatalf("expected no key stored for already-expired entry, exists=%d", k)
	}
}