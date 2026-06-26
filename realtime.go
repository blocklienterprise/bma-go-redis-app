// Redis pub/sub bridge — the cross-pod bus. Each pod subscribes to all thread
// channels and hands incoming events to its local hub; publishing a message or
// typing event PUBLISHes to the thread's channel so every pod's subscribers
// receive it.
package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type realtime struct {
	rdb *redis.Client
	hub *Hub
}

// run keeps a PSUBSCRIBE on thread:* alive, delivering events to local clients.
// PSUBSCRIBE thread:* is simple and correct at our scale (every pod sees every
// thread's traffic). If a pod ever carries enough threads that this is wasteful,
// switch to dynamic per-thread SUBSCRIBE keyed on the hub's active threads.
func (rt *realtime) run(ctx context.Context) {
	for ctx.Err() == nil {
		pubsub := rt.rdb.PSubscribe(ctx, "thread:*")
		log.Printf("realtime: subscribed to thread:*")
		for msg := range pubsub.Channel() {
			id, err := threadIDFromChannel(msg.Channel)
			if err != nil {
				continue
			}
			rt.hub.deliver(id, []byte(msg.Payload))
		}
		_ = pubsub.Close()
		if ctx.Err() == nil {
			log.Printf("realtime: pub/sub channel closed, resubscribing in 1s")
			time.Sleep(time.Second)
		}
	}
}

func (rt *realtime) publish(ctx context.Context, threadID int, payload []byte) error {
	return rt.rdb.Publish(ctx, fmt.Sprintf("thread:%d", threadID), payload).Err()
}

// setTyping records ephemeral typing state with a short TTL so a lost "stop"
// self-heals. The live signal is the published event; this key lets a late
// subscriber recover current state.
func (rt *realtime) setTyping(ctx context.Context, threadID, uid int, typing bool) error {
	key := fmt.Sprintf("typing:%d:%d", threadID, uid)
	if !typing {
		return rt.rdb.Del(ctx, key).Err()
	}
	return rt.rdb.Set(ctx, key, "1", 6*time.Second).Err()
}

func threadIDFromChannel(channel string) (int, error) {
	_, rest, ok := strings.Cut(channel, ":")
	if !ok {
		return 0, fmt.Errorf("malformed channel %q", channel)
	}
	return strconv.Atoi(rest)
}
