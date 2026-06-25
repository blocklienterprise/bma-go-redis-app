# Realtime Messaging — Architecture & Decision Record

Live BuddyBoss messaging for the Blockli Expo app: instant delivery, **typing
indicators**, presence, and read receipts — WhatsApp-style — built on this Go
service (`bma-go-redis-app`) running on Bunny Magic Containers with Redis.

This service is the realtime layer **beside** WordPress, not a replacement for
it. WordPress/BuddyBoss stays the source of truth for message persistence;
WordPress (PHP-FPM) cannot hold long-lived connections, so live delivery,
typing, and presence flow through here.

---

## Decision: Bunny Magic Containers ✅

Verified against Bunny's docs that the platform supports everything we need:

| Requirement | Support | Notes |
|---|---|---|
| WebSockets (`ws`/`wss`) | ✅ native at the edge | Pull Zone → General → **WebSockets** toggle. The Magic Container CDN endpoint is a pull zone, so it applies to the endpoint we already have. |
| Persistent connections | ✅ pay-as-you-go | ~$0.235 / million connection-minutes (≈ $10/mo per 1k always-on). |
| Concurrency | ✅ 500 default → 25,000 self-serve | Min 100; >25k via sales. |
| Raw TCP/WS fallback | ✅ Anycast endpoint | Escape hatch if we ever want WS outside the CDN layer. |
| Sticky sessions | ✅ header/cookie | Pins a socket to a pod; helps reconnects. |
| Region/scaling control | ✅ Single Region or global + replica count | Lets us start single-pod and scale deliberately. |

Sources: docs.bunny.net/docs/cdn-websockets,
docs.bunny.net/docs/magic-containers-how-to-expose-your-app-to-the-internet,
bunny.net/blog/migrating-from-heroku-to-magic-containers.

**Open empirical check (Phase 0):** docs confirm WS is supported, but only a
deploy proves the upgrade passes through *our* zone. The `/ws/echo` spike
(`ws_echo.go` + `ws-echo-test.html`) answers this in one deploy.

---

## Locked decisions

1. **Platform:** extend this service (`bma-go-redis-app`).
2. **Transport:** WebSocket (`wss`) over the CDN endpoint, WebSockets toggled on.
   Prefer a **dedicated pull zone / subdomain** (`rt.blockli.app`) so realtime
   connection limits and billing stay isolated from `cache.blockli.app`.
3. **Redis:** the per-pod **sidecar** is fine for v1 (Single Region, 1 replica) —
   it doubles as the pub/sub bus. The realtime code reads `REALTIME_REDIS_URL`
   (defaulting to the sidecar `REDIS_URL`), so moving to a **shared Redis** when
   we add replicas is a config change, not a rewrite. The cache sidecar usage is
   untouched.
4. **Auth:** Go validates the app's Bearer token against WordPress
   (`GET /wp-json/buddyboss/v1/members/me`) once per connection, caching
   `token → userID` in Redis (`EX 300`).
5. **Source of truth:** BuddyBoss REST for persistence (send / history /
   mark-read). A WP hook (`bp_messages_message_after_save`) calls Go
   `POST /internal/publish` to fan messages out live — so messages sent from the
   website also appear live in the app.
6. **Keepalive:** ~25–30s heartbeat ping (defeats any idle timeout and powers
   presence).

---

## Why per-pod Redis needs care

The sidecar Redis (`redis://localhost:6379`) is **co-located with each pod**.
Redis pub/sub only fans out *within* a Redis instance, so with multiple pods a
message published on pod A never reaches a subscriber on pod B.

- **v1:** Single Region + **1 replica** → sidecar is the bus. Zero extra infra.
- **Scale-out:** add a **shared Redis** (a small second Magic Container, or
  managed Redis) reachable by all pods; set `REALTIME_REDIS_URL` to it. Sticky
  sessions help reconnects but do **not** remove this need — two users in the
  same thread can land on different pods.

---

## Architecture

```
 Expo app ──REST (send / history / mark-read)──▶ WordPress + BuddyBoss
    │                                              │  bp_messages_message_after_save
    │  wss://rt.blockli.app/ws  (1 conn)           │  POST /internal/publish (X-Internal-Token)
    │   ↑ subscribe, typing, ping                  ▼
    └────────────────────────────────▶  Go realtime svc (this service)
                                          • validate Bearer → WP /members/me (cached EX 300)
                                          • hub: userID → conns → subscribed threads
                                          • bridge WS ⇄ Redis pub/sub
                                                   │
                                          Redis (sidecar v1 / shared at scale)
                                          • PUB/SUB  thread:{id}
                                          • typing:{thread}:{user}   EX 6
                                          • presence:{user}          EX 30
                                          • authcache:{tokenHash}    EX 300

 background → FCM/APNs push (existing expo-notifications) → foreground → reconnect + REST catch-up
```

### Channels & keys

| Redis name | Type | TTL | Purpose |
|---|---|---|---|
| `thread:{threadId}` | pub/sub channel | — | All live events for a thread (typed envelope). |
| `typing:{threadId}:{userId}` | string | 6s | Self-healing typing state; recovers if a "stop" is lost. |
| `presence:{userId}` | string | 30s | Existence = online; refreshed by heartbeat. |
| `lastseen:{userId}` | string | — | Timestamp written on disconnect. |
| `authcache:{tokenHash}` | string | 300s | Cached `token → userID` to avoid a WP call per reconnect. |

### Event envelope (WS frames and Redis payloads)

```json
{
  "type": "message.new | message.read | typing | presence",
  "thread_id": 123,
  "user_id": 45,
  "data": { },
  "ts": 1719300000
}
```

---

## Flows

### Connect & auth (Go calls WP to validate)
1. Client opens `wss://rt.blockli.app/ws` with `Authorization: Bearer {token}`
   on the handshake (RN `WebSocket` supports a headers option — keeps the token
   out of the URL/logs).
2. Go checks `authcache:{tokenHash}`; on miss, calls WP `members/me`, then caches
   `token → userID` (`EX 300`).
3. Client sends `{type:"subscribe", thread_ids:[…]}`. Go verifies thread
   membership (participant set cached per thread) before subscribing the conn to
   `thread:{id}`.

### New message (BuddyBoss authoritative)
1. Client `POST`s to BuddyBoss `/messages` (existing path) and gets the saved
   message back — optimistic UI in the meantime.
2. WP hook `bp_messages_message_after_save` → Go `POST /internal/publish`
   (`X-Internal-Token`) → Go `PUBLISH`es `message.new` to `thread:{id}`.
3. Subscribed pods deliver to their local connections.

### Typing (pure realtime — never touches MySQL)
- Client debounce: emit `typing:true` on first keystroke, re-emit every ~3s
  while typing; emit `typing:false` after ~3s idle, on send, or on blur/close.
- Go sets `typing:{thread}:{user}` `EX 6` and publishes to the thread.
- Receivers show dots; clear on `false`, on that user's `message.new`, or on the
  6s timer (covers a lost "stop").

### Read receipts
- Client marks read via BuddyBoss REST; WP hook publishes `message.read`
  (`up_to_message_id`) to the thread.

### Presence / last seen
- Client heartbeat (~15–25s) refreshes `presence:{user}` `EX 30`; publish
  online↔offline transitions only (not every beat) to limit noise. On
  disconnect, write `lastseen:{user}`.

### Background
- WS drops when the app backgrounds. New messages arrive via push (existing
  `expo-notifications`). On foreground: reconnect WS + REST catch-up by
  last-message-id.

---

## Build phases

| Phase | Scope | Gate / outcome |
|---|---|---|
| **0 — Spike** ← *current* | `/ws/echo` + browser test client | Does Bunny pass the WS upgrade through our zone? |
| 1 | `REALTIME_REDIS_URL`, WS auth (WP validate + cache), hub, subscribe, `message.new` fan-out via WP hook | Live message delivery |
| 2 | **Typing indicators** (client debounce + Redis TTL + fan-out) | Headline feature |
| 3 | Presence / last-seen + read receipts | |
| 4 | Reconnect/backoff, background→push handoff, REST catch-up | Robustness |

### Production realtime stack (Phase 1+)
- WebSocket lib: **`github.com/coder/websocket`** (minimal, modern, no transitive
  deps). The Phase 0 `ws_echo.go` hand-rolls the protocol to avoid a dependency
  for the throwaway spike — **do not** model production code on it; delete it
  once the edge is confirmed.
- New files: `ws.go` (handshake + read/write loops), `hub.go` (connection
  registry), `realtime.go` (Redis pub/sub bridge), `publish.go`
  (`/internal/publish` webhook + `X-Internal-Token`).
- New env: `REALTIME_REDIS_URL`, `WP_BASE_URL` (for token validation),
  `INTERNAL_PUBLISH_TOKEN`.

---

## Phase 0 — how to run the spike

1. Deploy this container with `WS_ECHO_ENABLED=1`.
2. Bunny dashboard: Pull Zone → General → **WebSockets** → ON.
3. Open `ws-echo-test.html` in a browser, point it at
   `wss://<your-endpoint>/ws/echo`, Connect, Send, run the latency test.
4. **Pass:** status `OPEN`, the "connected: ..." greeting, echoed messages, sane
   RTT. **Fail:** stuck `CONNECTING` then `CLOSED` → edge isn't passing the
   upgrade (recheck the toggle / endpoint type).

Local smoke test (no Bunny): `WS_ECHO_ENABLED=1 docker compose up --build`, then
point the test page at `ws://localhost:8080/ws/echo`.

Remove `ws_echo.go`, its route in `main.go`, and `ws-echo-test.html` once the
edge path is confirmed and Phase 1 begins.
