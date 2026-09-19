// Package consumer drains the analytics event stream and aggregates events into
// PostgreSQL with effectively-once semantics (consumer-group at-least-once plus
// an idempotency record per event).
package consumer

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"urlshortener/internal/metrics"
)

const (
	groupName   = "analytics"
	consumerID  = "worker-0"
	batchSize   = 32
	blockMillis = 2000
	// idempotencyTTL bounds how long dedupe keys are remembered. Stream ids are
	// globally unique for the stream's lifetime, so this is belt-and-braces.
	idempotencyTTL = 24 * time.Hour
)

type Consumer struct {
	rdb    redis.UniversalClient
	stream string
	pg     *pgx.Conn
	logger *slog.Logger
}

func New(rdb redis.UniversalClient, stream string, pg *pgx.Conn, logger *slog.Logger) *Consumer {
	return &Consumer{rdb: rdb, stream: stream, pg: pg, logger: logger}
}

// EnsureGroup creates the consumer group once (idempotent across restarts and
// multiple worker instances).
func (c *Consumer) EnsureGroup(ctx context.Context) error {
	err := c.rdb.XGroupCreateMkStream(ctx, c.stream, groupName, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("create group: %w", err)
	}
	return nil
}

// Run blocks, draining the stream; it returns when ctx is cancelled.
func (c *Consumer) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		n, err := c.ProcessOnce(ctx, true)
		if err != nil {
			c.logger.Error("consume batch failed", "err", err)
		}
		if n == 0 {
			// Give the stream time to fill before the next read.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

// ProcessOnce reads and processes one batch. With block=true it waits up to
// blockMillis for entries. It returns the number of entries processed. Separate
// from Run so tests can drive a single batch.
func (c *Consumer) ProcessOnce(ctx context.Context, block bool) (int, error) {
	args := &redis.XReadGroupArgs{
		Group:    groupName,
		Consumer: consumerID,
		Streams:  []string{c.stream, ">"},
		Count:    batchSize,
	}
	if block {
		args.Block = blockMillis * time.Millisecond
	}
	streams, err := c.rdb.XReadGroup(ctx, args).Result()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	processed := 0
	for _, stream := range streams {
		for _, msg := range stream.Messages {
			if err := c.handle(ctx, msg); err != nil {
				return processed, fmt.Errorf("handle %s: %w", msg.ID, err)
			}
			processed++
		}
	}
	return processed, nil
}

func (c *Consumer) handle(ctx context.Context, msg redis.XMessage) error {
	code, _ := msg.Values["code"].(string)
	if code == "" {
		return nil // malformed event; ack it
	}
	referrer, _ := msg.Values["referrer"].(string)

	processed, err := c.claim(ctx, msg.ID)
	if err != nil {
		return err
	}
	if !processed {
		metrics.AnalyticsDeduped.Inc()
		return c.ack(ctx, msg.ID)
	}

	day, err := eventDay(msg)
	if err != nil {
		return fmt.Errorf("bad ts: %w", err)
	}

	var urlID int64
	if err := c.pg.QueryRow(ctx,
		`SELECT id FROM urls WHERE code = $1`,
		code,
	).Scan(&urlID); err != nil {
		if err == pgx.ErrNoRows {
			metrics.AnalyticsDeduped.Inc()
			return c.ack(ctx, msg.ID)
		}
		return fmt.Errorf("lookup code: %w", err)
	}

	if _, err := c.pg.Exec(ctx,
		`INSERT INTO analytics_daily (url_id, day, dim, value, count)
		 VALUES ($1, $2, 'total', 'clicks', 1)
		 ON CONFLICT (url_id, day, dim, value)
		 DO UPDATE SET count = analytics_daily.count + 1`,
		urlID, day,
	); err != nil {
		return fmt.Errorf("upsert total: %w", err)
	}

	if referrer != "" {
		if _, err := c.pg.Exec(ctx,
			`INSERT INTO analytics_daily (url_id, day, dim, value, count)
			 VALUES ($1, $2, 'referrer', $3, 1)
			 ON CONFLICT (url_id, day, dim, value)
			 DO UPDATE SET count = analytics_daily.count + 1`,
			urlID, day, referrer,
		); err != nil {
			return fmt.Errorf("upsert referrer: %w", err)
		}
	}

	metrics.AnalyticsProcessed.Inc()
	return c.ack(ctx, msg.ID)
}

// claim records the event id in the idempotency table, returning true only the
// first time a given id is seen.
func (c *Consumer) claim(ctx context.Context, id string) (bool, error) {
	tag, err := c.pg.Exec(ctx,
		`INSERT INTO idempotency_records (key, scope, response, expires_at)
		 VALUES ($1, 'analytics', '{}', now() + $2::interval)
		 ON CONFLICT (key, scope) DO NOTHING`,
		id, idempotencyTTL.String(),
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (c *Consumer) ack(ctx context.Context, id string) error {
	return c.rdb.XAck(ctx, c.stream, groupName, id).Err()
}

func eventDay(msg redis.XMessage) (string, error) {
	raw, _ := msg.Values["ts"].(string)
	sec, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		sec = time.Now().Unix()
	}
	return time.Unix(sec, 0).UTC().Format("2006-01-02"), nil
}