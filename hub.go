// Connection hub — tracks live WebSocket clients on THIS pod and fans Redis
// events out to the local clients subscribed to a thread. Cross-pod delivery is
// Redis pub/sub's job (realtime.go); the hub only ever touches local sockets.
package main

import "sync"

// client is one active WebSocket connection.
type client struct {
	uid   int
	token string // the user's bearer, kept for BuddyBoss authorization calls
	send  chan []byte
	mu    sync.Mutex
	subs  map[int]struct{} // thread ids this client is subscribed to
}

func (c *client) subscribed(threadID int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.subs[threadID]
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

// Hub indexes clients by thread for O(subscribers) fan-out.
type Hub struct {
	mu       sync.RWMutex
	byThread map[int]map[*client]struct{}
}

func newHub() *Hub {
	return &Hub{byThread: make(map[int]map[*client]struct{})}
}

func (h *Hub) subscribe(c *client, threadID int) {
	h.mu.Lock()
	if h.byThread[threadID] == nil {
		h.byThread[threadID] = make(map[*client]struct{})
	}
	h.byThread[threadID][c] = struct{}{}
	h.mu.Unlock()

	c.mu.Lock()
	c.subs[threadID] = struct{}{}
	c.mu.Unlock()
}

// remove drops a client from every thread index it belonged to.
func (h *Hub) remove(c *client) {
	c.mu.Lock()
	threads := make([]int, 0, len(c.subs))
	for t := range c.subs {
		threads = append(threads, t)
	}
	c.mu.Unlock()

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, t := range threads {
		if set := h.byThread[t]; set != nil {
			delete(set, c)
			if len(set) == 0 {
				delete(h.byThread, t)
			}
		}
	}
}

// deliver fans a payload out to every local client subscribed to threadID.
func (h *Hub) deliver(threadID int, payload []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.byThread[threadID] {
		c.enqueue(payload)
	}
}
