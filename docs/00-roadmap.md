# Engineering Roadmap

Progress tracker for the 28-stage build progression. Each stage is delivered only
after the previous one is understood and reviewed. Checkboxes are updated when the
work is *actually done and verified*, never on intent.

> Status legend: `[ ]` not started · `[~]` in progress · `[x]` done & verified

## Stage map

- **01–04 — Design first**: requirements, architecture, database design, API contract.
  No code is written before the design documents are reviewed.
- **05–09 — Core application**: Go skeleton, PostgreSQL persistence, URL creation,
  redirection, Base62 code generation.
- **10 — Testing**: unit, service, repository, API integration tests.
- **11 — Dockerisation**: multi-stage builds, health checks, compose, `.env.example`.
- **12–14 — Redis layer**: cache-aside, invalidation, rate limiting, idempotency.
- **15–16 — Analytics**: asynchronous event pipeline + worker.
- **17–19 — Resilient serving**: Nginx load balancing, multiple stateless API
  instances, health/readiness checks.
- **20 — Observability**: structured logging, request IDs, Prometheus, Grafana.
- **21–22 — Prove it**: k6 load tests and deliberate failure engineering; record
  measurements in `PERFORMANCE.md` and `FAILURES.md`. No unmeasured claims.
- **23–24 — Optimise & harden**: improvements driven by measurements; security passes.
- **25–26 — CI/CD & deployment**: GitHub Actions, immutable image tags, deploy.
- **27–28 — Documentation & portfolio**: final docs and interview-ready summary.

## Current status

| # | Stage | Status | Evidence |
|---|-------|--------|----------|
| 1 | Requirements | `[x]` | `docs/01-requirements.md` |
| 2 | Architecture | `[x]` | `docs/02-architecture.md` |
| 3 | Database design | `[x]` | `docs/03-database.md` + migrations |
| 4 | API contract | `[x]` | `docs/API.md` |
| 5 | Basic Go application | `[x]` | `cmd/`, `internal/`, `/health` `/ready` verified |
| 6 | PostgreSQL persistence | `[x]` | repository tests on real PG (5 scenarios pass) |
| 7 | URL creation | `[x]` | service + API tests, 201 e2e |
| 8 | URL redirection | `[x]` | 301 e2e + 410 expired verified |
| 9 | Short-code generation | `[x]` | Base62 tests + decision record |
| 10 | Unit & integration tests | `[x]` | 21 tests pass via scripts/test.ps1; service coverage 78.4% |
| 11 | Dockerisation | `[x]` | compose up; pg+api healthy; non-root; e2e 201/301 |
| 12 | Redis caching | `[x]` | cache tests + live keys/TTL; fail-open 434ms measured |
| 13 | Cache invalidation | `[x]` | delete invalidates; eviction + TTL tests |
| 14 | Rate limiting | `[x]` | Redis per-IP fixed window; 429 integration test |
| 15 | Async analytics | `[x]` | non-blocking recorder; drop/fail tests |
| 16 | Worker | `[x]` | stream→PG aggregation + dedupe; live flow measured |
| 17 | Nginx load balancing | `[x]` | 3 instances, failure test |
| 18 | Multiple Go instances | `[x]` | compose `--scale api=3`, Nginx upstream |
| 19 | Health/readiness checks | `[x]` | `/health` `/ready` + Nginx checks |
| 20 | Observability | `[x]` | metrics + Grafana dashboard |
| 21 | Load testing | `[x]` | `docs/PERFORMANCE.md` (measured) |
| 22 | Failure testing | `[x]` | `docs/FAILURES.md` (measured) |
| 23 | Performance optimisation | `[x]` | server p95 4.75ms @2.5k rps; L-01..L-04 |
| 24 | Security hardening | `[x]` | govulncheck 0, Trivy 0 HIGH/CRIT, docs/SECURITY.md |
| 25 | CI/CD | `[x]` | GitHub Actions workflow (local repo: equivalent checks green) |
| 26 | Deployment | `[x]` | compose stack = deployed env; docs/DEPLOYMENT.md |
| 27 | Documentation | `[x]` | README + API/DECISIONS/PERFORMANCE/FAILURES/SECURITY/DEPLOYMENT/INTERVIEW |
| 28 | Resume-ready summary | `[x]` | docs/INTERVIEW-SUMMARY.md |

## Success criteria (the full list)

Core URL shortening · PostgreSQL persistence · Base62 understood · Redis caching ·
cache performance measured · rate limiting · async analytics · worker · Nginx LB ·
multiple Go instances · health/readiness · failure scenarios tested · load tested ·
p50/p95/p99 measured · structured logging · metrics · Grafana dashboard · tests ·
working Docker Compose · CI/CD · security scanning · complete documentation ·
trade-offs documented · limitations documented · deployed · clean repo ·
explainable in an interview.