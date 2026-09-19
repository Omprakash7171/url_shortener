# Security

> What we actually did and measured (stage 24). Nothing here is aspirational.

## Vulnerability scan results

| scan | tool | result |
| --- | --- | --- |
| Go module vulns | `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` | **No vulnerabilities found** |
| Container image (HIGH/CRITICAL) | `docker run --rm -v /var/run/docker.sock:/var/run/docker.sock aquasec/trivy:0.55.2 image --severity HIGH,CRITICAL --no-progress url-shortener-api` | **Total: 0 (HIGH: 0, CRITICAL: 0)** |
| `go vet ./...` | standard | clean |

## Defences already in place

- **Non-root container user** — the Dockerfile runs `server` as UID 100 (not root), so a
  webshell can't gain root in the image.
- **Minimal base** — final image is `alpine:3.21`; the Go toolchain is only in the builder.
- **No secrets in the repo** — `.env.example` documents variables; `DATABASE_URL`/`REDIS_ADDR`
  are supplied at run time via Compose. `.gitignore` excludes `.env` and `bin/`.
- **Rate limiting on the write path** — Redis fixed-window per IP (stage 14), 429 + Retry-After.
- **Input validation** — request bodies capped (`MaxBodyBytes`), JSON only; malformed
  bodies rejected early with structured errors.
- **Structured logging** — `request_id` on every line; credentials never logged.
- **Readable readiness** — `/ready` checks the DB pool; load-balancer health checks hit it.
- **Dependency hygiene** — only the minimal set of Go modules (chi, pgx, go-redis, prometheus).

## Known limits (documented, not fixed)

- TLS is provided by whatever fronts this in production (terminating at Nginx); the demo
  Compose serves HTTP on `localhost`.
- `X-Instance` leaks the container hostname to clients (used deliberately as an LB proof;
  remove the `WithInstanceID` option before public exposure).
- Rate limiting trusts `X-Forwarded-For` for the client IP once Nginx is in front (a
  spoofable-but-pragmatic proxy default; fine for v1).
- Nginx `stub_status` is restricted to 127.0.0.1 in its own container.
- No authentication/authorization on the API — public shortener by design (a fixture to
  discuss in interviews, not an oversight).