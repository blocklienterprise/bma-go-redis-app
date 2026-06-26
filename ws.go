// Realtime WebSocket endpoint (production path).
//
//	GET /ws   — authenticate the Bearer token locally (auth.go), then run a
//	            per-connection read/write loop. Clients send subscribe / typing /
//	            ping; the server fans message + typing events back via Redis.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// server holds the realtime dependencies shared by the WS and publish handlers.
type server struct {
	hub           *Hub
	rt            *realtime
	tokenSecret   string
	internalToken string
	wpBaseURL     string
	skipAuthz     bool
	httpClient    *http.Client
}

// event is the envelope shared across WS frames and Redis payloads.
type event struct {
	Type     string          `json:"type"`
	ThreadID int             `json:"thread_id,omitempty"`
	UserID   int             `json:"user_id,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	TS       int64           `json:"ts"`
}

func (s *server) handleWS(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	claims, err := validateBMAToken(token, s.tokenSecret, time.Now())
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Auth is the Bearer token, not the Origin. React Native clients send no
		// Origin header; the browser test page does. Don't gate on it.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	conn.SetReadLimit(1 << 20)

	c := &client{
		uid:   claims.UID,
		token: token,
		send:  make(chan []byte, 64),
		subs:  make(map[int]struct{}),
	}
	defer s.hub.remove(c)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go c.writeLoop(ctx, conn)

	c.enqueue(mustJSON(event{Type: "ready", UserID: c.uid, TS: time.Now().Unix()}))
	log.Printf("ws: uid=%d connected", c.uid)

	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			conn.Close(websocket.StatusNormalClosure, "")
			return
		}
		if typ == websocket.MessageText {
			s.handleClientMessage(ctx, c, data)
		}
	}
}

func (s *server) handleClientMessage(ctx context.Context, c *client, data []byte) {
	var in struct {
		Type      string `json:"type"`
		ThreadIDs []int  `json:"thread_ids"`
		ThreadID  int    `json:"thread_id"`
		Typing    bool   `json:"typing"`
	}
	if json.Unmarshal(data, &in) != nil {
		return
	}

	switch in.Type {
	case "subscribe":
		accepted := []int{}
		denied := []int{}
		for _, t := range in.ThreadIDs {
			if s.authorize(ctx, c, t) {
				s.hub.subscribe(c, t)
				accepted = append(accepted, t)
			} else {
				denied = append(denied, t)
			}
		}
		// Ack so the client can show which threads it's actually subscribed to
		// (a non-empty "denied" means thread authorization failed — usually a
		// missing/unreachable WP_BASE_URL on the server).
		ack, _ := json.Marshal(map[string][]int{"accepted": accepted, "denied": denied})
		c.enqueue(mustJSON(event{Type: "subscribed", Data: ack, TS: time.Now().Unix()}))
		log.Printf("ws: uid=%d subscribe accepted=%v denied=%v", c.uid, accepted, denied)
	case "typing":
		// Only participants of a subscribed thread may emit typing.
		if !c.subscribed(in.ThreadID) {
			return
		}
		_ = s.rt.setTyping(ctx, in.ThreadID, c.uid, in.Typing)
		payload, _ := json.Marshal(map[string]bool{"typing": in.Typing})
		ev := mustJSON(event{Type: "typing", ThreadID: in.ThreadID, UserID: c.uid, Data: payload, TS: time.Now().Unix()})
		_ = s.rt.publish(ctx, in.ThreadID, ev)
	case "ping":
		c.enqueue(mustJSON(event{Type: "pong", TS: time.Now().Unix()}))
	}
}

// authorize confirms the user participates in threadID. BuddyBoss only returns a
// thread to its participants, so a 200 from messages/{id} (with the user's own
// bearer) proves membership. Cached in Redis to avoid a WP call per subscribe.
func (s *server) authorize(ctx context.Context, c *client, threadID int) bool {
	if s.skipAuthz {
		return true
	}
	cacheKey := fmt.Sprintf("member:%d:%d", threadID, c.uid)
	if v, _ := s.rt.rdb.Get(ctx, cacheKey).Result(); v == "1" {
		return true
	}
	if s.wpBaseURL == "" {
		return false
	}
	url := fmt.Sprintf("%s/wp-json/buddyboss/v1/messages/%d", strings.TrimRight(s.wpBaseURL, "/"), threadID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	// Bypass ngrok's browser-warning interstitial when WP_BASE_URL is an ngrok
	// tunnel (dev). Harmless on real hosts.
	req.Header.Set("ngrok-skip-browser-warning", "true")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusOK {
		_ = s.rt.rdb.Set(ctx, cacheKey, "1", 5*time.Minute).Err()
		return true
	}
	return false
}

// writeLoop is the sole writer for a connection: it drains the send queue and
// emits periodic pings to keep the connection alive through any edge idle
// timeout.
func (c *client) writeLoop(ctx context.Context, conn *websocket.Conn) {
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-c.send:
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
