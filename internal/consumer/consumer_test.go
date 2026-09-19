package consumer

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"urlshortener/internal/metrics"
	"urlshortener/internal/repository"
	"urlshortener/migrations"
)

const testStream = "test-us:events"

func testInfra(t *testing.T) (*redis.Client, *pgx.Conn, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6389"
	}

	pool, err := repository.NewPool(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("test db pool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	if _, err := pool.Exec(ctx, `TRUNCATE urls, analytics_daily, idempotency_records RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := repository.NewMigrator(pool, migrations.FS).Apply(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx.Connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(ctx) })

	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis %s not reachable: %v", addr, err)
	}
	t.Cleanup(func() {
		rdb.Del(ctx, testStream)
		rdb.Close()
	})

	return rdb, conn, pool
}

func TestConsumerAggregatesAndDedupes(t *testing.T) {
	rdb, conn, pool := testInfra(t)
	ctx := context.Background()

	if _, err := repository.NewURLRepository(pool).CreateWithCode(ctx, "abc", "https://example.com/target", nil); err != nil {
		t.Fatalf("seed url: %v", err)
	}

	if _, err := rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: testStream,
		Values: map[string]any{"code": "abc", "referrer": "https://ref.example.com/", "ts": time.Now().Unix()},
	}).Result(); err != nil {
		t.Fatalf("xadd 1: %v", err)
	}
	if _, err := rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: testStream,
		Values: map[string]any{"code": "abc", "referrer": "", "ts": time.Now().Unix()},
	}).Result(); err != nil {
		t.Fatalf("xadd 2: %v", err)
	}

	c := New(rdb, testStream, conn, loggerForTest())
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("ensure group: %v", err)
	}
	if n, err := c.ProcessOnce(ctx, false); err != nil {
		t.Fatalf("process once: %v", err)
	} else if n != 2 {
		t.Fatalf("expected 2 entries processed, got %d", n)
	}

	url, err := repository.NewURLRepository(pool).GetByCode(ctx, "abc")
	if err != nil {
		t.Fatalf("get url: %v", err)
	}
	stats, err := repository.NewURLRepository(pool).StatsByURLID(ctx, url.ID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.TotalClicks != 2 {
		t.Fatalf("totalClicks = %d, want 2", stats.TotalClicks)
	}
	if len(stats.Daily) != 1 || stats.Daily[0].Clicks != 2 {
		t.Fatalf("daily = %+v, want single day with 2 clicks", stats.Daily)
	}
	if len(stats.TopReferrers) != 1 || stats.TopReferrers[0].Referrer != "https://ref.example.com/" {
		t.Fatalf("top referrers = %+v", stats.TopReferrers)
	}

	// Re-delivery with the same stream id must be deduped: pre-claim the event
	// just like a previous run would have, then process again.
	dup, err := rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: testStream,
		Values: map[string]any{"code": "abc", "referrer": "", "ts": time.Now().Unix()},
	}).Result()
	if err != nil {
		t.Fatalf("xadd dup: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO idempotency_records (key, scope, response, expires_at)
		 VALUES ($1, 'analytics', '{}', now() + interval '1 hour')`,
		dup,
	); err != nil {
		t.Fatalf("pre-claim: %v", err)
	}
	before := metrics.CounterValue(metrics.AnalyticsDeduped)
	if n, err := c.ProcessOnce(ctx, false); err != nil {
		t.Fatalf("process dup batch: %v", err)
	} else if n != 1 {
		t.Fatalf("expected 1 entry in dup batch, got %d", n)
	}
	if got := metrics.CounterValue(metrics.AnalyticsDeduped); got != before+1 {
		t.Fatalf("dedupe counter delta = %v, want 1", got-before)
	}
	stats, err = repository.NewURLRepository(pool).StatsByURLID(ctx, url.ID)
	if err != nil {
		t.Fatalf("stats after dup: %v", err)
	}
	if stats.TotalClicks != 2 {
		t.Fatalf("totalClicks after dup = %d, want still 2", stats.TotalClicks)
	}
}

func TestConsumerIgnoresDeletedCode(t *testing.T) {
	rdb, conn, pool := testInfra(t)
	ctx := context.Background()

	if _, err := repository.NewURLRepository(pool).CreateWithCode(ctx, "gone", "https://example.com/x", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := repository.NewURLRepository(pool).DeleteByCode(ctx, "gone"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: testStream,
		Values: map[string]any{"code": "gone", "referrer": "", "ts": time.Now().Unix()},
	}).Result(); err != nil {
		t.Fatalf("xadd: %v", err)
	}

	c := New(rdb, testStream, conn, loggerForTest())
	if err := c.EnsureGroup(ctx); err != nil {
		t.Fatalf("ensure group: %v", err)
	}
	if n, err := c.ProcessOnce(ctx, false); err != nil {
		t.Fatalf("process: %v", err)
	} else if n != 1 {
		t.Fatalf("expected 1 entry, got %d", n)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT coalesce(sum(count), 0) FROM analytics_daily`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("analytics rows should be 0 for a deleted url, got %d", count)
	}
}

func loggerForTest() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}