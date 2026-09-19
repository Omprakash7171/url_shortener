// Package idempotency makes POST /api/v1/urls idempotent for clients that send an
// Idempotency-Key header. Redis is the coordination and replay store: the first
// create claims the key, the winning request records the resulting short code,
// and later (or concurrent) requests with the same key get the original code back.
package idempotency

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// respTTL bounds how long a remembered create response can be replayed.
	respTTL = 24 * time.Hour
	// lockTTL bounds how long an in-flight create claim lives; it auto-expires
	// so a crashed creator never blocks the key forever.
	lockTTL = 10 * time.Second
	// opTimeout caps every Redis operation so an unavailable store degrades
	// fast instead of stalling the request (fail-open is the caller's call).
	opTimeout = 200 * time.Millisecond
)

// Store coordinates idempotent creates in Redis.
type Store struct {
	rdb     redis.UniversalClient
	keyBase string
	RespTTL time.Duration
	LockTTL time.Duration
}

func NewStore(rdb redis.UniversalClient, keyBase string) *Store {
	return &Store{rdb: rdb, keyBase: keyBase, RespTTL: respTTL, LockTTL: lockTTL}
}

func (s *Store) respKey(key string) string { return s.keyBase + "idem:resp:" + key }
func (s *Store) lockKey(key string) string { return s.keyBase + "idem:lock:" + key }

// Get returns the short code already produced for an idempotency key. ok=false
// means the key is unknown. A returned error means the store is unavailable and
// the caller decides how to degrade.
func (s *Store) Get(ctx context.Context, key string) (code string, ok bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	raw, err := s.rdb.Get(ctx, s.respKey(key)).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return raw, true, nil
}

// Set records the short code produced for an idempotency key.
func (s *Store) Set(ctx context.Context, key, code string) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	return s.rdb.Set(ctx, s.respKey(key), code, s.RespTTL).Err()
}

// Claim atomically claims the key for an in-flight create (SET NX + TTL).
// Returns true when this caller is the creator, false when another request
// already owns the claim.
func (s *Store) Claim(ctx context.Context, key string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	return s.rdb.SetNX(ctx, s.lockKey(key), "1", s.LockTTL).Result()
}

// Release drops the in-flight claim after the response was recorded. Safe to
// call blindly: the lock also expires by itself (LockTTL).
func (s *Store) Release(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	return s.rdb.Del(ctx, s.lockKey(key)).Err()
}