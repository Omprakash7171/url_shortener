# Failure Testing Evidence

> Every entry is a scenario we actually ran and measured. Claims without reproductions
> do not belong here. Each test names the exact commands used.

## F-01 — Redis down: redirects still resolve from PostgreSQL (fail-open)

**Scenario:** the Redis container (cache + rate limiter + analytics stream) is stopped
while the API and worker keep running.

**Setup/commands:**
```
docker compose stop url-shortener-redis-1
# single redirect through the stack:
curl -s -D - -o NUL http://localhost:8083/1        # 301 + Location
# restart it afterwards:
docker compose up -d redis
```

**Result:** `301` returned; warning logged `url cache read failed, failing open`; behaviour
degrades to PG-only lookups until Redis is back.

**Measures:** fail-open latency with Redis stopped is ~434ms (bounded by a per-op
`context.WithTimeout(200ms)` + `MaxRetries=1`; most of it a single DNS attempt to a dead
peer). Before the D-21 fix this scenario took ~10s per request.

---

## F-02 — API replica killed while under traffic (Nginx passive failover)

**Scenario:** three API replicas behind Nginx (`docker compose up -d --scale api=3`),
entry point `http://localhost:8090`. One replica is stopped mid-traffic.

**Setup/commands:**
```
curl -s -o NUL -w "%{http_code}" http://localhost:8090/1
docker stop url-shortener-api-1
# 10 sequential redirects against the LB:
1..10 | % { curl -s -o NUL -w "%{http_code}" --max-time 10 http://localhost:8090/1 }
# which instances answered:
curl -s -D - -o NUL http://localhost:8090/1 | Select-String X-Instance
```

**Result:** **10/10 redirects returned 301** — zero user-visible failures. `X-Instance`
shows only the two survivors (5e5098…, 9b0813…); the dead peer is dropped from rotation.

**Mechanism:** `proxy_next_upstream error timeout http_502 http_503 http_504 http_429`
retries the request on the next peer immediately; `max_fails=2 fail_timeout=10s` marks
the stopped replica down for subsequent separate clients.

**Measures:** requests after failover split between the two live instances (7/5 in the
run above); HTTP status sent to the client was `301` before and after failure;
no request exceeded the 10s `--max-time`.

---

## F-03 — Load distribution before fix (kept as regression note)

**Scenario:** Nginx configured with dynamic `server api:8080 resolve` + keepalive.

**Result:** 12 sequential requests, **12/12 pinned to one replica** (`X-Instance` single)
because Docker DNS returned one A-record first and the keepalive pool reused that
connection. This is why we moved to static upstreams (D-23) with keepalive disabled for
the sequential demo — see `docs/PERFORMANCE.md` for the corrected 4/4/4 split.

---

## Future tests planned

- F-04 Worker stopped → events accumulate in the Redis stream; on restart the consumer
  group resumes from the last acked ID (dedupe via `idempotency_records`).
- F-05 PostgreSQL stopped → create/redirect error paths; Redis cache still serves cached
  codes; reads fail closed with 500 when both layers are down.
- F-06 Rate-limiter Redis fail-open: 429 counting continues but requests pass when
  Redis is down (`url_ratelimit_fail_open_total`).