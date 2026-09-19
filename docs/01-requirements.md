# Requirements — Distributed URL Shortening & Analytics Service

> Status: **v1 draft — pending review**. Every requirement is written to be
> testable. Nothing here is "assumed working"; stages 10, 21 and 22 exist to prove it.

## 1. Purpose

A learning and portfolio project for a backend engineer role. The system must
demonstrate production-grade practices: layered Go application, PostgreSQL as the
system of record, Redis as a cache (not a database), asynchronous analytics,
horizontal scaling behind a load balancer, observable metrics, measured performance,
and deliberate failure testing.

The secondary product requirement: **never claim anything that has not been tested**.
Performance numbers, resilience claims, and architecture trade-offs must each be
backed by measurement or a test in this repository.

## 2. Problem statement

A long URL is hard to share, truncates in SMS/print, and can be tracked only by the
destination owner. The service provides:

- A short, shareable code that redirects to the original URL.
- An API to create, read, delete, and inspect the statistics of short links.
- Ability to signal the volume an URL is receiving without giving the creator direct
  access to the infrastructure.

Building it "properly" is harder than it looks: the redirect is a hot, read-only
path that must stay fast and available even when caches fail, while link creation is
a cold, write-heavy path that must stay correct under concurrency.

## 3. Reference project & attribution

The original open-source reference is
[`ahmadrizal1st/url-shortener`](https://github.com/ahmadrizal1st/url-shortener)
(Gin + Redis). We use it as an architectural reference, **not** as a code base.

| Reference idea | What we learned | How we differ |
|---|---|---|
| Redis fast-path for redirects | Redis is ideal for the read-hot path | Redis is a **cache**; PostgreSQL is the system of record |
| Blocking your own domain from being shortened | Prevents recursive shortening of the shortener itself | Kept (as `URLValidator`) |
| `govalidator.IsURL` for validation | URL validation is non-trivial | Kept, extended with scheme normalisation and length limits |
| Hand-rolled per-IP quota | Ad-hoc limiters are buggy (non-atomic, unmeasured) | Redis sliding-window limiter as middleware, tested |

The reference project stores URL mappings *in* Redis (including via GET/PUT/DELETE).
That design loses the mapping on Redis failure and cannot run analytics or durable
queries. We invert it: Redis loses nothing because it only ever holds a copy.

## 4. Glossary

- **Short code** — the opaque identifier a redirect URL is keyed on (e.g. `aB3x9Q`).
- **Redirect** — the act of resolving a short code to its long URL (HTTP 301/302).
- **Cache hit / miss** — whether the code resolved from Redis or had to hit PostgreSQL.
- **Idempotency key** — a caller-supplied token making a POST retry-safe.
- **Analytics event** — one observation of a redirect (time, link, referrer, UA, device).
- **Worker** — a separate process that consumes analytics events off the hot path.
- **API key** — a caller credential (introduced for rate-limit tiers; v1 scope below).

## 5. Functional requirements

Priority: **M** = must (release gate), **S** = should (post-core), **C** = could.

### FR1 — Create a short URL
- **FR1 (M)** `POST /api/v1/urls` accepts `{ "url": "...", "customCode"?: "...", "expiresIn"?: "..." }`.
- **FR1.1 (M)** Reject invalid URLs (400): no scheme after normalisation, malformed host, over length limit.
- **FR1.2 (M)** Reject URLs that point at this service's own domain (prevents infinite redirect loops).
- **FR1.3 (M)** If no custom code, generate one (codegen decision — Stage 9).
- **FR1.4 (M)** Reject a taken custom code with `409 Conflict`.
- **FR1.5 (M)** Persist to PostgreSQL; a successful response means durable.
- **FR1.6 (S)** Support expiration (`expiresIn`); expired links resolve to `410 Gone`.
- **FR1.7 (S)** Honour `Idempotency-Key` headers (identical create retried once → same resource, not a duplicate).

### FR2 — Redirect
- **FR2 (M)** `GET /{shortCode}` resolves and redirects (301 permanent for expansion links).
- **FR2.1 (M)** Unknown code → `404`, expired → `410`.
- **FR2.2 (M)** Resolution must not require a PostgreSQL round-trip on the fast path (cache-aside).
- **FR2.3 (M)** Redirect must succeed even when Redis is down (PostgreSQL fallback).

### FR3 — Read metadata
- **FR3 (M)** `GET /api/v1/urls/{shortCode}` returns the mapping without redirecting.

### FR4 — Delete
- **FR4 (M)** `DELETE /api/v1/urls/{shortCode}` deletes the mapping and invalidates the cache.
- **FR4.1 (S)** Ownership/authz — out of scope for v1 (no accounts); documented in v2.

### FR5 — Statistics
- **FR5 (M)** `GET /api/v1/urls/{shortCode}/stats` returns total clicks, clicks over time,
  and top referrers.
- **FR5.1 (S)** Device/browser breakdown and country where available.
- **FR5.2 (M)** Stats must be **eventually consistent** (analytics are async by design).

### FR6 — Rate limiting
- **FR6 (M)** Per-IP limits on creation; `429 Too Many Requests` + rate-limit headers.
- **FR6.1 (S)** Per-API-key tiers once keys exist.
- **FR6.2 (M)** Documented behaviour when Redis is unavailable (decision: fail-open).

### FR7 — Operations
- **FR7 (M)** `GET /health` (liveness) and `GET /ready` (readiness for dependencies).
- **FR7.1 (M)** Graceful shutdown: stop accepting traffic, drain in-flight requests/events.
- **FR7.2 (M)** Request ID on every request, returned and logged.
- **FR7.3 (M)** Metrics endpoint for Prometheus.

## 6. Non-functional requirements

### NFR1 — Performance (targets to be *measured*, not claimed)
These are planned targets. They are only recorded in `docs/PERFORMANCE.md` after k6 runs.

| Metric | Target | Verification |
|---|---|---|
| Redirect p50 / p95 / p99 | < 5 ms / < 20 ms / < 50 ms (cache hot) | k6, before/after Redis |
| Redirects sustained | ≥ 5,000 req/s across 3 API containers | k6 soak |
| Create p95 | < 100 ms | k6 |
| Error rate under load | < 0.1 % (excl. provoked 429/404) | k6 |
| PostgreSQL query count per redirect | 0 on cache hit, 1 on miss | metrics + EXPLAIN |

### NFR2 — Reliability
- **NFR2 (M)** PostgreSQL is the only durable store. Redis loss loses **no** mappings.
- **NFR2 (M)** Redis outage → redirects degrade to PostgreSQL (slower, still correct);
  rate limiting and analytics are explicitly best-effort during the outage (documented).
- **NFR2 (M)** No single API instance is required for correctness (stateless).
- **NFR2 (S)** Analytics events may be dropped during a Redis/worker outage; documented
  trade-off (measured in FAILURES.md).

### NFR3 — Scalability
- **NFR3 (M)** API instances are stateless and horizontally scalable behind Nginx.
- **NFR3 (M)** Local deployment runs 3 API containers to prove it.
- **NFR3 (S)** Write path scales to PG capacity; document the path to sharding (not built).

### NFR4 — Observability
- **NFR4 (M)** Structured logs (JSON) with request ID, timestamp, latency, status.
- **NFR4 (M)** Prometheus metrics: HTTP totals/durations, cache hit/miss, DB query
  durations, rate-limit rejections, worker processed/failed.
- **NFR4 (M)** Grafana dashboard consuming those metrics.

### NFR5 — Security
- **NFR5 (M)** Input validation on all endpoints; request body size limits.
- **NFR5 (M)** No secrets in the repository; everything from environment / `.env.example`.
- **NFR5 (M)** SQL injection prevented by parameterised queries (pgx).
- **NFR5 (S)** Secure headers, dependency vulnerability scanning in CI.
- **NFR5 (S)** Rate limiting as abuse protection on the write path.

### NFR6 — Testability
- **NFR6 (M)** Automated tests at unit / service / repository / API-integration level.
- **NFR6 (M)** Every failure scenario in FAILURES.md reproduced by a test or a script.
- **NFR6 (M)** `go vet` clean, code covered by real tests, no dead abstractions.

## 7. Assumptions & constraints

- **Tech stack is fixed (as specified)**: Go · PostgreSQL · Redis · Nginx · Docker
  Compose · k6 · GitHub Actions · Prometheus · Grafana.
- **New technology policy**: a new tool must prove one of these and be recorded in
  DECISIONS.md with the four questions (problem, why stack can't solve it,
  complexity introduced, necessity). Kafka, Kubernetes, OpenTelemetry are deferred
  until a demonstrated need exists.
- **Single-region, single-PostgreSQL, single-Nginx** for v1. Distribution beyond one
  PG is design-documented, not implemented.
- **Local-first**: the full system must run with `docker compose up`.
- Runs on the developer's machine (Windows/Windows) during development; deployment
  target decided at Stage 26 (not invented before then).

## 8. Explicitly out of scope for v1

- User accounts, authentication, authorisation, per-link ownership.
  *Decision*: auth shapes the whole data model; we keep v1 honest and add it as v2
  (documented in DECISIONS.md). Rate-limiting tiers are the only "identity" stub.
- Custom domains, vanity branding.
- Multi-region short-code generation, cross-DC replication.
- Kafka; streaming beyond a single Redis Stream is shown to be unnecessary at this
  scale (Stage 15 will justify the threshold).
- Kubernetes (README/CV will say "ready, not run").

## 9. Success criteria

The full 28-point checklist lives in `docs/00-roadmap.md`. The interview rule:
for **every** box ticked, there must be a test, a measurement, or a documented
trade-off that survives a question.

## 10. Open questions for review

1. Redirect status: **301** (permanent, browser/SEO-friendly) vs **302** (transient,
   permits later link change). Proposal: 301 for creator links, revisit with analytics.
2. Expiration: hard TTL in DB vs soft check on read. Proposal: stored `expires_at`,
   checked on the resolution path (cache can serve a stale-but-unexpired result).
3. API keys: introduce in v1 purely as a second rate-limit tier, or defer entirely?
   Proposal: defer to v2 — see `DECISIONS.md`.