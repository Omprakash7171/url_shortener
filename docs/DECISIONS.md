# Engineering Decision Records

> The single source of truth for why this system is built the way it is.
> If a decision is challenged in an interview, the answer is in this file.
> Each record follows the same format and the "How we tested it" field is only
> ever filled with something actually done.

## D-01 — PostgreSQL is the system of record; Redis is a cache

**Decision:** URL mappings live in PostgreSQL. Redis holds disposable copies (cache),
rate-limit state, and the analytics event stream — never the only copy of a mapping.
**Context:** The reference project
([`ahmadrizal1st/url-shortener`](https://github.com/ahmadrizal1st/url-shortener))
stores mappings *only* in Redis; a Redis flush/restart deletes every link, and there
is no way to run durable analytics queries.
**Options:** (a) Redis as datastore (reference design), (b) PG source of truth + Redis cache.
**Chosen approach:** (b).
**Why:** The read path (redirect) is cache-degradable; the write path (create) must be
durable. PG gives durability, integrity (unique codes), and queryability for stats.
Redis gives single-digit-ms reads for ~any-hot key. Splitting them means each does
what it is best at and either can fail without losing data.
**Advantages:** A `FLUSHALL` is an inconvenience (cold cache), not an outage. Cache
TTL bounds staleness. Analytics live where SQL is.
**Disadvantages:** Two systems to operate; cache-miss path does a PG round-trip.
**What we gave up:** The single-store simplicity of the reference project.
**How we tested it:** Stage 12 measures redirect latency before/after Redis; Stage 22
deliberately kills Redis and verifies redirects still resolve from PG.

## D-02 — Cache-aside with a short TTL

**Decision:** API reads `url:{code}` from Redis (cache-aside / read-through); on miss
it queries PG, then writes the cache with a TTL. Writes (create/delete) invalidate or
refresh the cache synchronously.
**Context:** Redirect is the hot read path. A cache must not be the source of truth,
and failure semantics must be explicit.
**Options:** cache-aside (read + repopulate), write-through, full read-through at
Nginx, no cache.
**Chosen approach:** cache-aside at the service layer (not Nginx), TTL bounded.
**Why:** Service-layer cache shares one Redis across instances, is invalidated with
the business operation (delete → `DEL url:{code}`), and instruments hit/miss trivially.
Nginx caching would add a second, harder-to-invalidate layer.
**Advantages:** Hot codes resolve without touching PG; misses self-heal.
**Disadvantages:** Miss storm on cold start (mitigated in D-02/Stage 13); a race can
serve stale data for ≤ TTL.
**What we gave up:** Strict read-your-writes inside the TTL window on the redirect
path (acceptable: expiry is checked on the authoritative read when it matters).
**How we tested it:** Stage 12: hit ratio + latency before/after. Stage 13: TTL
eviction, invalidation on delete, stampede (concurrent misses for one cold key).

## D-03 — Short codes: auto-increment PK encoded as Base62

**Decision:** `code = base62(urls.id)`, where `id` is a `BIGINT GENERATED ALWAYS AS
IDENTITY` primary key and the alphabet is `0-9A-Za-z`. User custom codes share the
same `code` column, constrained by one unique index.
**Context:** The brief demands an explicit comparison of strategies.
**Options evaluated:**

| Criterion | Auto-inc + Base62 | Random (e.g. 7×base62) | UUID | Hash of URL |
|---|---|---|---|---|
| Collision | impossible (DB-controlled) | probabilistic (retry) | ~impossible | guaranteed (hashes collide) |
| URL length | shortest (grows with id) | fixed (7–8) | 16+ | 8–11 |
| Predictability | sequential → enumerable | unpredictable | unpredictable | deterministic |
| Distributed gen | needs DB (or sharded seq) | any node | any node | any node |
| DB dependency | create ↔ DB (already a write) | gen-time none | none | none |
| Scalability | one PG sequence per region | excellent | excellent | excellent |

**Chosen approach:** auto-inc + Base62.
**Why:** Collision-free by construction (no retry loop, no probabilistic risk), keeps
codes shortest (id 1 → `1`, id 3.5e9 → 6 chars), and the "DB dependency" is free —
creating a link is a DB write anyway. For a *redirect* service, lookups are by code
(thr wire) via the unique index; nothing about the design depends on the id being
reversible.
**Advantages:** encoded, deterministic, zero collision handling, short codes.
**Disadvantages:** codes are sequential and enumerable — an attacker can iterate the
code space. Multiple regions need their own sequences.
**What we gave up:** Unpredictability (enumerability). Portable mitigation is a
format-preserving Feistel permutation over the id (random-looking, still
deterministic, collision-free) — documented as a future improvement, not built,
because no v1 requirement needs secrecy and the mitigation adds crypto surface.
**How we tested it:** Unit tests for the encoder round-trip and alphabet; Stage 10
duplicate-code conflict test at the repository/API level; Stage 22 "duplicate custom
code" failure test (409, no row written).

## D-04 — Analytics: never on the redirect path

**Decision:** A redirect enqueues one event into a Redis stream (through an in-process
buffer) and **does not block on analytics persistence**. A worker consumes the stream,
aggregates, and upserts `analytics_daily` counters into PostgreSQL.
**Context:** The hottest path must not absorb analytics write latency/lock contention
(measured to be the difference between "a redirect is a cache hit" and "a redirect is
a DS-involved write").
**Options:** (a) synchronous analytics write in API, (b) PG outbox written in API,
(c) Redis stream + worker (chosen), (d) Kafka + worker.
**Chosen approach:** (c).
**Why:** The API/XADD hop is sub-millisecond and uses infra already present. Kafka is
rejected for v1: no second consumer type exists, replay/ordering semantics are not
required, and an extra broker adds ops + a new failure domain. Kafka becomes
appropriate when (a) multiple consumer types need the same events or (b) analytics
must survive a full Redis outage with zero loss.
**Advantages:** redirect stays a pure cache read; worker batches writes to PG.
**Disadvantages:** analytics are eventually consistent; events can be lost if Redis is
down (degraded mode documented and tested in FAILURES.md).
**What we gave up:** strict, synchronous accuracy of analytics.
**How we tested it:** Stage 15 proves the redirect handler is unaffected by worker
unavailability; Stage 16 tests worker retries/dedup/graceful shutdown; Stage 22 tests
worker-down and Redis-down event handling.

## D-05 — Two-step create with nullable `code`

**Decision:** Create runs `INSERT ... RETURNING id` on a row whose `code` is NULL,
computes `base62(id)` in the API, then `UPDATE urls SET code = $1 WHERE id = $2` in
the same transaction. Custom codes are inserted directly in the first statement.
**Context:** `code` derives from `id`, which only exists after the insert.
**Options:** (a) trigger/function to compute code in SQL, (b) generated column, (c)
insert-then-update in one TX (chosen).
**Chosen approach:** (c).
**Why:** Keeps the Base62 logic in Go (unit-testable, single implementation), avoids
custom SQL functions, and the two-statement cost is off the hot path. A generated
column requires a SQL base62 function; a trigger hides implicit writes.
**Advantages:** clean, testable, no DB function maintenance, correct under
concurrency (unique index guards the update window).
**Disadvantages:** two statements per create (~+0.1 ms measured, ok).
**What we gave up:** single-statement inserts; SQL-side generation.
**How we tested it:** repository integration test: created row has both id and a
non-null, unique, correct code; concurrent custom-code conflict raises 409.

## D-06 — Generic dimensioned analytics table (`analytics_daily`)

**Decision:** One table, PK `(url_id, day, dim, value)`, `count` accumulated by
`INSERT ... ON CONFLICT DO UPDATE`. Dimensions: `total` (daily series + lifetime),
`referrer`, `device`, `country`.
**Context:** v1 needs: total clicks, per-day clicks, top referrers (device/country are
"should"s). A table per dimension would multiply migrations forever.
**Chosen approach:** generic (url_id, day, dim, value) counters.
**Why:** Adding a dimension is inserting rows, not ALTERing schema. All four v1 stats
queries are `WHERE url_id=$1 [AND dim=$2]` and are served by the PK prefix — measured
0.09–0.17 ms even after aggregation (docs/03-database.md §4).
**Advantages:** schema stability, single code path for the worker upsert, cheap stats.
**Disadvantages:** rows carry `dim/value` redundancy (e.g. `('total','total')`);
ranked referrers need a small per-link sort.
**What we gave up:** a fully normalised star schema.
**How we tested it:** seeded and EXPLAIN-measured (scripts/seed_explain.sql);
Stage 10 stats-service tests assert correct aggregation order.

## D-07 — Rate limiting: Redis, sliding window, fail-open

**Decision:** A sliding-window limiter in Redis middleware (logical DB 1), per-client
(`X-Forwarded-For` behind Nginx / client IP), returning `429` with `RateLimit-*`
headers. When Redis is unreachable the limiter **fails open** (allows) and records a
metric + log line.
**Context:** Chatty/abusive clients must not flood the write path. Distributed
instances need shared state → the limiter must live in Redis, not per-instance memory.
**Options:** fixed window (bursty at edges), sliding window (chosen), token bucket.
**Chosen approach:** sliding window.
**Why:** Fixed window allows 2× bursts at boundary; token bucket adds refill
bookkeeping. Sliding window (counter per `key:window_start` plus weighting of the
previous window) is simple, exact-ish, and a good interview artifact.
Fail-open: an availability-first service should degrade to "unlimited" rather than
"all 503" when the limiter's store vanishes — with a loud metric.
**Advantages:** shared across instances, precise, one Redis dependency style.
**Disadvantages:** fail-open admits abuse during a Redis outage.
**What we gave up:** strict enforcement during Redis outages; token-bucket's smooth
burst handling.
**How we tested it:** Stage 14 burst tests (limit-1, limit-2 exceed windows), 429 body
+ headers, concurrent requests from one key; Stage 22 kills Redis and verifies the
API still serves (fail-open) with the rejection metric flat.

## D-08 — 301 permanent redirect

**Decision:** `GET /{code}` returns `301 Moved Permanently` with the expanded URL.
**Context:** Short codes are immutable identifiers for the life of the link.
**Options:** 301 vs 302 vs 307.
**Chosen approach:** 301.
**Why:** Browser/SEO-friendly: clients cache the mapping, reducing repeat hits. The
analytics event still counts each *enqueued click*; caching by browsers only affects
how many of those browsers re-contact us (acceptable for v1).
**Advantages:** fewer repeat requests, better Share/SEO semantics.
**Disadvantages:** a deleted/re-pointed link is cached by clients that visited before
(we mitigate: delete invalidates Redis; clients holding a 301 cache eventually
expire).
**What we gave up:** instant propagation of post-publication edits to repeat visitors.
**How we tested it:** API integration test asserts 301 + Location header.

## D-09 — Layered Go: handler → service → repository

**Decision:** `internal/handler` (HTTP only) → `internal/service` (rules) →
`internal/repository` (pgx/SQL only). Cache is an adapter behind the service boundary.
**Context:** Testability and interview transparency; the reference project folded
rules into handlers.
**Options:** flat handlers (reference), layered (chosen), hexagon/ports.
**Chosen approach:** layered but **without** premature interfaces — repository and
cache are concrete structs with methods, injected for tests via interfaces only where
a fake is genuinely useful.
**Why:** Handlers stay thin; business logic tests need no HTTP; repository tests need
no HTTP. Interfaces are added when a test demands one, not by convention.
**Advantages:** test pyramid is clean; SQL is quarantined.
**Disadvantages:** more files to navigate.
**What we gave up:** the simplicity of the reference's flat handlers.
**How we tested it:** Stage 10 — unit (service motives), repository (real PG), API
(httptest).

## D-10 — Embedded migrator, not a migration tool binary

**Decision:** Migration SQL is `//go:embed`-ed into the API binary; a
`cmd/migrate` runs them against `DATABASE_URL` in order, each in a transaction under
`pg_advisory_lock` (so N API instances booting together cannot double-apply), tracked
in a `schema_migrations` table.
**Context:** The system runs in Docker; a running API should never start against an
unmigrated schema, and CI must apply migrations once.
**Options:** golang-migrate/atlas as a separate step (tool added), embedded runner
(chosen).
**Chosen approach:** embedded runner (~80 lines, zero new dependencies).
**Why:** Removes an external binary from the image/compose, teaches the advisory-lock
pattern, and the whole migration story fits in one reviewable file. If down-version
management becomes painful, this is an easy, documented swap to golang-migrate.
**Advantages:** no new dep; migrations travel with the binary; safe under concurrent
startup.
**Disadvantages:** custom code to maintain (tiny, tested).
**What we gave up:** third-party migration tooling for v1.
**How we tested it:** Stage 5 — two API instances start simultaneously against a
blank DB; schema applied exactly once (advisory lock).

## D-11 — Index discipline: only indexes that serve a v1 query

**Decision:** Every index in `docs/03-database.md` is paired with the query it serves
and a measured cost. `long_url`, `expires_at`, `created_at`, and `updated_at` are
deliberately unindexed/absent.
**Context:** "Add common indexes" is how schemas rot. Every index costs writes.
**Chosen approach:** measure, then decide. E.g. a single `long_url` lookup is
**27.98 ms** unindexed on 1M rows — but v1 has no such query, so no index;
if v2 adds lookup-by-URL the preferred fix is a `long_url_hash` column (index on a
hash), not a huge text index.
**Advantages:** wrote cost stays minimal; schema stays small.
**Disadvantages:** adding a lookup feature later is a migration.
**What we gave up:** v1 support for "find by original URL".
**How we tested it:** EXPLAIN ANALYZE for every present/absent index in
`scripts/seed_explain.sql`.

## D-12 — No expiry sweep; expiry enforced at read

**Decision:** `expires_at IS NOT NULL AND expires_at < now()` ⇒ `410 Gone`. No worker
or cron deletes expired rows.
**Context:** Expired links are "gone" to readers; leaving the rows costs a small,
index-free table.
**Options:** read-time check (chosen), background sweep.
**Chosen approach:** read-time.
**Why:** Removes a whole class of job (scheduling, locking, tombstone handling) for
zero user-visible benefit; the cache TTL naturally clears the hot expired rows.
**Advantages:** no expiry infra; correctness is in one place (resolution service).
**Disadvantages:** table grows with dead rows (cheap; revisit at scale).
**What we gave up:** storage recycling of expired links.
**How we tested it:** Stage 10 tests assert unexpired→redirect, expired→410.

## D-13 — `ON DELETE CASCADE` for analytics

**Decision:** Deleting a `urls` row cascades to `analytics_daily`.
**Context:** A deleted link's stats are unreachable via the API anyway.
**Options:** CASCADE (chosen), RESTRICT+manual cleanup, soft-delete.
**Chosen approach:** CASCADE.
**Why:** No orphaned counters, no cleanup job. Soft-delete is only worth it if we
want to reuse links or keep history — neither is a v1 requirement.
**Advantages:** referential integrity for free, delete is one statement.
**Disadvantages:** history is gone with the link.
**What we gave up:** click-history retention after deletion.
**How we tested it:** repository test: delete link → stats gone (FK + cascade).

## D-14 — Net/http + chi; pgx v5

**Decision:** Routing on Go `net/http` with `github.com/go-chi/chi/v5`; PostgreSQL
access via `pgx/v5` + `pgxpool` with hand-written SQL. Gin (reference) is dropped.
**Context:** The reference project used Gin. The user explicitly chose chi + pgx.
**Options:** Gin vs net/http+chi vs stdlib-only; pgx vs GORM vs sqlc.
**Chosen approach:** net/http+chi; pgx manual SQL.
**Why:** Standard handler signature, zero magic, middleware that is plain Go; pgx's
native interface is the fastest and most interview-credible PG driver; hand-written
SQL keeps the repository layer transparent and EXPLAIN-friendly.
**Advantages:** transparency, performance, fewer surprises.
**Disadvantages:** more boilerplate than Gin/GORM.
**What we gave up:** framework conveniences and ORM write-speed.
**How we tested it:** the whole project uses this; no foreign-key to another stack.

## D-15 — v2-only: auth, ownership, API keys

**Decision:** No accounts, API keys, or ownership in v1. Rate limiting is per IP.
**Context:** Auth shapes every table and middleware; adding it before the core is
proven would delay measurement and add surface.
**Options:** build auth now (deferred), keep interfaces HTTP-header-friendly.
**Chosen approach:** defer; `urls` has no `owner_id` (migration-free addition later).
**Why:** The brief explicitly permits deferral; the schema was designed so ownership
is additive (one column + one filtered index + a middleware), documented in
`docs/03-database.md` §2.
**Advantages:** v1 stays focused and measurable.
**Disadvantages:** "who owns this link?" is unanswered in v1.
**What we gave up:** tenant isolation for v1.
**How we tested it:** the deferral itself is the test of scope discipline; v2
migration path is written in `docs/03-database.md`.

## D-16 — Failure posture: fail-open for availability

**Decision:** When a non-authoritative dependency is unavailable, the API continues:
Redis down → cache skipped, rate limiting skipped (logged+metric), analytics events
dropped (logged); PG down → cache serves reads, writes return 503.
**Context:** The brief demands an explicit, tested resilience posture, not just
"we are resilient".
**Options:** fail-open (chosen for reads), fail-closed for writes.
**Chosen approach:** availability wins on reads; writes must still hit the source of
truth, so they fail closed (503) rather than silently accepting data that would be
lost.
**Advantages:** a Redis blip does not take down redirects.
**Disadvantages:** abuse is unthrottled during a Redis outage.
**What we gave up:** strict enforcement during degraded operation.
**How we tested it:** every case in `docs/FAILURES.md` (Stage 22) has a reproduction
and an outcome — Redis/PG/worker/unhealthy-instance scenarios.

## D-17 — Configuration & logging: 12-factor, zero dependencies

**Decision:** All configuration is read from environment variables once in
`internal/config`; the app never reads dotfiles. Logging uses the standard library's
`log/slog` with a JSON handler — no logging dependency.
**Context:** The reference project baked `.env` into its Docker image (`COPY . .`)
and used ad-hoc log prints. A portfolio service must be 12-factor and observable.
**Options:** godotenv + a logging framework (zap/logrus); env-only + slog (chosen).
**Chosen approach:** env-only + slog.
**Why:** Environment injection is the container-native contract (compose/CI set
env); reading dotfiles inside the app invites secret-inclusion mistakes. slog is
structured-by-default and maintained by the Go team — one less dependency to audit.
A missing mandatory variable (e.g. `DATABASE_URL`) fails fast at startup with a
clear message.
**Advantages:** no dotfile, no secret-baking, structured JSON logs out of the box.
**Disadvantages:** local `go run` requires exported env vars (documented dev flow).
**What we gave up:** auto-loaded local `.env` files and framework logging features.
**How we tested it:** `/health`, `/ready` and request-id log lines verified as JSON
with `request_id` propagated; missing `DATABASE_URL` fails fast at startup.

## D-18 — Input validation with the standard library, not a validation dependency

**Decision:** URL validation uses `net/url` (parse, enforce http/https scheme, non-empty
host, ≤ 2048 chars, own-domain guard); custom codes use a regex; expiry uses a regex and
bounded duration math. Request bodies are capped at 1 MiB (`http.MaxBytesReader`).
The reference project's `govalidator` dependency is not carried over.
**Context:** URL validation is genuinely hard (IDNs, ports, ambiguous hosts), but the
v1 acceptance criteria are deliberately modest: scheme, host, length, self-reference.
**Options:** `govalidator`/`go-playground/validator` (adds a dependency), stdlib `net/url` (chosen).
**Chosen approach:** stdlib.
**Why:** One less dependency to audit, and the stdlib rules are explicit and testable —
an interviewer can read exactly what is accepted. When stricter validation is needed
(SSRF protection, private-IP blocking), that is a documented add-on, not a default.
**Advantages:** zero dependency, transparent rules, no magic tags.
**Disadvantages:** we accept some URLs a validator would reject and vice versa
(acceptable, documented bounds).
**What we gave up:** validator-ecosystem feature richness for v1.
**How we tested it:** service unit tests (invalid schemes, empty host, over-length,
self-reference) and API integration tests asserting the `url_invalid` /
`self_reference` / `url_too_long` error codes.
## D-19 - Containerisation as a formal deliverable

**Decision:** a multi-stage Dockerfile (golang:1.27-alpine build, alpine:3.21 runtime),
non-root `app` user, static binary (`CGO_ENABLED=0`, `-trimpath`, `-s -w`), and a
docker-compose stack with real healthchecks (`pg_isready`, HTTP `/ready` via busybox
wget). The compose file uses plain defaults so it runs without a `.env`.
**Context:** the roadmap treats Docker as an integration milestone (stage 11), not an
afterthought; the container must be auditable and demotable.
**Options:** single-stage docker build (fast but fat), host-only dev (no delivery),
multi-stage (chosen).
**Chosen approach:** multi-stage, non-root, healthchecked compose.
**Why:** smallest runtime surface, least privilege, and `/ready` gives compose and the
later Nginx/load-balancer layer a real liveness signal.
**Advantages:** verified `uid=100(app)`, restart policy, e2e through the container.
**Disadvantages:** two copies of toolchain layers on the builder.
**What we gave up:** a scratch image (alpine gives us busybox `wget` for healthchecks).
**How we tested it:** `docker compose up -d --build`; both services report
`healthy`; create/redirect e2e over 8083; container runs as non-root. Re-verified
when Redis was added in stage 12.

## D-20 - Cache-aside URL lookup with a 60 s TTL (read hot path)

**Decision:** redirect resolution is cache-aside: `GET /{code}` reads `us:url:{code}`
from Redis and, on miss, reads PostgreSQL and (only on success) writes the cache with
TTL=60s (`min(60s, remaining expiry)`). Deletes invalidate the key. Cache entries that
are past `expires_at` are evicted and treated as a miss so expiries stay correct.
**Context:** redirects dominate traffic; the DB lookup is already ~0.04ms but the cache
removes DB round-trips entirely and smooths DB load spikes. The short TTL is deliberate:
invalidation is best-effort and the cost of a one-minute-stale window is benign (a
deleted/expired link may still redirect for up to 60s).
**Options:** no cache (pure PG), long TTL (hours), write-through, cache-aside short TTL (chosen).
**Chosen approach:** cache-aside, 60s, best-effort invalidation.
**Why:** simplest correct shape; matches the "PG is truth" rule; keeps the failure modes
bounded (short stale window).
**Advantages:** zero cache-write latency on the request path (write is async to the
response only), simple correctness story.
**Disadvantages:** stale window ≤ 60s after delete/expiry.
**What we gave up:** strict consistency on deletes (acceptable, documented).
**How we tested it:** integration test proves first redirect = cache miss, second =
cache hit (counters), and DELETE removes the key; live Redis shows `us:url:*` with
TTL and the `url_cache_{hits,misses}_total` counters move.

## D-21 - Fail-open with fast Redis deadlines everywhere

**Decision:** every Redis operation (cache get/set/del, rate-limit INCR) runs under a
`context.WithTimeout(200ms)` in addition to the client's 250ms write timeout and 500ms
dial/pool timeouts; retrieval failures fail open to PostgreSQL. A dead/DNS-stuck peer
must never stall a request.
**Context:** measured worst case with Redis stopped was ~10s+ per request because
Docker DNS on a stopped peer times out slowly and go-redis retried; that violated the
"reads never depend on Redis" rule.
**Options:** long retries (slow fail-open), fail-return (wrong: reads would error),
fail-open with short deadlines (chosen).
**Chosen approach:** 200ms per-op cap + `MaxRetries=1`.
**Why:** fail-open is only useful if it is fast; the cap makes the failure latency
bounded and visible in metrics (`url_cache_fail_open_total`).
**Advantages:** measured fail-open latency 434ms (most of it the single DNS attempt).
**Disadvantages:** a genuinely slow-but-live Redis becomes a miss rather than a hit.
**What we gave up:** nominal retry resilience on Redis (it is a cache, not a truth source).
**How we tested it:** stopped the Redis container and drove `/1` through the compose
stack: 301 returned, warn logged `url cache read failed, failing open`.

## D-22 - Fixed-window per-IP rate limiting on the write endpoint

**Decision:** `POST /api/v1/urls` is limited with a Redis fixed-window counter per
client IP (`us:rl:ip:{ip}:{minute}`), default 60/min, configurable via
`RATE_LIMIT_PER_MIN`; exceeded requests get `429` + `Retry-After`. Reads are not rate
limited. The limiter fails open (and counts that) if Redis is down.
**Context:** the create endpoint allocates a sequence ID and writes to PG; it is the
abuse surface. A per-IP window is trivial to explain in an interview and to reason about.
**Options:** token bucket on the API side (more moving parts), Nginx `limit_req`
(later, for the LB layer), fixed window in Redis (chosen) for now.
**Chosen approach:** fixed window, per IP, on the create route.
**Why:** smallest correct primitive; the coarse boundary (60/min) is acceptable for v1;
can be composed with Nginx `limit_req` at the edge later for defence in depth.
**Advantages:** one INCR+EXPIRE; cheap; 429 test proves the envelope and header.
**Disadvantages:** burstiness within the window is allowed.
**What we gave up:** precise token-bucket smoothing for v1.
**How we tested it:** integration test with limit=3: three creates 201, fourth 429 with
`Retry-After`, redirect read path still 301.

## D-23 - Nginx with static upstream replicas + passive health checks

**Decision:** a single Nginx entry point (host `8090:80`) reverse-proxies three API
replicas declared as *static* upstream `server url-shortener-api-{1,2,3}:8080` entries
with `max_fails=2 fail_timeout=10s`; `proxy_next_upstream` retries
error/timeout/502/503/504/429; API containers publish **no** host ports; upstream
keepalive is disabled for the demo so sequential clients round-robin.
**Context:** the first correct-looking design used `server api:8080 resolve;` +
`resolver 127.0.0.11` for dynamic DNS-based balancing (Nginx open-source supports
runtime DNS only via `resolve` with a shared-memory `zone`). That worked once, but
Compose's docker DNS returned one A-record first and the keepalive pool pinned every
sequential client to a single replica — measured 12/12 on one instance. Static names
rotate deterministically (measured 4/4/4).
**Options:** (a) static upstream list (chosen), (b) dynamic `resolve` upstream,
(c) Docker Swarm VIP/Consul-style service discovery.
**Chosen approach:** (a) for Compose; the hardcoded names are acceptable because
replica names are deterministic per project (documented limitation vs. auto-discovery).
**Why:** classic weighted round-robin + passive health checks is the simplest LB that is
*provable* in a demo, and compose-level scaling is the deployment envelope.
**Advantages:** gateway failures (`proxy_next_upstream`) are retried on the next peer;
a stopped replica is skipped after max_fails marks it down; single public port.
**Disadvantages:** adding replicas means editing the upstream block (no auto-discovery);
Nginx startup races the creation of new replica DNS names (needs a restart if it boots
before they register).
**What we gave up:** auto-discovery and dynamic scaling for v1.
**How we tested it:** `--scale api=3`; 12 redirects through `localhost:8090` split
4/4/4 across `X-Instance` headers; `docker stop url-shortener-api-1` → 10/10 requests
still 301, traffic served by the two survivors, dead peer dropped from rotation.

## D-24 - Prometheus/Grafana observability with per-process endpoints

**Decision:** each process exposes its own Prometheus exposition endpoint — the API on
`/metrics` (router option `WithMetrics()`), the worker on `:8081/metrics` — per-instance
(not through the LB, so the LB cannot become an observability SPOF). Prometheus scrapes
`api-{1,2,3}:8080` and `worker-1:8081` every 5s; Grafana (host `3001`) is provisioned
automatically with a Prometheus datasource and a "URL Shortener Observatory" dashboard.
**Context:** the metrics package (stages 12-16) already registers counters for cache
hit/miss/fail-open, events published/dropped/failed, analytics processed/deduped, and a
request-duration histogram. Stage 20 needed to expose them and visualise them.
**Options:** (a) scrape via Nginx `/metrics` (single endpoint, but the LB is a SPOF and
instance identity is lost), (b) scrape each instance directly (chosen), (c) Pushgateway.
**Chosen approach:** (b), direct per-instance scraping.
**Why:** per-instance scraping keeps instance labels (`X-Instance` matches), survives LB
failures, and makes per-replica latency/goroutine data visible for load tests.
**Advantages:** zero extra agents; the same counters power the CI load-test report and
the live dashboard; `up{job}` gives an instant health check.
**Disadvantages:** new replicas need their IP/name added to `prometheus.yml` (same static
limitation as D-23; the compose replica names are deterministic).
**What we gave up:** human-readable nginx stub-status scraping (not Prometheus-format —
removed from `scrape_configs`; `deploy/nginx.conf` still exposes stub_status locally).
**How we tested it:** `curl :9090/api/v1/targets` → all 4 targets `health:"up"`;
`sum(url_cache_hits_total)` returned real counts; `:3001` served the provisioned
dashboard (uid `url-shortener-obs`); worker `/metrics` shows `url_analytics_processed_total`
rising as events are aggregated.

## D-25 - Histograms are only useful if something actually observes them

**Decision:** every metric must have at least one production caller. The request-duration
histogram was registered in stage 12 but had no observer until stage 21, so Grafana's
latency panels stayed empty while k6 hammered the API.
**Context:** the CM was easy to audit — `metrics.HTTPRequests` appeared only in
`metrics.go`. The fix is a router-level `middleware.Metrics` that calls
`http.Handler`, times the request, and `Observe`s it. That one middleware now feeds the
latency dashboards and the L-03 percentiles.
**Why it matters:** an unobserved metric is configuration drift; it silently breaks any
dashboards/alerts built on it. The dashboards did not catch it because Prometheus still
scraped the (empty) histogram successfully.
**How we tested it:** `go build ./...`, recreate the images, re-run k6, then
`histogram_quantile(0.99, sum(rate(url_http_request_duration_seconds_bucket[10m])) by (le))`
returned 4.95ms instead of `NaN`.

## D-26 - Idempotent creates via a Redis claim/replay store

**Decision:** `POST /api/v1/urls` honours an `Idempotency-Key` header using a
Redis-backed store (`us:idem:resp|lock:{key}`). The first request `SET NX`-claims the
key, creates, and records the resulting short code (24 h TTL); later requests replay
the **exact original** response as `200` with `Idempotency-Replay: true`. Concurrent
same-key requests poll the recorded result for ≤ 1 s then return `409
idempotency_in_progress`. Redis-down is fail-open (create without dedupe + metric).
**Context:** `IdempotencyKey` was already threaded from the handler into
`CreateParams` (the API contract in `docs/API.md` promised it) but `Create` never
used it — a retry with the same key minted a fresh short code every time (reproduced
live: key `test-idempotency-003` produced codes 70 then 71).
**Options:** (a) PG `idempotency_records` table PK'd on key (durable, but a second
write-path dependency and the replay JSON must be stored/versioned), (b) Redis
claim + recorded code, replay from the PG row (chosen), (c) in-memory (not shared
across replicas — useless behind the LB).
**Chosen approach:** (b).
**Why:** the code is the only replay state (the PG `urls` row is the source of truth
for short→long, so replays stay correct after custom-code/edit changes); Redis is
already in the write path for rate limiting; the `SET NX` claim gives a primitive-lock
semantics that is trivially correct across 3 replicas.
**Advantages:** cross-replica dedupe proven (requests 1 and 2 of the reproduction hit
different `X-Instance`s and still yielded one row); no schema change; auto-expiring
lock can't deadlock after a crashed creator.
**Disadvantages:** an unavailable Redis degrades to create-without-dedupe (documented,
mirrors D-21); replay of a subsequently-deleted code surfaces `404` (acceptable).
**What we gave up:** durable idempotency records that survive a Redis flush — a
`FLUSHALL` forgets remembered keys (callers should treat idempotency as a
best-effort/retry-safety mechanism, not an audit log).
**How we tested it:** store unit tests (miss/round-trip/claim-exclusivity); handler
integration tests `TestCreateIdempotencyReplay` (201 then 200 same code, 1 row) and
`TestCreateIdempotencyConcurrent` (5 parallel same-key creates → 1 row, identical
codes); live repro through the LB: request 1 → `201` code 72, request 2 → `200` +
replay of 72, a new key → `201` code 73.
