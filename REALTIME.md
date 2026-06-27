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
4. **Auth (revised after reading the plugin):** the app's Bearer is a
   self-contained HMAC-SHA256 token (`bma.<payload>.<sig>`, issued by
   `BMA_Token_Auth`). Go validates **identity locally** with the shared
   `bma_token_secret` (env `BMA_TOKEN_SECRET`) — no WP round-trip per connection,
   and sockets keep authenticating even if WordPress is down. **Authorization**
   (which threads a user may subscribe to) is not in the token, so Go calls
   BuddyBoss `GET /messages/{id}` with the user's own bearer once per
   thread-subscribe and caches the result (`member:{thread}:{uid}` EX 300).
   _Supersedes the earlier "Go calls WP to validate identity" choice, which was
   made before the token format was known._
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

### Connect & auth (local token validation)
1. Client opens `wss://rt.blockli.app/ws` with `Authorization: Bearer {token}`
   (RN `WebSocket` supports a headers option). Browsers can't set headers on a
   WebSocket, so `/ws?token=…` is also accepted for the test page.
2. Go verifies the HMAC signature + expiry with `BMA_TOKEN_SECRET` and extracts
   `uid` — entirely local, no WP call (`auth.go`).
3. Client sends `{type:"subscribe", thread_ids:[…]}`. Go authorizes each thread
   via BuddyBoss `GET /messages/{id}` (cached `member:{thread}:{uid}` EX 300),
   then subscribes the conn to `thread:{id}`. `REALTIME_SKIP_AUTHZ=1` bypasses
   the check for local testing.

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

| Phase | Scope | Status |
|---|---|---|
| **0 — Spike** | `/ws/echo` + browser test client | ✅ Bunny passes WS upgrade (confirmed on `wss://redis.blockli.app/ws/echo`). |
| **1 — Realtime core** | `REALTIME_REDIS_URL`, local token auth, hub, subscribe + authz, `/ws`, `/internal/publish`, pub/sub fan-out | ✅ **Built + tested** end-to-end. Go: unit + cross-impl + 2-client integration. WP: `BMA_Realtime` hooks `messages_message_after_save` → `/internal/publish`. RN: `realtime.js` client (9/9 logic tests) + `Messaging.js` screen wired into `App.js`. |
| **2 — Typing** | client debounce + Redis TTL + fan-out | ✅ **Built + tested.** Server `typing` event + `typing:{thread}:{user}` EX 6; RN `createTypingEmitter` debounce + "X is typing…" UI with self-healing TTL. |
| 3 | Presence / last-seen + read receipts | ⬜ |
| 4 | Reconnect/backoff, background→push handoff, REST catch-up | ⬜ |

### Production realtime stack (built)
- WebSocket lib: **`github.com/coder/websocket`** v1.8.15 (minimal, no transitive
  deps). The Phase 0 `ws_echo.go` hand-rolls the protocol for the throwaway spike
  only — **not** a model for production; delete it once you're satisfied.
- Files: `auth.go` (local HMAC token validation), `hub.go` (connection registry +
  fan-out), `realtime.go` (Redis pub/sub bridge + typing), `ws.go` (`/ws`
  handshake, read/write loops, subscribe authz), `publish.go` (`/internal/publish`
  webhook + `X-Internal-Token`).
- Env: `BMA_TOKEN_SECRET` (required — WP `bma_token_secret`),
  `REALTIME_REDIS_URL` (defaults to `REDIS_URL`), `INTERNAL_PUBLISH_TOKEN`,
  `WP_BASE_URL` (for subscribe authz), `REALTIME_SKIP_AUTHZ=1` (testing only).
- Tests: `auth_test.go` (`go test`), plus the integration flow documented below.

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

---

## Deployment / config checklist (Phase 1+)

Three components must agree on the same secrets and URLs:

**Go service (this container, env):**
- `BMA_TOKEN_SECRET` = WordPress option `bma_token_secret` (so Go validates the
  same tokens WP issues).
- `INTERNAL_PUBLISH_TOKEN` = a shared secret for `/internal/publish`.
- `WP_BASE_URL` = the site root (e.g. `https://blockli.dev`) — needed for
  subscribe authorization. Without it, all subscribes are denied (unless
  `REALTIME_SKIP_AUTHZ=1`, testing only).
- `REALTIME_REDIS_URL` = shared Redis when running >1 pod (defaults to the
  sidecar).

**WordPress (`blockli-mobile-app`, `BMA_Realtime`):**
- Option `bma_realtime_url` = the Go service base (e.g. `https://redis.blockli.app`).
- Option `bma_realtime_internal_token` = same value as `INTERNAL_PUBLISH_TOKEN`.
- (Both can be overridden by constants `BMA_REALTIME_URL` /
  `BMA_REALTIME_INTERNAL_TOKEN`.)

**Mobile config (`blockli-mobile/v1/config` → `endpoints.realtime`):**
- Auto-derived per tenant from the WP option `bma_realtime_url` via
  `BMA_Realtime::ws_url()` (https→wss + `/ws`). Set the URL once in the plugin
  settings; the config endpoint emits `endpoints.realtime` for that tenant.
- The app is a single multi-tenant binary, so this value is **never** hardcoded
  in the client — when `endpoints.realtime` is empty, the socket simply doesn't
  connect (REST messaging still works; live delivery/typing are off for that
  tenant until the URL is set).

RN client lives in the Expo app: `realtime.js` (connection + typing debounce) and
`Messaging.js` (modal screen), opened from the header "Messages" button.

### Notification realtime fixes (2026-06-27)

The bell/badge path is realtime-first:

1. WordPress creates a BuddyBoss notification.
2. `BMA_Realtime::on_notification_saved()` publishes `notification.new` to
   `user:{recipient_id}`.
3. The app's `NotificationBell` listens via `useNotifications()` and immediately
   increments `unreadCount` on that event.
4. A delayed REST refresh reconciles the list/count, but the visual increment must
   not depend on visiting the Notifications screen.

What was broken:

- The SDK `RealtimeProvider` ref-counted early subscriptions before the socket
  client existed. `NotificationBell` mounts early and subscribes to `user:{id}`;
  that channel could be stored in the provider but never replayed into the
  `RealtimeClient`. Activity still appeared to work because `activity:global`
  was subscribed later when the activity screen mounted.
- The SDK notification refresh counted unread items in the first fetched page
  only. If the user already had 25 unread notifications, a realtime bump could be
  reconciled back to 25.
- The WP Realtime Tester originally published a synthetic `notification.new`
  directly. That proves the Go publish endpoint accepts the event, but it does
  not create a BuddyBoss notification row, so a REST refresh can remove the
  apparent count bump.
- The tester's first real-notification implementation used a custom component
  that was only registered during admin requests. BuddyBoss REST/UI notification
  queries filter to registered components, so app REST requests could hide the
  tester row.
- The tester defaults the target user to the current WP admin user. If the app is
  signed in as another user (for example Jessica), the tester must target that
  user's WordPress ID or the app will not be subscribed to the matching
  `user:{id}` channel.

What was fixed:

- `blockli-sdk/packages/runtime/src/realtime/RealtimeProvider.tsx`
  - On client creation, replay every active provider channel ref into the new
    `RealtimeClient` before `connect()`.
  - Do not clear active `refCounts` during client teardown; mounted hooks keep
    their subscriptions across reconnect/client recreation.
- `blockli-sdk/packages/runtime/src/notifications/index.ts`
  - Keep the realtime path: `notification.new` still runs
    `setUnreadCount((n) => n + 1)` immediately.
  - Reconcile using `GET /notifications?per_page=1&is_new=true` and the
    `X-WP-Total` header so the true unread total is used without loading all
    notifications.
- `blockli-sdk/packages/runtime/src/providerProps.ts` and `types.ts`
  - Parse existing WordPress/BuddyBoss `X-WP-Total` response headers and expose
    them as `apiFetch(...).total`.
- `blockli-mobile-app/includes/class-bma-realtime-tester.php`
  - The notification test now creates a real unread BuddyBoss notification,
    which triggers the normal `bp_notification_after_save` → `BMA_Realtime`
    publish path.
  - The tester notification component is registered globally so BuddyBoss REST/UI
    lists include the test notification outside admin requests.
  - Result details include the target label/channel, e.g. `Jessica (#123)` and
    `user:123`, to catch wrong-user tests quickly.
- `blockli-mobile-app/includes/class-bma-plugin.php`
  - Loads `BMA_Realtime_Tester::hooks()` globally; the admin page remains
    admin-only, but notification formatting/component registration must be
    available to REST requests too.

Verification checklist for bell issues:

- Reload the app bundle after SDK changes.
- Confirm the app is signed in as the same WordPress user ID used in the tester.
- In Metro logs, after the socket opens, look for a subscribe ack containing
  `user:{id}`.
- In WP Admin → Blockli → Realtime Tester, run `Send notification.new` with that
  same user ID.
- Expected: bell increments immediately; reloading/opening Notifications still
  shows the new unread tester notification and preserves the count.
