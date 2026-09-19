package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"urlshortener/internal/model"
)

var (
	ErrNotFound = errors.New("url: not found")
	ErrConflict = errors.New("url: conflict")
)

type URLRepository struct {
	pool *pgxpool.Pool
}

func NewURLRepository(pool *pgxpool.Pool) *URLRepository {
	return &URLRepository{pool: pool}
}

func (r *URLRepository) Create(ctx context.Context, longURL string, expiresAt *time.Time) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx,
		`INSERT INTO urls (long_url, expires_at) VALUES ($1, $2) RETURNING id`,
		longURL, expiresAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert url: %w", err)
	}
	return id, nil
}

func (r *URLRepository) CreateWithCode(ctx context.Context, code, longURL string, expiresAt *time.Time) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx,
		`INSERT INTO urls (code, long_url, expires_at) VALUES ($1, $2, $3) RETURNING id`,
		code, longURL, expiresAt,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("%w: code %q", ErrConflict, code)
		}
		return 0, fmt.Errorf("insert url with code: %w", err)
	}
	return id, nil
}

func (r *URLRepository) SetCode(ctx context.Context, id int64, code string) error {
	ct, err := r.pool.Exec(ctx,
		`UPDATE urls SET code = $1 WHERE id = $2`,
		code, id,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: code %q", ErrConflict, code)
		}
		return fmt.Errorf("set code: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w: id %d", ErrNotFound, id)
	}
	return nil
}

func (r *URLRepository) GetByCode(ctx context.Context, code string) (*model.URL, error) {
	var u model.URL
	err := r.pool.QueryRow(ctx,
		`SELECT id, code, long_url, expires_at, created_at FROM urls WHERE code = $1`,
		code,
	).Scan(&u.ID, &u.Code, &u.LongURL, &u.ExpiresAt, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get url by code: %w", err)
	}
	return &u, nil
}

func (r *URLRepository) DeleteByCode(ctx context.Context, code string) error {
	ct, err := r.pool.Exec(ctx, `DELETE FROM urls WHERE code = $1`, code)
	if err != nil {
		return fmt.Errorf("delete url: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}