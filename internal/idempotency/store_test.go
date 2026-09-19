package idempotency

import (
	"context"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
)

func testStore(t *testing.T) *Store {
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
		rdb.Del(ctx, "testidem:resp:k1", "testidem:lock:k1", "testidem:resp:k2", "testidem:lock:k2")
		rdb.Close()
	})
	return NewStore(rdb, "testidem:")
}

func TestGetUnknownKey(t *testing.T) {
	s := testStore(t)
	code, ok, err := s.Get(context.Background(), "missing")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok || code != "" {
		t.Fatalf("expected (\"\", false), got (%q, %v)", code, ok)
	}
}

func TestSetGetRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.Set(ctx, "k1", "ab12"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	code, ok, err := s.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok || code != "ab12" {
		t.Fatalf("expected code ab12, got (%q, %v)", code, ok)
	}
}

func TestClaimIsExclusive(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	first, err := s.Claim(ctx, "k2")
	if err != nil {
		t.Fatalf("Claim 1: %v", err)
	}
	if !first {
		t.Fatal("first claimant should win")
	}
	second, err := s.Claim(ctx, "k2")
	if err != nil {
		t.Fatalf("Claim 2: %v", err)
	}
	if second {
		t.Fatal("second claimant must not win while lock is held")
	}

	if err := s.Release(ctx, "k2"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	after, err := s.Claim(ctx, "k2")
	if err != nil {
		t.Fatalf("Claim 3: %v", err)
	}
	if !after {
		t.Fatal("after release the key must be claimable again")
	}
	_ = s.Release(ctx, "k2")
}