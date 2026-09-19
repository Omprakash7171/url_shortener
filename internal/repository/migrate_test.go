package repository

import (
	"context"
	"io/fs"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"urlshortener/migrations"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := NewPool(context.Background(), dsn, 4)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func resetSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE`); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE SCHEMA public`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
}

func TestMigratorAppliesExactlyOnceUnderConcurrency(t *testing.T) {
	pool := testPool(t)
	resetSchema(t, pool)
	ctx := context.Background()

	m := NewMigrator(pool, migrations.FS)
	const goroutines = 3
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = m.Apply(ctx)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("migration goroutine %d failed: %v", i, err)
		}
	}

	var applied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	files, err := fs.Glob(migrations.FS, "*.up.sql")
	if err != nil {
		t.Fatalf("list up migrations: %v", err)
	}
	if applied != len(files) {
		t.Fatalf("applied %d migrations, want %d", applied, len(files))
	}

	var hasURLs bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.urls') IS NOT NULL`).Scan(&hasURLs); err != nil {
		t.Fatalf("check urls table: %v", err)
	}
	if !hasURLs {
		t.Fatal("urls table missing after migration")
	}

	if err := m.Apply(ctx); err != nil {
		t.Fatalf("second apply must be a no-op: %v", err)
	}
}