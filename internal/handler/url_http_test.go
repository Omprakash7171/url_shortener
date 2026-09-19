package handler_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"urlshortener/internal/cache"
	"urlshortener/internal/idempotency"
	"urlshortener/internal/metrics"
	"urlshortener/internal/repository"
	"urlshortener/internal/router"
	"urlshortener/internal/service"
	"urlshortener/migrations"
)

func newAPI(t *testing.T) http.Handler {
	return newAPIServer(t, nil)
}

func newRatedAPI(t *testing.T, perMinute int) http.Handler {
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
		rdb.FlushDB(ctx)
		rdb.Close()
	})

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := repository.NewPool(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if _, err := pool.Exec(ctx, `TRUNCATE urls RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate urls: %v", err)
	}
	if err := repository.NewMigrator(pool, migrations.FS).Apply(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	svc, err := service.NewURLService(repository.NewURLRepository(pool), nil, nil, "http://s.example", logger)
	if err != nil {
		t.Fatalf("NewURLService: %v", err)
	}
	return router.New(logger, pool, svc, router.WithRateLimiter(rdb, "test-rl:", perMinute))
}

func newAPIServer(t *testing.T, withCache *cache.URLCache) http.Handler {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}

	ctx := context.Background()
	pool, err := repository.NewPool(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	if _, err := pool.Exec(ctx, `TRUNCATE urls RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate urls: %v", err)
	}
	if err := repository.NewMigrator(pool, migrations.FS).Apply(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	svc, err := service.NewURLService(repository.NewURLRepository(pool), withCache, nil, "http://s.example", logger)
	if err != nil {
		t.Fatalf("NewURLService: %v", err)
	}
	return router.New(logger, pool, svc)
}

// newIdemAPI builds a full API (Redis connected) with the idempotency store, so
// we can test Idempotency-Key semantics end to end. Returns the handler and the
// pool so tests can assert against the database.
func newIdemAPI(t *testing.T) (http.Handler, *pgxpool.Pool) {
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
		rdb.FlushDB(ctx)
		rdb.Close()
	})

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := repository.NewPool(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if _, err := pool.Exec(ctx, `TRUNCATE urls RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate urls: %v", err)
	}
	if err := repository.NewMigrator(pool, migrations.FS).Apply(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	svc, err := service.NewURLService(repository.NewURLRepository(pool), nil, idempotency.NewStore(rdb, "testidem:"), "http://s.example", logger)
	if err != nil {
		t.Fatalf("NewURLService: %v", err)
	}
	return router.New(logger, pool, svc), pool
}

func rowCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM urls`).Scan(&n); err != nil {
		t.Fatalf("count urls: %v", err)
	}
	return n
}

func doRequest(t *testing.T, api http.Handler, method, path, body string, hdrs map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	return rec
}

func TestCreateAndGet(t *testing.T) {
	api := newAPI(t)

	rec := doRequest(t, api, http.MethodPost, "/api/v1/urls",
		`{"url":"https://example.com/abc"}`, map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", rec.Code, rec.Body.String())
	}

	var created struct {
		URL        string `json:"url"`
		ShortCode  string `json:"shortCode"`
		ShortURL   string `json:"shortUrl"`
		CreatedAt  string `json:"createdAt"`
		ExpiresAt  any    `json:"expiresAt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.URL != "https://example.com/abc" {
		t.Fatalf("url = %q", created.URL)
	}
	// first auto code in a truncated table: base62(1) = "1"
	if created.ShortCode != "1" {
		t.Fatalf("shortCode = %q, want %q", created.ShortCode, "1")
	}
	if created.ShortURL != "http://s.example/1" {
		t.Fatalf("shortUrl = %q", created.ShortURL)
	}
	if created.ExpiresAt != nil {
		t.Fatalf("expiresAt should be null, got %v", created.ExpiresAt)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("expected a request id header on the response")
	}

	rec = doRequest(t, api, http.MethodGet, "/api/v1/urls/1", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestCreateValidationErrors(t *testing.T) {
	api := newAPI(t)

	cases := []struct {
		body string
		want string
	}{
		{`{"url":""}`, "url_invalid"},
		{`{"url":"ftp://example.com/x"}`, "url_invalid"},
		{`{"url":"http://s.example/x"}`, "self_reference"},
		{`{"url":"https://example.com/x","customCode":"ab"}`, "custom_code_invalid"},
		{`{"url":"https://example.com/x","customCode":"health"}`, "custom_code_invalid"},
		{`{"url":"https://example.com/x","expiresIn":"99d"}`, ""},
		{`{"url":"https://example.com/x","expiresIn":"1x"}`, "expires_in_invalid"},
		{`not json`, "invalid_json"},
	}
	for _, c := range cases {
		rec := doRequest(t, api, http.MethodPost, "/api/v1/urls", c.body,
			map[string]string{"Content-Type": "application/json"})
		if c.want == "" {
			if rec.Code != http.StatusCreated {
				t.Errorf("body %q: expected 201, got %d (%s)", c.body, rec.Code, rec.Body.String())
			}
			continue
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: expected 400, got %d (%s)", c.body, rec.Code, rec.Body.String())
			continue
		}
		var errResp struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
			t.Fatalf("decode error body: %v", err)
		}
		if errResp.Error.Code != c.want {
			t.Errorf("body %q: error code = %q, want %q", c.body, errResp.Error.Code, c.want)
		}
	}
}

func TestCreateCustomCodeConflict(t *testing.T) {
	api := newAPI(t)
	body := `{"url":"https://example.com/1","customCode":"dup"}`
	if rec := doRequest(t, api, http.MethodPost, "/api/v1/urls", body,
		map[string]string{"Content-Type": "application/json"}); rec.Code != http.StatusCreated {
		t.Fatalf("first create: %d", rec.Code)
	}

	rec := doRequest(t, api, http.MethodPost, "/api/v1/urls", body,
		map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409", rec.Code)
	}
	var errResp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error.Code != "custom_code_taken" {
		t.Fatalf("error code = %q, want custom_code_taken", errResp.Error.Code)
	}
}

func TestRedirectFlow(t *testing.T) {
	api := newAPI(t)
	doRequest(t, api, http.MethodPost, "/api/v1/urls",
		`{"url":"https://example.com/target"}`, map[string]string{"Content-Type": "application/json"})

	rec := doRequest(t, api, http.MethodGet, "/1", "", nil)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("redirect status = %d, want 301", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://example.com/target" {
		t.Fatalf("Location = %q", loc)
	}

	if rec := doRequest(t, api, http.MethodGet, "/missing", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown code status = %d, want 404", rec.Code)
	}
}

func TestRedirectExpiredReturns410(t *testing.T) {
	api := newAPI(t)

	// Create an expired row directly (the API only accepts future expirations).
	dsn := os.Getenv("TEST_DATABASE_URL")
	p, err := repository.NewPool(context.Background(), dsn, 2)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer p.Close()
	past := time.Now().Add(-time.Hour)
	id, err := repository.NewURLRepository(p).CreateWithCode(context.Background(), "gone", "https://example.com/old", &past)
	if err != nil {
		t.Fatalf("seed expired link: %v", err)
	}
	if id == 0 {
		t.Fatal("unexpected zero id")
	}

	rec := doRequest(t, api, http.MethodGet, "/gone", "", nil)
	if rec.Code != http.StatusGone {
		t.Fatalf("expired status = %d, want 410", rec.Code)
	}
	if rec := doRequest(t, api, http.MethodGet, "/api/v1/urls/gone", "", nil); rec.Code != http.StatusGone {
		t.Fatalf("expired metadata status = %d, want 410", rec.Code)
	}
	if rec := doRequest(t, api, http.MethodGet, "/api/v1/urls/gone/stats", "", nil); rec.Code != http.StatusGone {
		t.Fatalf("expired stats status = %d, want 410", rec.Code)
	}
}

func TestDeleteFlow(t *testing.T) {
	api := newAPI(t)
	doRequest(t, api, http.MethodPost, "/api/v1/urls",
		`{"url":"https://example.com/bye"}`, map[string]string{"Content-Type": "application/json"})

	if rec := doRequest(t, api, http.MethodDelete, "/api/v1/urls/1", "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", rec.Code)
	}
	if rec := doRequest(t, api, http.MethodGet, "/1", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("after delete status = %d, want 404", rec.Code)
	}
	if rec := doRequest(t, api, http.MethodDelete, "/api/v1/urls/1", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404", rec.Code)
	}
}

func TestStatsEmptyShape(t *testing.T) {
	api := newAPI(t)
	doRequest(t, api, http.MethodPost, "/api/v1/urls",
		`{"url":"https://example.com/s"}`, map[string]string{"Content-Type": "application/json"})

	rec := doRequest(t, api, http.MethodGet, "/api/v1/urls/1/stats", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("stats status = %d", rec.Code)
	}
	var stats struct {
		TotalClicks  int64           `json:"totalClicks"`
		Daily        json.RawMessage `json:"daily"`
		TopReferrers json.RawMessage `json:"topReferrers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stats.TotalClicks != 0 {
		t.Fatalf("totalClicks = %d, want 0", stats.TotalClicks)
	}
	if string(stats.Daily) != "[]" {
		t.Fatalf("daily should be [] not null, got %s", stats.Daily)
	}
	if string(stats.TopReferrers) != "[]" {
		t.Fatalf("topReferrers should be [] not null, got %s", stats.TopReferrers)
	}
}

func TestRedirectPopulatesAndInvalidatesCache(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6389"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis %s not reachable: %v", addr, err)
	}
	defer rdb.Close()

	rdb.FlushDB(ctx)
	urlCache := cache.NewURLCache(rdb, "test-us:")
	api := newAPIServer(t, urlCache)

	hitsBefore := metrics.CacheHitCount("urllookup")
	missesBefore := metrics.CacheMissCount("urllookup")

	doRequest(t, api, http.MethodPost, "/api/v1/urls",
		`{"url":"https://example.com/cached"}`, map[string]string{"Content-Type": "application/json"})

	if rec := doRequest(t, api, http.MethodGet, "/1", "", nil); rec.Code != http.StatusMovedPermanently {
		t.Fatalf("first redirect status = %d", rec.Code)
	}
	if got := metrics.CacheMissCount("urllookup"); got != missesBefore+1 {
		t.Fatalf("expected 1 cache miss, got delta %v", got-missesBefore)
	}

	if rec := doRequest(t, api, http.MethodGet, "/1", "", nil); rec.Code != http.StatusMovedPermanently {
		t.Fatalf("second redirect status = %d", rec.Code)
	}
	if got := metrics.CacheHitCount("urllookup"); got != hitsBefore+1 {
		t.Fatalf("expected 1 cache hit, got delta %v", got-hitsBefore)
	}

	// Deleting must invalidate the cache entry, not just the DB row.
	doRequest(t, api, http.MethodDelete, "/api/v1/urls/1", "", nil)
	if u, err := urlCache.Get(ctx, "1"); err != nil || u != nil {
		t.Fatalf("cache should be invalidated after delete, got %+v err=%v", u, err)
	}
	if rec := doRequest(t, api, http.MethodGet, "/1", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("post-delete status = %d, want 404", rec.Code)
	}
}

func TestRateLimitReturns429(t *testing.T) {
	const limit = 3
	api := newRatedAPI(t, limit)

	recs := make([]*httptest.ResponseRecorder, 0, limit+1)
	for i := 0; i < limit; i++ {
		rec := doRequest(t, api, http.MethodPost, "/api/v1/urls",
			`{"url":"https://example.com/r"}`, map[string]string{"Content-Type": "application/json"})
		recs = append(recs, rec)
		if rec.Code != http.StatusCreated {
			t.Fatalf("request %d: expected 201, got %d (%s)", i+1, rec.Code, rec.Body.String())
		}
	}

	blocked := doRequest(t, api, http.MethodPost, "/api/v1/urls",
		`{"url":"https://example.com/r"}`, map[string]string{"Content-Type": "application/json"})
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d (%s)", blocked.Code, blocked.Body.String())
	}
	if blocked.Header().Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header on 429")
	}
	var errResp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(blocked.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}
	if errResp.Error.Code != "rate_limited" {
		t.Fatalf("error code = %q, want rate_limited", errResp.Error.Code)
	}

	// Reads are not rate limited: the redirect path still works.
	if rec := doRequest(t, api, http.MethodGet, "/1", "", nil); rec.Code != http.StatusMovedPermanently {
		t.Fatalf("redirect after 429 status = %d, want 301", rec.Code)
	}
}

func TestCreateIdempotencyReplay(t *testing.T) {
	api, pool := newIdemAPI(t)
	body := `{"url":"https://example.com/idem"}`
	hdr := map[string]string{"Content-Type": "application/json", "Idempotency-Key": "key-replay"}

	first := doRequest(t, api, http.MethodPost, "/api/v1/urls", body, hdr)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create status = %d (%s)", first.Code, first.Body.String())
	}
	if replay := first.Header().Get("Idempotency-Replay"); replay != "false" {
		t.Fatalf("first request Idempotency-Replay = %q, want false", replay)
	}
	var a struct {
		ShortCode string `json:"shortCode"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &a); err != nil {
		t.Fatalf("decode first: %v", err)
	}

	// Same key, same body: must return the SAME code, not allocate a new row.
	second := doRequest(t, api, http.MethodPost, "/api/v1/urls", body, hdr)
	if second.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200 (%s)", second.Code, second.Body.String())
	}
	if replay := second.Header().Get("Idempotency-Replay"); replay != "true" {
		t.Fatalf("second request Idempotency-Replay = %q, want true", replay)
	}
	var b struct {
		ShortCode string `json:"shortCode"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	if b.ShortCode != a.ShortCode {
		t.Fatalf("replayed shortCode = %q, want %q", b.ShortCode, a.ShortCode)
	}
	if n := rowCount(t, pool); n != 1 {
		t.Fatalf("urls rows = %d, want 1 (no duplicate created)", n)
	}
}

func TestCreateIdempotencyConcurrent(t *testing.T) {
	api, pool := newIdemAPI(t)
	body := `{"url":"https://example.com/idem-race"}`
	hdr := map[string]string{"Content-Type": "application/json", "Idempotency-Key": "key-race"}

	const workers = 5
	results := make([]string, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			rec := doRequest(t, api, http.MethodPost, "/api/v1/urls", body, hdr)
			var out struct {
				ShortCode string `json:"shortCode"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			results[i] = out.ShortCode
		}(i)
	}
	wg.Wait()

	for i := 1; i < workers; i++ {
		if results[i] != results[0] {
			t.Fatalf("concurrent same-key creates diverged: %v", results)
		}
	}
	if n := rowCount(t, pool); n != 1 {
		t.Fatalf("urls rows = %d, want 1 (concurrent duplicates)", n)
	}
}