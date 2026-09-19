package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"urlshortener/internal/cache"
	"urlshortener/internal/idempotency"
	"urlshortener/internal/metrics"
	"urlshortener/internal/model"
	"urlshortener/internal/repository"
)

var (
	ErrNotFound      = errors.New("url: not found")
	ErrExpired       = errors.New("url: expired")
	ErrInvalidURL    = errors.New("url: invalid")
	ErrURLTooLong    = errors.New("url: too long")
	ErrSelfReference = errors.New("url: points at own domain")
	ErrInvalidCode   = errors.New("url: invalid custom code")
	ErrInvalidExpiry = errors.New("url: invalid expiry")
	ErrCodeTaken     = errors.New("url: custom code already taken")
	ErrInFlight      = errors.New("url: idempotent create already in progress")
	ErrIdemKeyTooLong = errors.New("url: Idempotency-Key too long")
)

const (
	maxURLLength    = 2048
	MaxBodyBytes    = 1 << 20
	maxExpiry       = 365 * 24 * time.Hour
	minCustomCode   = 3
	maxCustomCode   = 32
	maxIdemKeyLength = 256
	// idemWaitPolls / idemWaitStep bound how long we wait for a concurrent
	// request that claimed the same key to finish before returning ErrInFlight.
	idemWaitPolls = 5
	idemWaitStep  = 200 * time.Millisecond
)

var (
	customCodeRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{` + strconv.Itoa(minCustomCode) + `,` + strconv.Itoa(maxCustomCode) + `}$`)
	expiryRe       = regexp.MustCompile(`^(\d+)([mhd])$`)
	reservedCodes  = map[string]struct{}{"api": {}, "health": {}, "ready": {}, "metrics": {}}
)

type URLStore interface {
	Create(ctx context.Context, longURL string, expiresAt *time.Time) (int64, error)
	CreateWithCode(ctx context.Context, code, longURL string, expiresAt *time.Time) (int64, error)
	SetCode(ctx context.Context, id int64, code string) error
	GetByCode(ctx context.Context, code string) (*model.URL, error)
	DeleteByCode(ctx context.Context, code string) error
	StatsByURLID(ctx context.Context, urlID int64) (*model.StatsSummary, error)
}

type CreateParams struct {
    URL            string
    CustomCode     string
    ExpiresIn      string
    IdempotencyKey string
}

type CreateResult struct {
	URL      *model.URL
	ShortURL string
	// Replayed is true when the caller sent an Idempotency-Key that matched an
	// earlier create: the URL shown is the original one, nothing new was written.
	Replayed bool
}

type URLService struct {
	store       URLStore
	cache       *cache.URLCache
	idem        *idempotency.Store
	baseURL     *url.URL
	blockDomain string
	logger      *slog.Logger
}

// NewURLService constructs the service. cache and idem may be nil (disabled):
// without cache the redirect path always reads PostgreSQL; without idem the
// create endpoint ignores Idempotency-Key headers.
func NewURLService(store URLStore, cacheSvc *cache.URLCache, idem *idempotency.Store, baseURL string, logger *slog.Logger) (*URLService, error) {
	bu, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url %q: %w", baseURL, err)
	}
	if bu.Scheme != "http" && bu.Scheme != "https" {
		return nil, fmt.Errorf("base url must be http(s)")
	}
	if bu.Host == "" {
		return nil, fmt.Errorf("base url has no host")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &URLService{store: store, cache: cacheSvc, idem: idem, baseURL: bu, blockDomain: bu.Host, logger: logger}, nil
}

func (s *URLService) Create(ctx context.Context, in CreateParams) (*CreateResult, error) {
	longURL, err := s.validateLongURL(in.URL)
	if err != nil {
		return nil, err
	}

	var expiresAt *time.Time
	if in.ExpiresIn != "" {
		expiresAt, err = parseExpiry(in.ExpiresIn)
		if err != nil {
			return nil, err
		}
	}

	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.IdempotencyKey != "" {
		return s.createIdempotent(ctx, in, longURL, expiresAt)
	}

	res, err := s.createOne(ctx, in, longURL, expiresAt)
	if err != nil {
		return nil, err
	}
	return &CreateResult{URL: res.URL, ShortURL: res.ShortURL}, nil
}

// createIdempotent guards createOne with the Redis idempotency store, or fails
// through to a plain create when the store is unavailable (log + create).
func (s *URLService) createIdempotent(ctx context.Context, in CreateParams, longURL string, expiresAt *time.Time) (*CreateResult, error) {
	if len(in.IdempotencyKey) > maxIdemKeyLength {
		return nil, ErrIdemKeyTooLong
	}
	if s.idem == nil {
		return s.createOne(ctx, in, longURL, expiresAt)
	}

	if code, ok, err := s.idem.Get(ctx, in.IdempotencyKey); err == nil {
		if ok {
			metrics.IdempotencyReplays.Inc()
			return s.replay(ctx, code)
		}

		claimed, err := s.idem.Claim(ctx, in.IdempotencyKey)
		if err != nil {
			s.logger.Warn("idempotency store unavailable, creating without dedupe", "err", err)
			metrics.IdempotencySkipped.Inc()
			return s.createOne(ctx, in, longURL, expiresAt)
		}
		if claimed {
			res, err := s.createOne(ctx, in, longURL, expiresAt)
			if err != nil {
				// Leave the lock to expire on its own; nothing recorded.
				return nil, err
			}
			if serr := s.idem.Set(ctx, in.IdempotencyKey, res.URL.Code); serr != nil {
				s.logger.Warn("idempotency record failed", "err", serr)
			}
			_ = s.idem.Release(ctx, in.IdempotencyKey)
			return res, nil
		}
	} else if err != nil {
		s.logger.Warn("idempotency store read failed, creating without dedupe", "err", err)
		metrics.IdempotencySkipped.Inc()
		return s.createOne(ctx, in, longURL, expiresAt)
	}

	// Another request claims the key and is creating right now: wait briefly
	// for its result, then admit defeat.
	for i := 0; i < idemWaitPolls; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		time.Sleep(idemWaitStep)
		if code, ok, err := s.idem.Get(ctx, in.IdempotencyKey); err == nil && ok {
			metrics.IdempotencyReplays.Inc()
			return s.replay(ctx, code)
		}
	}
	return nil, ErrInFlight
}

// replay rebuilds the create result for an already-stored idempotency key from
// the persistent URL row (the single source of truth for short->long mapping).
func (s *URLService) replay(ctx context.Context, code string) (*CreateResult, error) {
	u, err := s.store.GetByCode(ctx, code)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// The recorded code was deleted since creation; nothing meaningful
			// to replay. Surface it as a normal miss.
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &CreateResult{URL: u, ShortURL: s.shortURLFor(u.Code), Replayed: true}, nil
}

func (s *URLService) createOne(ctx context.Context, in CreateParams, longURL string, expiresAt *time.Time) (*CreateResult, error) {
	var code string
	if in.CustomCode != "" {
		if err := validateCustomCode(in.CustomCode); err != nil {
			return nil, err
		}
		if _, err := s.store.CreateWithCode(ctx, in.CustomCode, longURL, expiresAt); err != nil {
			if errors.Is(err, repository.ErrConflict) {
				return nil, fmt.Errorf("%w: %q", ErrCodeTaken, in.CustomCode)
			}
			return nil, err
		}
		code = in.CustomCode
	} else {
		id, err := s.store.Create(ctx, longURL, expiresAt)
		if err != nil {
			return nil, err
		}
		code = EncodeBase62(uint64(id))
		if err := s.store.SetCode(ctx, id, code); err != nil {
			if errors.Is(err, repository.ErrConflict) {
				return nil, fmt.Errorf("codegen collision (database state inconsistent): %w", err)
			}
			return nil, err
		}
	}

	created, err := s.store.GetByCode(ctx, code)
	if err != nil {
		return nil, err
	}
	return &CreateResult{URL: created, ShortURL: s.shortURLFor(code), Replayed: false}, nil
}

func (s *URLService) Resolve(ctx context.Context, code string) (*model.URL, error) {
	if s.cache != nil {
		if cached, err := s.cache.Get(ctx, code); err == nil && cached != nil {
			if cached.ExpiresAt != nil && !cached.ExpiresAt.After(time.Now()) {
				_ = s.cache.Delete(ctx, code) // expired entry, evict and fall through
			} else {
				return cached, nil
			}
		} else if err != nil {
			metrics.CacheFailOpen.WithLabelValues("urllookup").Inc()
			s.logger.Warn("url cache read failed, failing open", "code", code, "err", err)
		}
	}

	u, err := s.store.GetByCode(ctx, code)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if u.ExpiresAt != nil && !u.ExpiresAt.After(time.Now()) {
		return nil, ErrExpired
	}
	if s.cache != nil {
		se := u
		if err := s.cache.Set(ctx, code, se); err != nil {
			s.logger.Warn("url cache write failed", "code", code, "err", err)
		}
	}
	return u, nil
}

func (s *URLService) Delete(ctx context.Context, code string) error {
	err := s.store.DeleteByCode(ctx, code)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if s.cache != nil {
		if err := s.cache.Delete(ctx, code); err != nil {
			s.logger.Warn("url cache delete failed", "code", code, "err", err)
		}
	}
	return nil
}

func (s *URLService) GetStats(ctx context.Context, code string) (*model.StatsSummary, error) {
	u, err := s.Resolve(ctx, code)
	if err != nil {
		return nil, err
	}
	return s.store.StatsByURLID(ctx, u.ID)
}

func (s *URLService) ShortURLFor(code string) string {
	return s.shortURLFor(code)
}

func (s *URLService) shortURLFor(code string) string {
	return s.baseURL.JoinPath(code).String()
}

func (s *URLService) validateLongURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrInvalidURL
	}
	if len(raw) > maxURLLength {
		return "", ErrURLTooLong
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", ErrInvalidURL
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", ErrInvalidURL
	}
	if u.Host == "" {
		return "", ErrInvalidURL
	}
	if strings.EqualFold(u.Host, s.blockDomain) {
		return "", ErrSelfReference
	}
	return u.String(), nil
}

func validateCustomCode(code string) error {
	if !customCodeRe.MatchString(code) {
		return ErrInvalidCode
	}
	if _, reserved := reservedCodes[strings.ToLower(code)]; reserved {
		return ErrInvalidCode
	}
	return nil
}

func parseExpiry(in string) (*time.Time, error) {
	m := expiryRe.FindStringSubmatch(in)
	if m == nil {
		return nil, ErrInvalidExpiry
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return nil, ErrInvalidExpiry
	}
	var d time.Duration
	switch m[2] {
	case "m":
		d = time.Duration(n) * time.Minute
	case "h":
		d = time.Duration(n) * time.Hour
	case "d":
		d = time.Duration(n) * 24 * time.Hour
	}
	if d > maxExpiry || d <= 0 {
		return nil, ErrInvalidExpiry
	}
	t := time.Now().Add(d).UTC()
	return &t, nil
}

