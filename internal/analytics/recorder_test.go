package analytics

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"urlshortener/internal/metrics"
)

func testRedis(t *testing.T) *redis.Client {
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
	t.Cleanup(func() {
		rdb.Del(ctx, "test-events")
		rdb.Close()
	})
	return rdb
}

func TestRecorderPublishesEventsToStream(t *testing.T) {
	rdb := testRedis(t)
	r := NewRecorder(rdb, "test-events")
	defer close(r.ch) // stops the writer goroutine

	if !r.Push(Event{Code: "abc", Referrer: "https://example.com", TS: time.Now()}) {
		t.Fatal("first push should not be dropped")
	}
	if !r.Push(Event{Code: "def", Referrer: "", TS: time.Now()}) {
		t.Fatal("second push should not be dropped")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		n, err := rdb.XLen(context.Background(), "test-events").Result()
		if err == nil && n == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	n, err := rdb.XLen(context.Background(), "test-events").Result()
	if err != nil {
		t.Fatalf("XLen: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 stream entries, got %d", n)
	}
}

func TestRecorderDropsWithoutBlockingWhenRedisDown(t *testing.T) {
	rdb := redissDown(t)
	r := NewRecorder(rdb, "test-events")
	defer close(r.ch)

	start := time.Now()
	for i := 0; i < 3; i++ {
		r.Push(Event{Code: "x", TS: time.Now()})
	}
	if elapsed := time.Since(start); elapsed > 1*time.Second {
		t.Fatalf("Push should be non-blocking, took %v", elapsed)
	}
	if metrics.CounterValue(metrics.EventsDropped) > 0 {
		t.Fatalf("buffer (4096) should absorb 3 events without dropping")
	}
}

// redissDown points at a port nothing listens on so operations fail fast
// without tripping the integration skip guard.
func redissDown(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { rdb.Close() })
	return rdb
}