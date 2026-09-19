# Resume-Ready Summary (interview walkthrough)

> What to say when asked "walk me through this project" in 3 minutes, then in 15.
> References `docs/DECISIONS.md` (D-nn) for every "why".

## The pitch (90 seconds)

I built a URL shortener with click analytics where the two hot paths are engineered
separately: **writes are durable, reads are cache-fast and fail-open.** When someone
creates a URL, PostgreSQL is the system of record (D-01). When someone clicks one, a
Redis-fuelled cache answers without touching the database; if Redis or the DB hiccups,
redirects keep working (D-20/D-21). Clicks become analytics without blocking the request:
each 301 pushes a non-blocking event into a Redis stream (buffer 4096, drop-and-count
instead of stalling), and a separate worker consumes it into `analytics_daily`, deduped
via an idempotency table so at-least-once delivery is safe (D-18). All of it runs as
three stateless API replicas behind an Nginx load balancer with passive health checks —
I killed one replica mid-traffic and 10/10 requests still returned 301 (F-02). The whole
stack is `docker compose up --scale api=3`, and Prometheus + Grafana monitor it.

## The 15-minute version (what I defend)

1. **Layering** — PG owns truth; Redis only holds copies with TTLs. A `FLUSHALL` is a
   cold cache, not an outage. Analytics queries run in SQL where they belong (D-01).
2. **Base62 short codes** (D-10) — a sequence + Base62 instead of hashes: `9,999,999` in
   4 chars, no collision logic to get wrong, collision handling is an INSERT conflict.
3. **Cache-aside with bounded staleness** (D-02) — min(60s, remaining URL expiry) TTL,
   expired entries treated as misses, deletes invalidate synchronously; hit/miss/fail-open
   all instrumented.
4. **Fast fail-open** (D-21) — every Redis op is under a 200ms deadline with
   `MaxRetries=1`; measured 434ms worst case with Redis completely down, vs ~10s before
   the fix. This is the "I optimised a real failure mode" story.
5. **Rate limiting** (D-14/D-22) — Redis fixed-window per IP on the write path, 429 +
   Retry-After, fails open (and counts it) when Redis is down.
6. **Analytics pipeline** (D-15..D-18) — non-blocking recorder, then a consumer-group
   worker with batch reads, `Xbacklog`-style claim via `idempotency_records`
   (`ON CONFLICT DO NOTHING`, 24h expiry) — that's the at-least-once guarantee.
7. **Nginx + replicas** (D-23) — static upstreams and why (measured: dynamic DNS kept
   every client on one replica; static names gave an even 4/4/4 split); passive health
   checks + `proxy_next_upstream` for zero-downtime replica loss (F-02).
8. **Observability** (D-24/D-25) — per-process Prometheus endpoints; Grafana dashboard
   provisioned in Compose. A stage-21 engineer-authored bug (unobserved histogram) is
   documented as D-25 — honest, self-aware.
9. **Hygiene** — 21 tests incl. integration suites, `govulncheck` + Trivy clean,
   non-root container, structured logs with `request_id`, README + 8 docs.

## Numbers to quote (all measured)

- 2,500 rps sustained, 0 failures (k6, 3 replicas).
- p50/p95/p99 = 2.5/4.75/4.95 ms (Prometheus histogram).
- 10/10 requests survived one killed replica.
- ~434 ms worst-case fail-open (Redis down).
- 21/21 tests green; service coverage 78.4%; 0 vulnerabilities.

## Honest limitations (say these first if asked)

- No auth: it's a public shortener by design.
- Nginx upstreams are static (Compose names, deterministic) — real scaling wants
  service discovery; documented in D-23.
- TLS/HTTPS terminates wherever you host it; the demo Compose is HTTP on localhost.
- Latency numbers are single-host Docker, not a cloud benchmark.

## Five questions an interviewer might lead with

- "Why Redis instead of PG for reads?" → D-01/D-02/D-20 (it's a cache, PG stays truth).
- "How do you know the cache is right?" → counters + TTL + eviction tests + fail-open.
- "What happens if the worker dies mid-stream?" → consumer group resumes at last ACK;
  idempotency table dedupes re-deliveries (D-18).
- "Race: delete during a redirect?" → cache invalidation is synchronous with the delete;
  stale entry can serve once, bounded by TTL (D-02).
- "Why not just use a hash of the URL?" → D-10 (sequence+Base62; predictable length,
  no collision retries).