# URL Shortener

A production-flavoured URL shortener + click-analytics platform, built and verified
stage by stage: PostgreSQL as the system of record, Redis for cache + rate limiting +
an analytics event stream, 3 API replicas behind Nginx, a consumer worker, and a full
Prometheus/Grafana observability loop.

## Measured results (see docs/)

| claim | value | evidence |
| --- | --- | --- |
| Sustained throughput (cache-hot, 3 replicas) | 2,500 rps, 0 failures | `docs/PERFORMANCE.md` L-02 |
| Server-side latency p50 / p95 / p99 | 2.5 / 4.75 / 4.95 ms | L-03 |
| Fail-open latency, Redis stopped | ~434 ms/request | L-04 + `docs/FAILURES.md` F-01 |
| Replica failure while under load | 10/10 requests still 301 | F-02 |
| Unit/integration tests | 21 passing | `scripts/test.ps1` |
| `govulncheck` / Trivy scan | 0 / 0 HIGH+CRITICAL | `docs/SECURITY.md` |

## Quick start

```
docker compose up -d --scale api=3 --build
```

Entry point: **http://localhost:8090** (Nginx → 3 API replicas).

- Grafana: http://localhost:3001 (admin/admin) — "URL Shortener Observatory"
- Prometheus: http://localhost:9090
- PostgreSQL: localhost:5433, Redis: localhost:6389

## Endpoints (`docs/API.md` has the full contract)

| method | path | purpose |
| --- | --- | --- |
| `GET` | `/health`, `/ready` | liveness / readiness |
| `GET` | `/metrics` | Prometheus exposition |
| `POST` | `/api/v1/urls` | create (rate-limited per IP, 60/min) |
| `GET` | `/api/v1/urls/{code}` | URL metadata |
| `DELETE` | `/api/v1/urls/{code}` | delete + cache invalidation |
| `GET` | `/api/v1/urls/{code}/stats` | total/daily/referrer click stats |
| `GET` | `/{code}` | 301 redirect + async analytics event |

## Architecture in one breath

Create → Base62 short code → PostgreSQL (source of truth). Redirect → Redis cache
(cache-aside, 60s TTL, fail-open) else PostgreSQL, then a non-blocking event lands in a
Redis stream; the worker consumer-group aggregates it into `analytics_daily` with
idempotency dedupe. All of it runs read-hot on 3 stateless replicas behind a passive-health
Nginx; every layer feeds Prometheus/Grafana and structured logs.

## Documentation index

- `docs/00-roadmap.md` — 28 stages, what is `[x]` and why
- `docs/01-requirements.md`, `docs/02-architecture.md`, `docs/03-database.md`
- `docs/API.md` — HTTP contract + error envelope
- `docs/DECISIONS.md` — D-01..D-25, every trade-off with "how we tested it"
- `docs/PERFORMANCE.md` — load-test evidence (L-01..L-04)
- `docs/FAILURES.md` — failure-test evidence (F-01..F-03)
- `docs/SECURITY.md` — scans + defences + known limits
- `docs/DEPLOYMENT.md` — run/deploy the stack
- `docs/INTERVIEW-SUMMARY.md` — the 3-minute walkthrough

## Local testing (Windows note)

`scripts/test.ps1` compiles test binaries with `go test -c -o bin\*` first because
Windows App Control can block `go test` from Go's temp cache:

```
.\scripts\test.ps1     # needs: dev PG on 5439 with urlshortener_test, redis on 6389
```

## Stage notes / choices

Go, chi v5, pgx v5, go-redis, Prometheus client. 28-stage build with every stage
verified (live API calls, tests, or measured failure) before advancing. See
`docs/DECISIONS.md` for the why behind every call.