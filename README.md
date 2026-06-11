# Blockli Go Redis API

Go HTTP service that gives Blockli Studio and the Bunny Edge Script controlled access to Redis. It is the fourth runtime component in the Blockli mobile platform, alongside Studio Cloud, the WordPress plugin, and the Expo mobile shell.

The service is designed for Bunny Magic Containers with a Redis sidecar, but it can run anywhere that provides a Redis connection URL.

## Current Architecture

```text
Blockli Studio
  -> PUT /cache
  -> stores published app config as blockli:appconfig:{appId}

Expo Mobile App
  -> cache.blockli.app
  -> Bunny Edge Script
       -> GET/PUT /cache for WordPress REST response caching
       -> GET /apps/{appId}/app-config.json for published app config
            -> Go API
            -> Redis

WordPress Plugin
  -> Bunny Edge Script invalidation webhook
  -> Edge Script calls POST /cache/purge
```

The Go service does not authenticate WordPress users and does not proxy WordPress content itself. The Bunny Edge Script owns tenant routing, cache-key construction, origin requests, response shaping, and bearer-token cache scoping.

## Endpoints

### Health

```text
GET /health
```

Returns HTTP 200 even when Redis is temporarily unavailable so the container is not restarted for a transient sidecar failure. Inspect the `redis` field:

```json
{
  "status": "ok",
  "redis": true
}
```

### Read Cache Value

```text
GET /cache?key={redis-key}
X-Cache-Token: {CACHE_API_TOKEN}
```

Returns the raw stored JSON or HTTP 404 when the key is missing.

### Store Cache Value

```text
PUT /cache?key={redis-key}&ttl={seconds}
X-Cache-Token: {CACHE_API_TOKEN}
Content-Type: application/json
```

The request body is stored unchanged with the requested Redis TTL. The default TTL is 300 seconds and request bodies are capped at 10 MB. Success returns HTTP 204.

### Purge Cache Keys

```text
POST /cache/purge
X-Cache-Token: {CACHE_API_TOKEN}
Content-Type: application/json

{
  "pattern": "blockli:prod:app-id:activity:*"
}
```

The service uses Redis `SCAN` followed by `DEL`, avoiding the blocking `KEYS` command.

### Public App Config

```text
GET /apps/{appId}/app-config.json
```

This endpoint is intentionally public because Bunny CDN and the mobile app must retrieve published app configuration without a cache token. It reads:

```text
blockli:appconfig:{appId}
```

The response is JSON with `Cache-Control: public, max-age=300`. Studio writes the underlying Redis value with a 30-day TTL each time an app configuration is published.

## Authentication

`GET /cache`, `PUT /cache`, and `POST /cache/purge` require `X-Cache-Token` when `CACHE_API_TOKEN` is configured.

`GET /health` and `GET /apps/{appId}/app-config.json` are public.

If `CACHE_API_TOKEN` is empty, protected endpoints accept unauthenticated requests. That behavior is for local development only and must not be used in production.

## Redis Key Families

Published app configuration:

```text
blockli:appconfig:{appId}
```

Edge-cached WordPress/provider responses:

```text
blockli:{environment}:{appId}:{routeType}:{routeHash}:{queryHash}:{authScope}:{appVersion}
```

The Bunny Edge Script creates response-cache keys. This service treats keys and values as opaque data.

## Environment

```text
REDIS_URL            Redis URL, default redis://localhost:6379
CACHE_API_TOKEN      shared secret for protected endpoints
PORT                 HTTP port, default 8080
CORS_ALLOWED_ORIGINS allowed browser origins, default *
```

## Local Development

Run the Go service and Redis together:

```bash
docker compose up --build
```

The local stack uses:

```text
API:   http://localhost:8080
Redis: redis://redis:6379
Token: dev-secret
```

Smoke test:

```bash
curl http://localhost:8080/health

curl -X PUT "http://localhost:8080/cache?key=test&ttl=60" \
  -H "X-Cache-Token: dev-secret" \
  -H "Content-Type: application/json" \
  -d '{"hello":"world"}'

curl "http://localhost:8080/cache?key=test" \
  -H "X-Cache-Token: dev-secret"

curl -X POST http://localhost:8080/cache/purge \
  -H "X-Cache-Token: dev-secret" \
  -H "Content-Type: application/json" \
  -d '{"pattern":"test*"}'
```

Run directly against an existing Redis instance:

```bash
REDIS_URL=redis://localhost:6379 \
CACHE_API_TOKEN=dev-secret \
PORT=8080 \
go run .
```

## Deployment

The included Dockerfile builds a static Linux binary and exposes port `8080`. `bunny.json` defines the intended Magic Containers deployment with:

- The Go application container.
- A Redis 7 sidecar.
- A persistent volume mounted at `/data`.
- A public HTTP endpoint for the Go API.

Pushes to `main` run `.github/workflows/deploy.yml`, which builds and publishes the container to GHCR and updates the configured Bunny Magic Containers application.

Required GitHub repository configuration:

```text
Variable: APP_ID
Secret:   BUNNYNET_API_KEY
```

Production must also configure `CACHE_API_TOKEN` in Magic Containers and store the same URL/token on the corresponding Studio app record.

## Integration Boundaries

- Studio calls the protected cache API when publishing app config and from manual purge controls.
- The Bunny Edge Script looks up each app's `cache_api_url` and `cache_api_token` in the shared Studio database.
- The Bunny Edge Script uses this service for cached WordPress/provider responses.
- The Bunny Edge Script delegates `/apps/{appId}/app-config.json` to this service.
- Expo normally accesses this service through `cache.blockli.app`, not through the Magic Container URL directly.

## Current Limitations

- There are no automated Go tests yet.
- The API token is service-wide; per-app isolation is currently provided by Studio registry routing and key naming rather than independent API credentials enforced by this service.
- The public app-config endpoint accepts any path-safe app ID and relies on possession of the app ID plus the public nature of mobile configuration.
- Redis durability and regional replication depend on the selected Magic Containers/Redis deployment configuration.
