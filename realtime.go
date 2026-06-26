// Redis pub/sub bridge — the cross-pod bus. Each pod subscribes to all realtime
// channel namespaces and hands incoming events to its local hub; publishing an
// event PUBLISHes to a channel so every pod's subscribers receive it.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// channelPatterns are the pub/sub namespaces this service fans out. PSUBSCRIBE on
// an explicit set (rather than "*") so unrelated keyspace traffic is ignored.
var channelPatterns = []string{
	"thread:*",
	"user:*",
	"activity:*",
	"group:*",
	"forum:*",
	"presence",
}

type realtime struct {
	rdb *redis.Client
	hub *Hub
}

// run keeps the PSUBSCRIBE alive, delivering events to local clients by channel.
func (rt *realtime) run(ctx context.Context) {
	for ctx.Err() == nil {
		pubsub := rt.rdb.PSubscribe(ctx, channelPatterns...)
		log.Printf("realtime: subscribed to %v", channelPatterns)
		for msg := range pubsub.Channel() {
			rt.hub.deliver(msg.Channel, []byte(msg.Payload))
		}
		_ = pubsub.Close()
		if ctx.Err() == nil {
			log.Printf("realtime: pub/sub channel closed, resubscribing in 1s")
			time.Sleep(time.Second)
		}
	}
}

func (rt *realtime) publish(ctx context.Context, channel string, payload []byte) error {
	return rt.rdb.Publish(ctx, channel, payload).Err()
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
