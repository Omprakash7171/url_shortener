# Database Design — PostgreSQL

> Status: **v1 complete**. DDL lives in `migrations/0001_init.up.sql`.
> Every number below was **measured** on 2026-09-18 against a real PostgreSQL 17
> (container `urlshortener-pg`, 1,000,000 seeded rows) using
> `scripts/seed_explain.sql`. Nothing is estimated or copied.

## 1. Goals

1. PostgreSQL is the **system of record**. Redis is a cache and must be able to lose
   everything without losing a single mapping.
2. The redirect lookup — the hottest read — is a **single-row PK-by-index lookup**;
   the schema makes that one index scan, nothing more.
3. Writes are trivially simple (one INSERT on create), so the write path is not the
   scaling problem.
4. Analytics are stored as **pre-aggregated daily counters**, not raw events, so the
   stats endpoints are point queries, not table scans.
5. The schema is shaped so v2 ownership (auth) is additive, not a redesign.

## 2. Tables

### 2.1 `urls`

| Column | Type | Constraints | Why |
|---|---|---|---|
| `id` | `BIGINT` | `GENERATED ALWAYS AS IDENTITY`, PK | Sequence-controlled. This is the Base62 **source**: `code = base62(id)`. See D-03. |
| `code` | `TEXT` | `UNIQUE`, nullable; CHECK charset | The short code (auto Base62 or user custom). **Nullable** so a create can insert without a code and assign `base62(id)` in the same transaction (D-05). One `UNIQUE` handles both auto and custom codes. |
| `long_url` | `TEXT` | `NOT NULL`; CHECK `1..2048` | Deliberately bounded: 2048 is the common practical URL ceiling and an abuse-control bound. |
| `expires_at` | `TIMESTAMPTZ` | nullable | `NULL` = never expires. Checked at read time — **no periodic expiry job** (D-12). |
| `created_at` | `TIMESTAMPTZ` | `NOT NULL` | Audit + stats ordering. |

Deliberately **absent**: `updated_at` (no v1 update endpoint — YAGNI; added when a
re-point feature exists), `owner_id` (v2; adding it now would be speculative).

### 2.2 `analytics_daily` — pre-aggregated, dimensioned counters

| Column | Type | Constraints | Why |
|---|---|---|---|
| `url_id` | `BIGINT` | FK → `urls(id) ON DELETE CASCADE` | CASCADE: deleting a link deletes its stats (D-13). |
| `day` | `DATE` | part of PK | Time-bucket granularity. |
| `dim` | `TEXT` | CHECK `('total','referrer','device','country')` | Dimension name in one generic table instead of one table per dimension. |
| `value` | `TEXT` | part of PK | Dimension value. |
| `count` | `BIGINT` | `NOT NULL DEFAULT 0` | Accumulated by the worker via `INSERT ... ON CONFLICT DO UPDATE`. |

**Model**: one row per `(link, day, dimension, value)`. "Clicks per day" is
`dim='total'`; "top referrers" is `dim='referrer'`. The worker upserts counters, so a
link with 1M clicks produces exactly one row per active day — the stats query never
grows with event volume (D-04).

### 2.3 `idempotency_records` (schema now, implementation Stage 14.5)

| Column | Type | Constraints | Why |
|---|---|---|---|
| `key`, `scope` | `TEXT` | composite PK | A key is only unique **within a scope** (endpoint), avoiding cross-endpoint collisions. |
| `response` | `JSONB` | `NOT NULL` | The exact created response to replay on a retry. |
| `created_at`, `expires_at` | `TIMESTAMPTZ` | — | Retention. Purge runs on `(expires_at)` below. |

## 3. Index strategy (each one justified, with evidence)

PostgreSQL indexes are not free: every write must maintain them. The rule applied:
**an index must serve a query that exists in v1, or it is not created.** Both halves
of that rule are tested below.

### I1 — PK `urls(id)` (implicit)
- Serves: nothing on the hot path; used by analytics FK joins and `WHERE id = $1`.
- The identity/sequence itself.
- Write cost: one B-tree node entry per insert (unavoidable — it is the table's heap
  order anchor for identity).

### I2 — `urls_code_key` UNIQUE (on `code`)
- Serves: **every redirect and API lookup** (`WHERE code = $1`), plus collision
  detection for custom codes at insert time.
- Measured impact on 1M rows:

| Query | Plan | Execution | Buffers |
|---|---|---|---|
| `WHERE code='c500000'` **with** index | `Index Scan using urls_code_key` | **0.041 ms** | 4 |
| `WHERE code='c500000'` **no** index (same data, `urls_plain`) | `Parallel Seq Scan` (3 workers, 333k rows filtered each) | **23.777 ms** | 10,368 |
| Insert (indexed `urls`) | `Insert on urls` | **0.121 ms** | 9 |
| Insert (no-index `urls_plain`) | `Insert on urls_plain` | **0.044 ms** | 1 |

- The lookup speedup is **~580×** on this hardware; the insert penalty is
  **+0.077 ms** (~1.9× on a 0.12 ms operation that is off the hot path by design).
- A duplicate `code` insert is rejected by this same index *before any row is
  written* (repro: script block **F**) — collision detection is O(log n) index probe,
  not a scan.

### I3 — PK `analytics_daily(url_id, day, dim, value)`
- Serves all three v1 stats queries via its `(url_id)` prefix — measured:

| Query | Plan | Execution |
|---|---|---|
| Daily series (`url_id=1, dim='total'`) | Bitmap index/heap scan on PK | **0.091 ms** (60 rows) |
| Top-10 referrers (group by + order by count desc) | Index scan + in-memory sort | **0.171 ms** (`HashAggregate`, 300 rows scanned → 10 sorted) |
| Lifetime total clicks | index scan + aggregate | **0.094 ms** |

- The top-referrer query sorts (10 rows) — the sort is in-memory and costs ~0.02 ms;
  a `(url_id, count DESC)` partial index is **not** added today because per-link row
  counts are small. That decision is revisited in PERFORMANCE.md only if measured
  stats latency demands it.
- Write cost: one B-tree entry per upsert; the worker batches these.

### I4 — `idx_idempotency_records_expires_at`
- Serves the periodic purge (`DELETE ... WHERE expires_at < now()`). Without it the
  purge is a table scan of the whole idempotency table.
- Write cost: negligible (table written only during creates).

## 4. Deliberately NOT indexed

- **`long_url`** — no v1 query filters by long URL. Lookup cost without an index is
  measured at **27.978 ms** (1M rows, seq scan). If v2 adds "find my link by URL",
  this needs an index — or better, a `long_url_hash` column (see DECISIONS D-11).
- **`expires_at`** — nothing queries "links expiring soon"; expiry is enforced at
  read time. An index would be pure write overhead.
- **`urls.created_at`** — no sorted/range query on it in v1.

These three are the "common-but-unjustified indexes" the brief warns about — each is
documented with the query it *would* serve and why that query was not built.

## 5. Referential integrity

- `analytics_daily.url_id` FK → `urls.id` `ON DELETE CASCADE`: deleting a mapping
  must not leave orphaned counters. Trade-off (D-13): we lose click history on delete
  (accepted for v1; soft-delete is the v2 alternative).

## 6. Migrations

- Applied by a small **embedded migrator** (`cmd/migrate`) shipped inside the API
  binary: migration files are `//go:embed`-ed, applied in lexicographic order, each in
  a transaction guarded by `pg_advisory_lock` so multiple API instances starting
  concurrently cannot double-apply. Down versions exist for dev but are never run in
  production automatically. (D-10.)

## 7. Connection management

- `pgxpool` (pgx v5) with env-driven `max_conns`/`min_conns`. Pool sizing is a
  tuning decision deferred to Stage 23 — the pool is instrumented (`database_query_duration`)
  before any number is tuned, so tuning is benchmark-driven.

## 8. What this design gives up

- Click-history granularity past daily (raw events are not kept in PG by default).
- No expiring-link cleanup job (expiry returns `410` at read; dead rows are cheap).
- No multi-tenant isolation (v2).
- No `long_url` dedup (two people may create the same destination in v1 — the second
  link is not a violation).

## 9. Reproduction

```
docker exec -i urlshortener-pg psql -U urlshortener -d urlshortener < scripts/seed_explain.sql
```

The script truncates and reseeds 1M rows, then prints every plan above. Result
tables, hardware, and date are recorded so the numbers are auditable.