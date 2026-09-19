-- scripts/seed_explain.sql
-- Reproducible seed + EXPLAIN ANALYZE measurements for docs/03-database.md.
-- Idempotent: safe to re-run.
-- Run against the dev database:
--   docker exec -i urlshortener-pg psql -U urlshortener -d urlshortener < scripts/seed_explain.sql

TRUNCATE urls RESTART IDENTITY CASCADE;
DROP TABLE IF EXISTS urls_plain;

-- 1. Seed 1M URL mappings (indexed table) -------------------------------------
INSERT INTO urls (code, long_url)
SELECT 'c' || i, 'https://example.com/p/' || i
FROM generate_series(1, 1000000) AS i;

-- 2. Unindexed copy: proves what the unique index buys vs. a seq scan ----------
CREATE TABLE urls_plain AS SELECT * FROM urls;

ANALYZE urls;
ANALYZE urls_plain;

\echo '=== A. code lookup: WITH unique index (the redirect hot path) ==='
EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM urls WHERE code = 'c500000';

\echo '=== B. code lookup: same data, NO index (seq scan) ==='
EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM urls_plain WHERE code = 'c500000';

\echo '=== C. long_url lookup: no index. Deliberately NOT a supported query in v1. ==='
EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM urls WHERE long_url = 'https://example.com/p/500000';

-- 3. Insert cost with vs. without the unique index ------------------------------
\echo '=== D. INSERT: index maintenance cost (indexed table) ==='
EXPLAIN (ANALYZE, BUFFERS) INSERT INTO urls (code, long_url) VALUES ('newcode1', 'https://example.com/new/1');
\echo '=== E. INSERT: no index to maintain (plain table) ==='
EXPLAIN (ANALYZE, BUFFERS) INSERT INTO urls_plain (code, long_url) VALUES ('newcode2', 'https://example.com/new/2');

\echo '=== F. duplicate custom code: the unique index detects the collision (statement aborts) ==='
INSERT INTO urls (code, long_url) VALUES ('c500000', 'https://example.com/dup/');

-- 4. Analytics seed (distinct (day, value) pairs so no PK collision) + plans ---
INSERT INTO analytics_daily (url_id, day, dim, value, count)
SELECT u.id, current_date - g, 'total', 'total', g + 1
FROM urls u, generate_series(0, 59) AS g
WHERE u.id <= 5;

INSERT INTO analytics_daily (url_id, day, dim, value, count)
SELECT u.id, current_date - (g % 30), 'referrer', 'ref' || (g / 30), (g % 1000) + 1
FROM urls u, generate_series(0, 299) AS g
WHERE u.id <= 5;

ANALYZE analytics_daily;

\echo '=== G. stats: daily series for one link (PK prefix url_id + day) ==='
EXPLAIN (ANALYZE) SELECT day, count FROM analytics_daily WHERE url_id = 1 AND dim = 'total' ORDER BY day;

\echo '=== H. stats: top-10 referrers for one link ==='
EXPLAIN (ANALYZE) SELECT value, sum(count) AS total
FROM analytics_daily
WHERE url_id = 1 AND dim = 'referrer'
GROUP BY value ORDER BY total DESC LIMIT 10;

\echo '=== I. stats: lifetime total clicks for one link ==='
EXPLAIN (ANALYZE) SELECT coalesce(sum(count), 0) FROM analytics_daily WHERE url_id = 1 AND dim = 'total';