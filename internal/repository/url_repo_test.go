package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"urlshortener/migrations"
)

func resetURLs(t *testing.T, pool *pgxpool.Pool, m *Migrator) {
	t.Helper()
	ctx := context.Background()
	if err := m.Apply(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE urls RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate urls: %v", err)
	}
}

func TestURLCreateThenSetCodeThenGetByCode(t *testing.T) {
	pool := testPool(t)
	repo := NewURLRepository(pool)
	m := NewMigrator(pool, migrations.FS)
	resetURLs(t, pool, m)
	ctx := context.Background()

	id, err := repo.Create(ctx, "https://example.com/a", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == 0 {
		t.Fatal("create returned zero id")
	}

	if err := repo.SetCode(ctx, id, "abc123"); err != nil {
		t.Fatalf("set code: %v", err)
	}

	got, err := repo.GetByCode(ctx, "abc123")
	if err != nil {
		t.Fatalf("get by code: %v", err)
	}
	if got.ID != id || got.LongURL != "https://example.com/a" || got.Code != "abc123" {
		t.Fatalf("unexpected row: %+v", got)
	}
	if got.ExpiresAt != nil {
		t.Fatalf("expected nil expiry, got %v", got.ExpiresAt)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("created_at not populated")
	}
}

func TestURLCreateWithCodeConflict(t *testing.T) {
	pool := testPool(t)
	repo := NewURLRepository(pool)
	m := NewMigrator(pool, migrations.FS)
	resetURLs(t, pool, m)
	ctx := context.Background()

	if _, err := repo.CreateWithCode(ctx, "taken", "https://example.com/1", nil); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := repo.CreateWithCode(ctx, "taken", "https://example.com/2", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
	if _, err := repo.CreateWithCode(ctx, "other", "https://example.com/3", nil); err != nil {
		t.Fatalf("different code must not conflict: %v", err)
	}
}

func TestURLGetNotFound(t *testing.T) {
	pool := testPool(t)
	repo := NewURLRepository(pool)
	m := NewMigrator(pool, migrations.FS)
	resetURLs(t, pool, m)

	if _, err := repo.GetByCode(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestURLSetCodeRoundTripExpiry(t *testing.T) {
	pool := testPool(t)
	repo := NewURLRepository(pool)
	m := NewMigrator(pool, migrations.FS)
	resetURLs(t, pool, m)
	ctx := context.Background()

	exp := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Microsecond)
	id, err := repo.Create(ctx, "https://example.com/e", &exp)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.SetCode(ctx, id, "expxyz"); err != nil {
		t.Fatalf("set code: %v", err)
	}

	got, err := repo.GetByCode(ctx, "expxyz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("expiry mismatch: got %v want %v", got.ExpiresAt, exp)
	}
}

func TestURLDeleteAndCascade(t *testing.T) {
	pool := testPool(t)
	repo := NewURLRepository(pool)
	m := NewMigrator(pool, migrations.FS)
	resetURLs(t, pool, m)
	ctx := context.Background()

	id, err := repo.CreateWithCode(ctx, "delme", "https://example.com/d", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO analytics_daily (url_id, day, dim, value, count) VALUES ($1, current_date, 'total', 'total', 7)`,
		id,
	); err != nil {
		t.Fatalf("seed analytics: %v", err)
	}

	if err := repo.DeleteByCode(ctx, "delme"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.GetByCode(ctx, "delme"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}

	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM analytics_daily WHERE url_id = $1`, id).Scan(&remaining); err != nil {
		t.Fatalf("count analytics: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("expected cascaded analytics deletion, %d rows remain", remaining)
	}

	if err := repo.DeleteByCode(ctx, "delme"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete must be ErrNotFound, got %v", err)
	}
}