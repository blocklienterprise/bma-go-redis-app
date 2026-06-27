// Presence — driven directly by live WebSocket connections. A connected socket
// means the user is online; the hub counts connections per user so presence
// flips on the first connect (0→1) and last disconnect (1→0). State is published
// to the "presence" channel and mirrored to short-TTL Redis keys (refreshed by
// the client heartbeat) so a snapshot survives a brief blip and a late
// subscriber can query who's online.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	presenceChannel = "presence"
	presenceTTL     = 35 * time.Second // a bit over the 25s client ping
)

func presenceKey(uid int) string { return fmt.Sprintf("presence:%d", uid) }
func lastSeenKey(uid int) string { return fmt.Sprintf("lastseen:%d", uid) }

// bgCtx is a short-lived context independent of any request, so cleanup on
// disconnect still runs after the connection's context is cancelled.
func bgCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*time.Second)
}

// presenceConnect marks uid online on its first connection and refreshes the
// TTL on subsequent ones.
func (s *server) presenceConnect(uid int) {
	ctx, cancel := bgCtx()
	defer cancel()
	first := s.hub.addConn(uid)
	_ = s.rt.rdb.Set(ctx, presenceKey(uid), "1", presenceTTL).Err()
	if first {
		s.publishPresence(ctx, uid, true, "")
	}
}

// presenceDisconnect marks uid offline only when its last connection closes.
func (s *server) presenceDisconnect(uid int) {
	ctx, cancel := bgCtx()
	defer cancel()
	if s.hub.removeConn(uid) {
		seen := time.Now().UTC().Format(time.RFC3339)
		_ = s.rt.rdb.Del(ctx, presenceKey(uid)).Err()
		_ = s.rt.rdb.Set(ctx, lastSeenKey(uid), seen, 0).Err()
		s.publishPresence(ctx, uid, false, seen)
	}
}

// presenceRefresh extends the TTL on heartbeat so a live user never expires.
func (s *server) presenceRefresh(uid int) {
	ctx, cancel := bgCtx()
	defer cancel()
	_ = s.rt.rdb.Expire(ctx, presenceKey(uid), presenceTTL).Err()
}

func (s *server) publishPresence(ctx context.Context, uid int, online bool, lastSeen string) {
	data, _ := json.Marshal(map[string]any{
		"user_id":   uid,
		"online":    online,
		"last_seen": lastSeen,
	})
	ev := mustJSON(event{
		Type:    "presence",
		Channel: presenceChannel,
		UserID:  uid,
		Data:    data,
		TS:      time.Now().Unix(),
	})
	_ = s.rt.publish(ctx, presenceChannel, ev)
}

// GET /presence?users=1,2,3 — token-gated snapshot of which of the given users
// are currently online (used by clients to seed presence before live updates).
func (s *server) handlePresence(w http.ResponseWriter, r *http.Request) {
	if _, err := validateBMAToken(bearerToken(r), s.tokenSecret, time.Now()); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	online := []int{}
	raw := r.URL.Query().Get("users")
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		uid, err := strconv.Atoi(part)
		if err != nil {
			continue
		}
		if n, _ := s.rt.rdb.Exists(r.Context(), presenceKey(uid)).Result(); n == 1 {
			online = append(online, uid)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string][]int{"online": online})
}
