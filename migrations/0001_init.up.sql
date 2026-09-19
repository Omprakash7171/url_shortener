-- 0001_init.up.sql
-- Initial schema for the URL shortener.
-- Every decision is justified in docs/03-database.md and docs/DECISIONS.md.
-- The migrator wraps this file in its own transaction, guarded by
-- pg_advisory_lock, so no explicit BEGIN/COMMIT here.

CREATE TABLE IF NOT EXISTS urls (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    code       TEXT UNIQUE,
    long_url   TEXT        NOT NULL,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT urls_code_format     CHECK (code IS NULL OR code ~ '^[A-Za-z0-9_-]{1,32}$'),
    CONSTRAINT urls_long_url_length CHECK (char_length(long_url) BETWEEN 1 AND 2048)
);

CREATE TABLE IF NOT EXISTS analytics_daily (
    url_id BIGINT  NOT NULL REFERENCES urls(id) ON DELETE CASCADE,
    day    DATE    NOT NULL,
    dim    TEXT    NOT NULL,
    value  TEXT    NOT NULL,
    count  BIGINT  NOT NULL DEFAULT 0,
    PRIMARY KEY (url_id, day, dim, value),

    CONSTRAINT analytics_daily_dim_check CHECK (dim IN ('total', 'referrer', 'device', 'country'))
);

CREATE TABLE IF NOT EXISTS idempotency_records (
    key        TEXT        NOT NULL,
    scope      TEXT        NOT NULL,
    response   JSONB       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (key, scope)
);

-- Used by the periodic purge of expired idempotency records.
CREATE INDEX IF NOT EXISTS idx_idempotency_records_expires_at
    ON idempotency_records (expires_at);