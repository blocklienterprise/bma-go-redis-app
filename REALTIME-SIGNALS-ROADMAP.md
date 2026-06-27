# Realtime Signals Roadmap

This document tracks where the Blockli app should wire realtime signals next.
The infrastructure is already in place: WordPress publishes typed events to the
Go realtime service, the Go service fans them out by channel over one WebSocket,
and the Expo app consumes them through `@blocklienterprise/runtime`.

Use this as a pick-list. Each item should be implemented and verified
independently.

---

## Already Solid

### Messaging

`thread:{id}` is wired through `useMessages()`:

- loads the thread over BuddyBoss REST
- subscribes to the thread channel
- live-merges `message.new`
- handles `typing`
- dedupes realtime echoes

Code:

- `blockli-sdk/packages/runtime/src/messaging/index.ts`
- `blockli-mobile-expo-tester/Messaging.js`
- `blockli-mobile-app/includes/class-bma-realtime.php`
- `bma-go-redis-app/ws.go`

### Bell Notifications

`user:{id}` increments the bell in realtime through `useNotifications()`, then
reconciles by REST total.

Code:

- `blockli-sdk/packages/runtime/src/notifications/index.ts`
- `blockli-sdk/packages/runtime/src/realtime/RealtimeProvider.tsx`
- `blockli-mobile-expo-tester/App.js` (`NotificationBell`)
- `blockli-mobile-app/includes/class-bma-realtime.php`

### Activity New-Post Signal

`activity:global` shows the "▲ N new posts" pill on new activity posts.

Code:

- `blockli-sdk/packages/runtime/src/activity/index.ts`
- `blockli-mobile-expo-tester/App.js` (`RuntimeActivityScreen`)
- `blockli-mobile-app/includes/class-bma-realtime.php`

---

## Highest-Value Next Wiring

### 1. Merge Activity Comments, Reactions, And Deletes Into Visible Feed Cards

Status: implemented in the mobile app bridge, SDK runtime store, and Expo detail
wrapper.

Backend already publishes:

- `activity.comment`
- `activity.reaction`
- `activity.deleted`

Fixed behavior:

- `activity.new` increments the new-post pill.
- `activity.comment` patches the visible card's `comment_count`.
- `activity.reaction` patches the visible card's `favorite_count`; if the
  acting user is the current viewer, it also patches `is_favorited`.
- `activity.deleted` removes the visible card from the feed.
- An open activity detail refreshes on matching comment/reaction events and
  closes on a matching delete event.

Implementation notes:

- WordPress now splits favorite/unfavorite hooks so `activity.reaction` includes
  `liked`, `is_favorited`, and `reaction_delta`.
- WordPress includes current BuddyBoss count fields when available:
  `comment_count`, `comments_count`, `favorite_count`, `favorites_count`, and
  `likes_count`.
- The SDK runtime store listens on `activity:global` after the activity feed has
  loaded and patches the shared `useActivity()` list in place.
- The Expo `RuntimeActivityDetail` wrapper listens to the same channel and calls
  the existing detail refresh callback for the currently open activity.

Verification:

- Open the activity feed on one device.
- From another user/session, comment, like/unlike, and delete.
- The visible card updates without pressing Sync.

Primary files:

- `blockli-sdk/packages/runtime/src/activity/index.ts`
- `blockli-sdk/packages/runtime/src/useActivity.tsx`
- `blockli-mobile-expo-tester/App.js`
- `blockli-mobile-app/includes/class-bma-realtime.php`

### 2. Finish Group And Topic Detail Migration To Runtime Detail

The live logic already exists:

- `group:{id}` refreshes on `member.joined`, `member.left`, `group.updated`
- `forum:{topicId}` refreshes on `reply.new`

Current app behavior:

- Screens using `RuntimeDetailScreen` benefit from the live channel.
- Legacy detail paths may not live-refresh yet.

Recommended implementation:

- Migrate remaining group and topic detail screens to `useDetail()`.
- Ensure open group details subscribe to `group:{id}`.
- Ensure open topic details subscribe to `forum:{topicId}`.
- Keep the current fallback behavior: on channel event, refresh detail data.
- Later improvement: merge member counts/replies in place instead of always
  refreshing.

Verification:

- Open a group detail.
- Join/leave/update the group from another session.
- Detail refreshes automatically.
- Open a topic detail.
- Add a forum reply from another session.
- Topic detail refreshes automatically.

Primary files:

- `blockli-mobile-expo-tester/App.js` (`RuntimeDetailScreen`)
- `blockli-sdk/packages/runtime/src/useDetail.ts`
- `blockli-sdk/packages/runtime/src/channel/index.ts`
- `blockli-mobile-app/includes/class-bma-realtime.php`

### 3. Expand Presence Beyond Messaging

`usePresence()` currently powers the conversation header online dot.

Recommended implementation:

- Add presence dots to member list rows.
- Add online/last-seen state to member profile/detail headers.
- Add presence to group member lists.
- Optionally add active-now status in search/member cards.

Backend edge to address:

- Presence can briefly flip offline then online during reconnects. Add a short
  disconnect grace period before publishing `online:false`.

Verification:

- Open member list/profile while another test user connects/disconnects.
- Online state updates live.
- Reconnect does not cause a visible offline/online flicker after grace period is
  added.

Primary files:

- `blockli-sdk/packages/runtime/src/presence/index.ts`
- `blockli-mobile-expo-tester/App.js`
- `bma-go-redis-app/presence.go`

### 4. Make Notifications Screen Consume The Same Realtime State As The Bell

Current app behavior:

- The bell count is live.
- The notifications list is mostly REST-refresh driven.

Recommended implementation options:

- Prepend the new notification immediately from `notification.new`, then
  reconcile via REST.
- Or show a "new notification" pill and refresh on tap.

Notes:

- `notification.new` payload currently carries IDs and metadata, not full
  rendered BuddyBoss notification text.
- If immediate prepend is desired, either include enough display data in the
  payload or render a lightweight placeholder until REST refresh completes.

Verification:

- Open Notifications screen.
- Trigger a notification for the same user.
- The list updates or shows a clear new-notification affordance without manual
  Sync.

Primary files:

- `blockli-sdk/packages/runtime/src/notifications/index.ts`
- `blockli-mobile-expo-tester/App.js`
- `blockli-mobile-app/includes/class-bma-realtime.php`

### 5. Make Thread List Update Live

Current app behavior:

- `useMessages(threadId)` is live for the open thread.
- The inbox/thread list is REST-driven.

Recommended implementation:

- Update thread list when a new message arrives:
  - move the thread to the top
  - update preview text/date
  - increase unread count for threads not currently open
- Consider publishing a user-scoped inbox event in addition to thread events:
  - `user:{recipient_id}` with `message.new`
  - or a future `inbox:{user_id}` channel

Verification:

- Open the message list, not the conversation.
- Send a message from another account.
- Thread moves to top and unread/preview updates live.

Primary files:

- `blockli-sdk/packages/runtime/src/messaging/index.ts`
- `blockli-mobile-expo-tester/Messaging.js`
- `blockli-mobile-app/includes/class-bma-realtime.php`

---

## Good Later Signals

### Livestream Events

Candidate events:

- `live.started`
- `live.ended`
- `live.viewer_count`

Candidate channels:

- `activity:global`
- `user:{id}`
- `live:{streamId}`

Recommended behavior:

- Show live badges immediately.
- Update stream cards without polling.
- Notify relevant users when a followed/member/group stream starts.

Primary files:

- `blockli-mobile-app/includes/class-bma-livestream.php`
- `blockli-mobile-expo-tester/App.js`
- `bma-go-redis-app/ws.go` if adding a new `live:{id}` namespace

### Group Activity Feeds

Recommended behavior:

- Publish activity inside a group to `group:{id}` as well as `activity:global`.
- Open group screens can refresh or patch without watching the whole global feed.

Primary files:

- `blockli-mobile-app/includes/class-bma-realtime.php`
- `blockli-mobile-expo-tester/App.js`
- `blockli-sdk/packages/runtime/src/channel/index.ts`

### Follow, Friend, And Member Events

Useful event candidates:

- `member.followed`
- `friend.request`
- `friend.accepted`
- `group.invite`

Recommended behavior:

- Keep notifications as the baseline signal.
- Add direct UI updates where useful, such as buttons, counts, and invite badges.

Primary files:

- BuddyBoss hook publishers in `blockli-mobile-app`
- `blockli-sdk/packages/runtime/src/notifications/index.ts`
- relevant member/group screens in `blockli-mobile-expo-tester/App.js`

---

## Contract And Documentation Updates

Keep the SDK/MCP-facing contracts aligned as signal wiring changes.

Recommended updates:

- `blockli-sdk/contracts/sdk-capabilities.v1.json`
  - `ApiFetchResult` includes optional `total`.
  - `useNotifications()` is realtime-first; REST is reconciliation.
  - `RealtimeProvider` replays active subscriptions into new clients/reconnects.
- `blockli-sdk/contracts/live-messaging.v1.json`
  - Feature hooks may subscribe before the socket exists; `RealtimeProvider`
    preserves/replays refs.
  - Troubleshooting: if activity works but bell does not, check `user:{id}`
    subscribe ack and target user ID.
  - `activity.comment`, `activity.reaction`, and `activity.deleted` are
    published, but visible in-place UI merging is a separate app wiring step.
- `blockli-mobile-expo-tester/REALTIME-ROADMAP.md`
  - Tighten "DONE" wording where publishers are done but UI merge is only
    partial.

No contract v2 is needed for additive fields such as `ApiFetchResult.total`.

---

## Suggested Pick Order

1. Merge visible activity comment/reaction/delete updates.
2. Finish runtime detail migration for group/topic detail screens.
3. Expand presence to members/profile/group member views.
4. Make notifications screen live, not only the bell.
5. Make thread list/inbox update live.
6. Add livestream-specific events.
7. Add group-feed scoped activity events.
8. Add follow/friend/member direct UI events.
