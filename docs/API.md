# API Contract — v1

> Version: 1.0 draft. This is the interface the service implements; changes to it
> require a review against the failure/scenario matrix. All paths are HTTP, JSON
> bodies, UTF-8. Timeouts, request IDs, validation and error shapes are contract
> (they are tested, not optional).

## 1. Base URL and versioning

- Versioned by URL prefix: `/api/v1/...` (immutable once released; incompatible
  change ⇒ new major). Redirects deliberately live **outside** the versioned prefix:
  `GET /{code}` is the public face of a link, not the API surface.
- Host layout (Stage 11+): Nginx exposes the service on `:80` (prod) / `:8090` (local);
  all paths below are relative to that.

## 2. Common behaviour

### 2.1 Request headers
| Header | Required | Meaning |
|---|---|---|
| `Content-Type: application/json` | for bodies | enforced; `415` otherwise |
| `X-Request-Id` | no | caller-supplied trace id; if absent the service generates one. **Always** echoed on the response and present in logs/metrics. |
| `Idempotency-Key` | on POST `/urls` | makes create retry-safe (see §6). |

### 2.2 Error envelope (every 4xx/5xx)
```json
{
  "error": {
    "code": "custom_code_taken",
    "message": "custom code 'x' already exists",
    "request_id": "ab12...",
    "details": {}
  }
}
```
- `code` — stable, machine-readable string (list in each endpoint). Never free-form.
- `message` — human-readable, safe to send to a client.
- `details` — validation field errors when relevant (shape `{"field": ["reason"]}`).

### 2.3 Status codes used
| Code | Meaning |
|---|---|
| `200` | OK |
| `201` | created (create + idempotent replay) |
| `204` | deleted (no body) |
| `301` | permanent redirect |
| `400` | malformed/invalid request (validation) |
| `404` | unknown short code |
| `409` | custom code already taken |
| `410` | link existed, now expired |
| `413` | request body too large |
| `415` | wrong content type |
| `429` | rate limited (with `RateLimit-*` headers) |
| `500` | unexpected internal error |
| `503` | dependency unavailable (PG down during a write; `/ready` when not ready) |

### 2.4 Timeouts
- Server `ReadTimeout: 10s`, `WriteTimeout: 10s`, `IdleTimeout: 60s` (chosen so a
  slow client cannot hold a connection; tuned under k6 in Stage 23).
- Per-dependency context timeouts: **Redis 100 ms**, **PostgreSQL 500 ms** (writes
  get the full budget). Anything slower is declared down, logged with the request id.

### 2.5 Graceful shutdown
- SIGTERM/SIGINT → stop accepting new conns (`http.Server.Shutdown(ctx)`), drain
  in-flight requests (≤ 10s), flush analytics buffer, close pools. Contract: no
  in-flight request is killed mid-write.

## 3. Endpoints

### 3.1 `POST /api/v1/urls` — create
Request:
```json
{
  "url": "https://example.com/very/long/path?q=1",
  "customCode": "my-link",        // optional
  "expiresIn": "7d"               // optional; "365d", "24h", "30m". Absent = never
}
```
Responses:
- **201** — created (body below).
- **200** — idempotent replay: the caller sent the same `Idempotency-Key` as an
  earlier create; body is the **exact original** (same `shortCode`/`createdAt`),
  response header `Idempotency-Replay: true` (§8).
```json
{
  "url": "https://example.com/very/long/path?q=1",
  "shortCode": "aB3x9Q",
  "shortUrl": "http://localhost:8090/aB3x9Q",
  "createdAt": "2026-09-18T10:00:00Z",
  "expiresAt": null
}
```
- **400** `code` values: `invalid_json`, `url_invalid`, `url_too_long`,
  `self_reference` (shortening the shortener), `custom_code_invalid`,
  `expires_in_invalid`, `idempotency_key_too_long` (`Idempotency-Key` > 256 chars).
- **413** `body_too_large` (request body > 1 MiB).
- **409** `custom_code_taken` · `idempotency_in_progress` (another request with the
  same `Idempotency-Key` is mid-flight; retry shortly).
- **429** `rate_limited` + `RateLimit-*` headers (§7).
- **503** `unavailable` when the write cannot reach PostgreSQL.

Validation rules (enforced in service, shapes enforced in handler):
| Field | Rule |
|---|---|
| `url` | required; ≤ 2048 chars; scheme must be `http`/`https` (after `http://` prepend normalisation); host must parse and be a valid domain/IP; must not be this service's own host (loop guard) |
| `customCode` | optional; `^[A-Za-z0-9_-]{3,32}$`; must not equal a reserved path (`api`, `health`, `ready`, `metrics`) |
| `expiresIn` | optional; `^(\d+)(m|h|d)$`; max 365d. Absent ⇒ never expires |

### 3.2 `GET /api/v1/urls/{code}` — metadata (no redirect)
- **200** — same JSON mapping object as create (without request-level fields).
- **404** `not_found` · **410** `link_expired`.

### 3.3 `DELETE /api/v1/urls/{code}` — delete
- **204** no body. Cache is invalidated. (`ON DELETE CASCADE` removes stats.)
- **404** `not_found` · **410** `link_expired`.

### 3.4 `GET /api/v1/urls/{code}/stats` — statistics
- **200**
```json
{
  "totalClicks": 4821,
  "daily": [ { "day": "2026-09-12", "clicks": 302 } ],
  "topReferrers": [ { "referrer": "github.com", "clicks": 1410 } ]
}
```
- **404** `not_found` · **410** `link_expired`.
- Counters are **aggregated** to `analytics_daily` by the worker (eventual
  consistency, bounded by stream lag — measured in Stage 20s).

### 3.5 `GET /{code}` — redirect (public URL, no version prefix)
- **301** `Location: <expanded url>` (D-08). No body.
- **404** unknown · **410** expired.
- Not rate-limited (the write path is the abuse vector; reads are cache-served).
- Enqueues exactly one analytics event per check (non-blocking).

### 3.6 `GET /health` — liveness
- **200** `{"status":"ok"}`. Process is alive. No dependency checks.

### 3.7 `GET /ready` — readiness
- **200** `{"status":"ready"}` when PG reachable **and** Redis reachable (Redis is
  optional per fail-open policy — see note below).
- **503** when dependencies are not up; Nginx uses this to remove a container.
- Note (D-16): `/ready` fails on PG-down; it **does not** fail on Redis-down
  (redirects still work — fail-open), but includes `"redis":"degraded"` in the body.

## 4. Redirect semantics

- `GET /{code}` resolves via cache-aside; a code with `expiresAt <= now` → `410`.
- Codes are case-sensitive (`aB3` ≠ `ab3`) — enforced by the Base62 alphabet.
- Response timing contract: cache hit p50 < 5 ms, cache-miss p50 < 20 ms (targets to
  be *measured* in docs/PERFORMANCE.md, not claimed).

## 5. Rate limiting (`POST /api/v1/urls` only)

- Sliding window, per client (`X-Forwarded-For` first hop behind Nginx, falls back to
  remote IP). Default: **10 requests / 60 s** (config, `RATE_LIMIT` env).
- Response headers on every create response:
  - `RateLimit-Limit: 10`
  - `RateLimit-Remaining: 7`
  - `RateLimit-Reset: <unix seconds when window reopens>`
- On exceeding: **429** `rate_limited` with the same headers (`Remaining: 0`).
- Redis-down ⇒ **fail-open** (no limiting, loud metric + log) — D-07/D-16.

## 6. Idempotency-Key

- Scope: `POST /api/v1/urls`. Value: client-supplied, ≤ 256 chars, sent as the
  `Idempotency-Key` request header.
- First create with a key **wins**: it records the resulting `shortCode` in Redis
  (`us:idem:resp:{key}`, TTL 24 h). Any later create with the same key replays the
  **exact original** response — HTTP `200`, same `shortCode`/`createdAt`,
  header `Idempotency-Replay: true` (the create that first won replies `201` with
  `Idempotency-Replay: false`).
- Concurrency: the first request claims the key in Redis (`SET NX` lock, 10 s TTL); a
  concurrent request with the same key waits briefly (≤ 1 s) for the winner's result
  and replays it, else gets `409 idempotency_in_progress`. A crashed winner's lock
  self-expires. Cross-replica safe because the store is shared Redis (D-26).
- Redis unavailable ⇒ **fail-open**: the create proceeds without dedupe (loud
  `url_idempotency_skipped_total` + warn log); duplicates are possible during the
  outage (documented caveat, mirrors D-21).
- No key ⇒ normal behaviour; a retry without a key may create a duplicate resource
  (documented caveat).

## 7. Observability hooks (contract for Stage 20)

- Every request carries a request id → response header + every log line + error body.
- `/metrics` (Prometheus) exported; metric names follow `docs/ARCHITECTURE.md` §.
- Structured logs: JSON lines `{ts, level, request_id, method, path, status, dur_ms}`.

## 8. API.md → code traceability

Each endpoint above is implemented as one handler in `internal/handler`, one service
method, one repository method. Contract violations are caught by API-level tests in
`tests/` (Stage 10). The error `code` strings in §3 are the exact strings tests assert.