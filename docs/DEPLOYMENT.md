# Deployment

## The deliverable: a Compose-provisioned production-like environment

The project ships as a self-contained `docker compose` stack. It is the "deployed"
environment for this project: a fresh machine with Docker can bring up the entire
product (Postgres + Redis + 3 API replicas behind Nginx + analytics worker +
Prometheus + Grafana) with five commands. This is what `/` actually runs on today.

### Local single-node deployment

```
docker compose up -d --scale api=3 --build
```

| service | host endpoint | role |
| --- | --- | --- |
| Nginx LB | `http://localhost:8090` | public entry, round-robins the 3 replicas |
| API (x3) | internal `:8080`, no host ports | CRUD, cache, rate limit, analytics feed |
| worker | internal `:8081/metrics` | Redis stream → PostgreSQL aggregation |
| PostgreSQL | `localhost:5433` | system of record |
| Redis | `localhost:6389` | cache, rate-limit, event stream |
| Prometheus | `http://localhost:9090` | scraping api-{1,2,3}:8080 + worker:8081 |
| Grafana | `http://localhost:3001` (admin/admin) | "URL Shortener Observatory" dashboard |

Compose names are deterministic because the project directory is `url-shortener`
(`url-shortener-api-{1..3}`); nginx references them statically (D-23). If the project
folder is renamed, update `deploy/nginx.conf` and `deploy/prometheus.yml` accordingly.

### Recommended smoke + verification after any deploy

```
curl -s -D - http://localhost:8090/health            # -> 200
curl -s -X POST http://localhost:8090/api/v1/urls \
  -H 'Content-Type: application/json' -d '{"url":"https://example.org"}'   # -> 201 {shortUrl}
curl -s -o NUL -w "%{http_code}" http://localhost:8090/<code>             # -> 301
curl -s http://localhost:8090/metrics | head                              # -> url_* counters
curl -s http://localhost:9090/api/v1/targets | grep -c '"health":"up"'    # -> 4
```

### Production notes (what a real host would add, all documented as limits)

- Terminate TLS at Nginx (the demo serves HTTP; certificate provisioning is out of scope).
- Pin image tags and add a secrets manager for `DATABASE_URL` instead of Compose env.
- Run the API as a replicated service with real discovery (D-23 limitation); keep
  `X-Instance` off for public exposure.
- Persist `pgdata`/`grafana-data` volumes (already declared).
- Next stage after "public": a `deploy/` manifest for a PaaS/K8s target.

## Example "how to take this to a cloud box" checklist

1. Copy the repo; `docker compose build`.
2. Point `BASE_URL` at the public hostname in `docker-compose.yml`.
3. Add TLS certs to Nginx; disable `stub_status` external access on prod.
4. Advance the static upstream names to DNS/service discovery.
5. Re-run `scripts/test.ps1` + the k6/F-ran scenarios against the public endpoint.