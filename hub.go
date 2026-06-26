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

// Hub indexes clients by channel for O(subscribers) fan-out.
type Hub struct {
	mu        sync.RWMutex
	byChannel map[string]map[*client]struct{}
}

func newHub() *Hub {
	return &Hub{byChannel: make(map[string]map[*client]struct{})}
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

// deliver fans a payload out to every local client subscribed to channel.
func (h *Hub) deliver(channel string, payload []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.byChannel[channel] {
		c.enqueue(payload)
	}
}
