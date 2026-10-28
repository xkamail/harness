// Package redisbus is a pgharness.Bus over Redis pub/sub, so Follow on one
// instance sees what another instance publishes.
package redisbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/xkamail/harness"
	"github.com/xkamail/harness/pgharness"
)

// healthCheck: a subscription silent this long is pinged; no reply by the next one closes it.
const healthCheck = 30 * time.Second

// Bus publishes each conversation's events as JSON on the channel prefix+conversationID.
type Bus struct {
	rdb    redis.UniversalClient
	prefix string
}

var _ pgharness.Bus = (*Bus)(nil)

func New(rdb redis.UniversalClient, prefix string) *Bus {
	return &Bus{rdb: rdb, prefix: prefix}
}

func (b *Bus) Publish(ctx context.Context, conversationID string, ev harness.Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("redisbus: publish: %w", err)
	}
	if err := b.rdb.Publish(ctx, b.prefix+conversationID, data).Err(); err != nil {
		return fmt.Errorf("redisbus: publish: %w", err)
	}
	return nil
}

// Subscribe holds one Redis connection per call. A slow subscriber drops
// events and its next one is marked Lagged; a Redis error or a dead
// connection closes the channel, which Follow reports as
// pgharness.ErrSubscriptionClosed.
func (b *Bus) Subscribe(ctx context.Context, conversationID string) (<-chan harness.Event, func(), error) {
	ps := b.rdb.Subscribe(ctx, b.prefix+conversationID)
	// wait for the subscribe confirmation so nothing published after return is missed
	if _, err := ps.Receive(ctx); err != nil {
		ps.Close()
		return nil, nil, fmt.Errorf("redisbus: subscribe: %w", err)
	}
	ch := make(chan harness.Event, 256)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(ch)
		defer ps.Close()
		// stop ends the loop by closing ps, not ctx
		ctx := context.WithoutCancel(ctx)
		lagged, pinged := false, false
		for {
			msg, err := ps.ReceiveTimeout(ctx, healthCheck)
			if err != nil {
				// a half-open connection never errors on its own
				if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() && !pinged {
					pinged = true
					if ps.Ping(ctx) == nil {
						continue
					}
				}
				return
			}
			pinged = false
			m, ok := msg.(*redis.Message)
			if !ok {
				continue
			}
			var ev harness.Event
			if err := json.Unmarshal([]byte(m.Payload), &ev); err != nil {
				lagged = true
				continue
			}
			ev.Lagged = lagged
			select {
			case ch <- ev:
				lagged = false
			default:
				lagged = true
			}
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			ps.Close()
			<-done
		})
	}
	return ch, stop, nil
}
