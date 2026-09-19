# Architecture — Distributed URL Shortening & Analytics Service

> Status: **v1 draft — pending review**. Edition of `docs/02-architecture.md`.
> Decision records are consolidated in `docs/DECISIONS.md` as the build progresses.

## 1. System context

```
                    ┌───────────────┐
                    │    Client     │
                    └──────┬────────┘
                           │ HTTP
                           ▼
                    ┌───────────────┐
                    │    Nginx     │  reverse proxy + load balancer + TLS term
                    └──────┬────────┘
             ┌─────────────┼──────────────┐
             ▼             ▼              ▼
       ┌───────────┐ ┌───────────┐ ┌───────────┐
       │  api :1   │ │  api :2   │ │  api :3   │   stateless Go (chi) instances
       └─────┬─────┘ └─────┬─────┘ └─────┬─────┘
             └─────────────┼─────────────┘
                           │
              ┌────────────┴───────────┐
              │          Redis         │   cache + stream + rate limits
              └────────────┬───────────┘
                           │
              ┌────────────┴───────────┐
              │       PostgreSQL       │   system of record
              └────────────┬───────────┘
                           │
                    ┌──────┴──────┐
                    │   Worker    │   consumes stream → aggregates → PG
                    └─────────────┘
```

Prometheus scrapes `api` and `worker`; Grafana visualises; both are side infra (not on
the request path).

## 2. Component responsibilities

### Nginx
- Single entry point; distributes requests across API instances (round-robin).
- Terminates TLS in production (local compose uses HTTP).
- Runs health checks against `/ready`; fails traffic away from unhealthy instances.
- Dealers' note: it is a reverse proxy — it does **not** cache responses here because
  the redirect answer is tiny and the cache already exists behind the API.

### API instances (x3)
- **Stateless by construction**: no session state, no local cache, no local
  persistence. All state lives in Redis (transient) and PostgreSQL (durable).
- Handlers parse/validate HTTP; services enforce business rules; repositories talk to
  PostgreSQL. No SQL or Redis calls inside handlers (testability, and clean layering).
- Provide `/health`, `/ready`, `/metrics`, and the public API.

### Redis (three roles — three logical DBs, one process)
| Role | Logical DB | Keys |
|---|---|---|
| Read cache (cache-aside) | 0 | `url:{code}` → JSON mapping, TTL'd |
| Rate limiting | 1 | sliding window per client |
| Analytics stream | 2 | `events` stream + consumer groups |

> Rule enforced by design: **Redis never stores the only copy of a URL mapping.**
> A flush/restart of Redis loses cache (rebuilt from PG), never data.

### PostgreSQL
- System of record: `urls`, `analytics` (aggregate) tables, plus idempotency records.
- Handles every write once (create/delete) and every cache miss.

### Worker
- Runs separate from the API. Consumes analytics events from the Redis stream,
  aggregates them, and writes summary rows to PostgreSQL.
- Absorbs the analytics write load so redirects never wait on it.
- Handles retries, backpressure, graceful shutdown. (Design at Stage 15–16.)

### Prometheus / Grafana
- Metrics endpoint on API + worker; dashboards for latency, cache ratio, DB load,
  worker throughput, error rates.

## 3. Request flows

### 3.1 Create a short URL (`POST /api/v1/urls`)
```
Client → Nginx → api
  handler: validate body (size, URL validity, own-domain guard)
  service: rate limit (Redis) → idempotency check (Stage 14.5)
  service: generate code (custom or Base62, Stage 9)
  repository: INSERT urls (SQL, unique constraint guards races)
  409 on duplicate custom code
  cache: SET url:{code} (warm the read path)
← 201 Created { short_url, ... }
```
Warm-path note: the created code is immediately cached so the first redirect is a hit.

### 3.2 Redirect (`GET /{code}`)
```
Client → Nginx → api
  handler → service
    cache lookup (Redis):   HIT → shorten latency, serve
                            MISS → repository lookup (PG)
                                   → cache SET (TTL), serve
  record analytics event → Redis stream (non-blocking, in-process queue)
← 301 Location: original URL   |   404 unknown   |   410 expired
```
Redis down (`GET url:{code}` errors): skip cache, query PG directly, serve.
This is fail-open for availability, measured at Stage 22.

### 3.3 Analytics pipeline (Stage 15–16)
```
redirect (API) ──XADD events──▶ Redis Stream
                                    │ worker consumes (XREADGROUP)
                                    ▼
                              aggregate in worker
                                    ▼
                              UPSERT analytics (PG)
```
- The API never blocks on analytics persistence (on-the-path event write is a single
  fast `XADD` through an in-process buffer).
- Events are **at-least-once**: worker redelivers on failure; aggregation is
  idempotent by (link, time bucket, dimension).

## 4. Key decisions (summary — records live in DECISIONS.md)

| # | Decision | Summary rationale |
|---|---|---|
| D-01 | PostgreSQL = source of truth; Redis = cache only | Loses nothing on Redis failure; reference project's core flaw |
| D-02 | Cache-aside with short TTL | Read-hot path; TTL is the invalidation backstop; decision on value at Stage 13 |
| D-03 | Code = auto-increment PK → Base62 | Collision-free, short, DB-anchored; full comparison at Stage 9 |
| D-04 | Redis Streams + worker for analytics | Reuses existing Redis; no PG writes on hot path; no Kafka at this scale |
| D-05 | Stateless API behind Nginx (round-robin) | Any instance serves any request; scale = add containers |
| D-06 | Rate limiting: Redis sliding window, fail-open on Redis outage | Precise per-IP accounting; availability wins over strict limits (measured) |
| D-07 | 301 permanent redirect | Expansion links are immutable identifiers; browser-friendly |
| D-08 | Layered Go (handler→service→repository) | Testability and interview transparency; no premature abstraction (e.g. no interface per file) |

## 5. Data placement map

| Data | Source of truth | Redundant copies |
|---|---|---|
| URL mapping | `urls` table (PG) | Redis `url:{code}` (TTL, disposable) |
| Expiration | `urls.expires_at` (PG) | cached value (short TTL bounds staleness) |
| Rate-limit windows | none needed | Redis only (transient by nature) |
| Raw analytics events | worker → `analytics_events` | Redis stream (transient buffer) |
| Aggregated stats | `analytics` table (PG) | none (read from PG, low QPS) |

## 6. Failure model (summary)

Full test matrix + measured impact in `docs/FAILURES.md` (Stage 22). Summary of
designed behaviour:

| Failure | Designed behaviour | Fails open/closed |
|---|---|---|
| Redis down | Redirect falls back to PG; analytics + rate limiting best-effort (logged) | open (availability) |
| PG down | Redirects serve from cache while TTL lasts; writes 503; reads hit cache only | open for reads, closed for writes |
| API instance down | Nginx health check removes it from rotation; zero data loss (stateless) | open |
| Worker down | Stream buffers events; PG aggregate goes stale (never lost while stream persists) | open (degraded freshness) |
| Slow PG | Cache absorbs reads; writes queue; timeouts + circuit documented | — |

## 7. Scalability path (documented, not built)

- **More reads** → add API instances; keep Redis; raise cache TTL.
- **More writes** → consider async creation queue; then PG read replicas (reads only).
- **PG saturation** → vertical first; then partition/archive analytics by time bucket
  (the analytics table is the first thing to grow).
- **Regional distribution** → this is when code generation moves off a single PG
  sequence (see D-03 record); base62 already tolerates it.
- **Kafka trigger**: replaces the Redis stream only when (a) >1 consumer type needs
  the same events and replay/ordering semantics, or (b) events must survive a Redis
  outage with zero loss. Not now.

## 8. Layer rules (kept in review)

1. Handlers parse HTTP, validate shapes, call **one** service method, map errors. No SQL.
2. Services hold business rules (validation, codegen, cache policy, rate limiting) and
   are testable without HTTP.
3. Repositories are the only place that touches pgx/SQL.
4. The cache is its own adapter behind the service boundary; Redis "DBs" are its
   internal detail.
5. Every request carries a request ID; logs are JSON; every error is wrapped once with
   context.

## 9. What this design deliberately gives up

- Some redundant lookups (a create warms cache; a redirect re-reads it) — negligible.
- Strict rate limiting during a Redis outage (fail-open) — availability first.
- Analytics durability during simultaneous Redis+down worker — accepted, covered by
  FAILURES.md.
- No auth/ownership in v1 — the model is shaped so ownership can be added as a column
  + auth filter in v2 without redesign.