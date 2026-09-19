package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"urlshortener/internal/model"
	"urlshortener/internal/repository"
)

type fakeStore struct {
	mu      sync.Mutex
	next    int64
	pending map[int64]*model.URL
	rows    map[string]*model.URL
}

func newFakeStore() *fakeStore {
	return &fakeStore{pending: map[int64]*model.URL{}, rows: map[string]*model.URL{}}
}

func (f *fakeStore) Create(ctx context.Context, longURL string, expiresAt *time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.pending[f.next] = &model.URL{ID: f.next, LongURL: longURL, ExpiresAt: expiresAt, CreatedAt: time.Now().UTC()}
	return f.next, nil
}

func (f *fakeStore) CreateWithCode(ctx context.Context, code, longURL string, expiresAt *time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.rows[code]; exists {
		return 0, repository.ErrConflict
	}
	f.next++
	f.rows[code] = &model.URL{ID: f.next, Code: code, LongURL: longURL, ExpiresAt: expiresAt, CreatedAt: time.Now().UTC()}
	return f.next, nil
}

func (f *fakeStore) SetCode(ctx context.Context, id int64, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.rows[code]; exists {
		return repository.ErrConflict
	}
	p, ok := f.pending[id]
	if !ok {
		return repository.ErrNotFound
	}
	p.Code = code
	f.rows[code] = p
	delete(f.pending, id)
	return nil
}

func (f *fakeStore) GetByCode(ctx context.Context, code string) (*model.URL, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.rows[code]
	if !ok {
		return nil, repository.ErrNotFound
	}
	return u, nil
}

func (f *fakeStore) DeleteByCode(ctx context.Context, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[code]; !ok {
		return repository.ErrNotFound
	}
	delete(f.rows, code)
	return nil
}

func (f *fakeStore) StatsByURLID(ctx context.Context, urlID int64) (*model.StatsSummary, error) {
	return &model.StatsSummary{Daily: []model.DailyCount{}, TopReferrers: []model.ReferrerCount{}}, nil
}

func newTestService(t *testing.T, store URLStore) *URLService {
	t.Helper()
	svc, err := NewURLService(store, nil, nil, "http://s.example", nil)
	if err != nil {
		t.Fatalf("NewURLService: %v", err)
	}
	return svc
}

func TestCreateAutoGeneratesBase62Code(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	res, err := svc.Create(context.Background(), CreateParams{URL: "https://example.com/abc"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.URL.Code != "1" {
		t.Fatalf("first auto code = %q, want %q (base62 of id 1)", res.URL.Code, "1")
	}
	if res.ShortURL != "http://s.example/1" {
		t.Fatalf("short url = %q, want %q", res.ShortURL, "http://s.example/1")
	}
}

func TestCreateCustomCode(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	res, err := svc.Create(context.Background(), CreateParams{URL: "https://example.com/1", CustomCode: "my-link"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.URL.Code != "my-link" {
		t.Fatalf("code = %q, want my-link", res.URL.Code)
	}
}

func TestCreateCustomCodeTaken(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	ctx := context.Background()
	if _, err := svc.Create(ctx, CreateParams{URL: "https://example.com/1", CustomCode: "taken"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := svc.Create(ctx, CreateParams{URL: "https://example.com/2", CustomCode: "taken"})
	if !errors.Is(err, ErrCodeTaken) {
		t.Fatalf("expected ErrCodeTaken, got %v", err)
	}
}

func TestCreateRejectsOwnDomain(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	cases := []string{
		"http://s.example/x",
		"https://S.EXAMPLE/x", // case-insensitive
	}
	for _, c := range cases {
		_, err := svc.Create(context.Background(), CreateParams{URL: c})
		if !errors.Is(err, ErrSelfReference) {
			t.Errorf("URL %q: expected ErrSelfReference, got %v", c, err)
		}
	}
}

func TestCreateRejectsInvalidURLs(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	cases := []string{
		"",
		"http://",
		"ftp://example.com/file",
		"not a url at all",
		strings.Repeat("a", 2050),
	}
	for _, c := range cases {
		_, err := svc.Create(context.Background(), CreateParams{URL: c})
		if err == nil {
			t.Errorf("URL %q: expected an error", c)
		}
		if errors.Is(err, ErrSelfReference) || errors.Is(err, ErrCodeTaken) {
			t.Errorf("URL %q: wrong error class %v", c, err)
		}
	}
}

func TestCreateRejectsInvalidCustomCode(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	cases := []string{"ab", "health", "has space", "x!nvalid", "aVeryVeryLongCodeThatExceedsThirtyTwoCharacters"}
	for _, c := range cases {
		_, err := svc.Create(context.Background(), CreateParams{URL: "https://example.com/x", CustomCode: c})
		if !errors.Is(err, ErrInvalidCode) {
			t.Errorf("code %q: expected ErrInvalidCode, got %v", c, err)
		}
	}
}

func TestCreateExpiryParsing(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	before := time.Now().UTC()
	res, err := svc.Create(context.Background(), CreateParams{URL: "https://example.com/x", ExpiresIn: "7d"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	after := time.Now().UTC()
	if res.URL.ExpiresAt == nil {
		t.Fatal("expected expires_at set")
	}
	want := before.Add(7 * 24 * time.Hour)
	if res.URL.ExpiresAt.Before(want.Add(-time.Minute)) || res.URL.ExpiresAt.After(after.Add(7*24*time.Hour)) {
		t.Fatalf("expiry out of range: %v (window %v..%v)", res.URL.ExpiresAt, want, after.Add(7*24*time.Hour))
	}

	for _, bad := range []string{"1x", "366d", "0h", "-1d", "7"} {
		_, err := svc.Create(context.Background(), CreateParams{URL: "https://example.com/x", ExpiresIn: bad})
		if !errors.Is(err, ErrInvalidExpiry) {
			t.Errorf("expiry %q: expected ErrInvalidExpiry, got %v", bad, err)
		}
	}
}

func TestResolveExpiredLink(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(t, store)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	if _, err := store.CreateWithCode(ctx, "old", "https://example.com/old", &past); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := svc.Resolve(ctx, "old"); !errors.Is(err, ErrExpired) {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
	if _, err := svc.Resolve(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestURLServiceNormalizesScheme(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(t, store)
	res, err := svc.Create(context.Background(), CreateParams{URL: "example.com/no-scheme"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.URL.LongURL != "http://example.com/no-scheme" {
		t.Fatalf("expected http:// prefix, got %q", res.URL.LongURL)
	}
}