// Connection hub — tracks live WebSocket clients on THIS pod and fans Redis
// events out to the local clients subscribed to a channel. Cross-pod delivery is
// Redis pub/sub's job (realtime.go); the hub only ever touches local sockets.
//
// Channels are arbitrary strings (thread:{id}, user:{id}, activity:global,
// group:{id}, forum:{id}, presence) — see REALTIME-ROADMAP.md.
package main

import "sync"

// client is one active WebSocket connection.
type client struct {
	uid   int
	token string // the user's bearer, kept for BuddyBoss authorization calls
	send  chan []byte
	mu    sync.Mutex
	subs  map[string]struct{} // channels this client is subscribed to
}

func (c *client) subscribed(channel string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.subs[channel]
	return ok
}

// enqueue queues an outbound frame, dropping it if the client's buffer is full
// (a slow/stalled client must never block fan-out; it catches up via REST on
// reconnect).
func (c *client) enqueue(b []byte) {
	select {
	case c.send <- b:
	default:
	}
}

// Hub indexes clients by channel for O(subscribers) fan-out, and counts live
// connections per user for presence.
type Hub struct {
	mu        sync.RWMutex
	byChannel map[string]map[*client]struct{}
	byUser    map[int]int
}

func newHub() *Hub {
	return &Hub{
		byChannel: make(map[string]map[*client]struct{}),
		byUser:    make(map[int]int),
	}
}

// addConn records a new connection for uid and reports whether it's the user's
// first (0→1 — they just came online).
func (h *Hub) addConn(uid int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.byUser[uid]++
	return h.byUser[uid] == 1
}

// removeConn drops a connection for uid and reports whether it was the last
// (1→0 — they just went offline).
func (h *Hub) removeConn(uid int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byUser[uid] > 0 {
		h.byUser[uid]--
	}
	if h.byUser[uid] == 0 {
		delete(h.byUser, uid)
		return true
	}
	return false
}

// online reports whether the user currently has at least one live connection.
func (h *Hub) online(uid int) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.byUser[uid] > 0
}

func (h *Hub) subscribe(c *client, channel string) {
	h.mu.Lock()
	if h.byChannel[channel] == nil {
		h.byChannel[channel] = make(map[*client]struct{})
	}
	h.byChannel[channel][c] = struct{}{}
	h.mu.Unlock()

	c.mu.Lock()
	c.subs[channel] = struct{}{}
	c.mu.Unlock()
}

// remove drops a client from every channel index it belonged to.
func (h *Hub) remove(c *client) {
	c.mu.Lock()
	channels := make([]string, 0, len(c.subs))
	for ch := range c.subs {
		channels = append(channels, ch)
	}
	c.mu.Unlock()

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range channels {
		if set := h.byChannel[ch]; set != nil {
			delete(set, c)
			if len(set) == 0 {
				delete(h.byChannel, ch)
			}
		}
	}
}

// unsubscribe drops a client from a single channel.
func (h *Hub) unsubscribe(c *client, channel string) {
	h.mu.Lock()
	if set := h.byChannel[channel]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(h.byChannel, channel)
		}
	}
	h.mu.Unlock()

	c.mu.Lock()
	delete(c.subs, channel)
	c.mu.Unlock()
}

// deliver fans a payload out to every local client subscribed to channel.
func (h *Hub) deliver(channel string, payload []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.byChannel[channel] {
		c.enqueue(payload)
	}
}
