# Performance Evidence

> All numbers below are measured runs, with the exact commands/environment noted
> alongside. Environment: Windows 11 host, Docker Desktop with a single host network
> for all containers, 3 API replicas + nginx behind it. Load from `grafana/k6:0.53.0`
> talking to `http://host.docker.internal:8090` (the Nginx LB).

## L-01 — Sustained redirect throughput through the LB (cache-hot)

**Command:**
```
Get-Content scripts\k6-load.js -Raw | docker run --rm -i grafana/k6:0.53.0 run - ^
  -e BASE=http://host.docker.internal:8090 -e RATE=500 -e CODE=1
```

**Result:** 30,061 iterations in 60s (~500 rps sustained), **0 failed requests**,
0 interrupted. Client-side latency from k6: med/p90/p95 ≈ 0.4ms (sub-millisecond;
all components colocated on one Docker host).

## L-02 — Saturation attempt at 2,500 rps

**Command:** same as L-01 with `RATE=2500`.

**Result:** 150,059 iterations in 60s, **no failures, no threshold crosses**. The stack
kept up; we did not drive it to the breaking point (the shared Windows host, not the app,
would be the next bottleneck). Rate limiting side-effect: the `creates` scenario hit the
60/min per-IP limiter whenever it exceeded 1 rps from one IP, returning 429 — counted as
expected behaviour, not failure.

## L-03 — Server-side percentiles (Prometheus histogram)

The API records every request into `url_http_request_duration_seconds`. Queried after the
heavy runs above, window `[10m]`:

| percentile | value |
| --- | --- |
| p50 | 2.50 ms |
| p95 | 4.75 ms |
| p99 | 4.95 ms |

Query:
```
histogram_quantile(0.99, sum(rate(url_http_request_duration_seconds_bucket[10m])) by (le))
```

Notes: the latency includes LB overhead plus the cache read (Redis `GET` per redirect);
a cache miss adds a PostgreSQL round-trip on the same host.

## L-04 — Fail-open latency with Redis stopped (from F-01)

~434ms per request. Bound by `context.WithTimeout(200ms)` per Redis op + `MaxRetries=1`
(D-21). Before D-21 this was ~10s/request.

## Cross-checks and caveats

- Client (k6) < 1ms vs. server (Prometheus) ~2.5–5ms: both are real; k6 measures on the
  same host over loopback, Prometheus includes container networking + scrape sampling.
- These numbers are not "production on AWS" claims — they prove the architecture and the
  measurement loop (k6 + histogram + Grafana) work end-to-end.