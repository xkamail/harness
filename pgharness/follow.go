package pgharness

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/xkamail/harness"
)

// Bus fans events out to every instance; delivery may be lossy and unordered, Follow repairs from the log.
type Bus interface {
	Publish(ctx context.Context, conversationID string, ev harness.Event) error
	// Subscribe must be receiving on return; stop closes the channel and is idempotent.
	Subscribe(ctx context.Context, conversationID string) (events <-chan harness.Event, stop func(), err error)
}

// ErrSubscriptionClosed: the bus closed the subscription; clients reconnect from their last seq.
var ErrSubscriptionClosed = errors.New("pgharness: bus subscription closed")

type FollowConfig struct {
	// PageSize is the catch-up page; default 200.
	PageSize int
	// Heal re-reads the log this often to repair a lost publish; default 15s.
	Heal time.Duration
	// OnTick runs every Heal tick after catch-up (e.g. an SSE ": ping").
	OnTick func() error
}

// Follow yields persisted events after afterSeq exactly once in seq order, then stream.ready, then ephemeral events.
// It subscribes before reading the log; a persisted or Lagged live event only triggers a catch-up from the log.
func Follow(ctx context.Context, bus Bus, conversationID string, afterSeq int64, cfg FollowConfig, yield func(harness.Event) error) error {
	if cfg.PageSize <= 0 {
		cfg.PageSize = 200
	}
	if cfg.Heal <= 0 {
		cfg.Heal = 15 * time.Second
	}
	events, stop, err := bus.Subscribe(ctx, conversationID)
	if err != nil {
		return err
	}
	defer stop()

	last := afterSeq
	catchUp := func() error {
		for {
			page, err := Events(ctx, conversationID, last, cfg.PageSize)
			if err != nil {
				return err
			}
			for _, ev := range page {
				if err := yield(ev); err != nil {
					return err
				}
				last = ev.Seq
			}
			if len(page) < cfg.PageSize {
				return nil
			}
		}
	}
	if err := catchUp(); err != nil {
		return err
	}
	if err := yield(harness.Event{Type: harness.EventStreamReady, Payload: []byte("{}"), CreatedAt: time.Now()}); err != nil {
		return err
	}

	heal := time.NewTicker(cfg.Heal)
	defer heal.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-heal.C:
			if err := catchUp(); err != nil {
				return err
			}
			if cfg.OnTick != nil {
				if err := cfg.OnTick(); err != nil {
					return err
				}
			}
		case ev, ok := <-events:
			if !ok {
				return ErrSubscriptionClosed
			}
			if ev.Lagged || ev.Seq > last {
				if err := catchUp(); err != nil {
					return err
				}
			}
			if ev.Seq > 0 {
				continue
			}
			ev.Lagged = false
			if err := yield(ev); err != nil {
				return err
			}
		}
	}
}

// MemoryBus is an in-process Bus; a slow subscriber drops events and its next one is marked Lagged.
type MemoryBus struct {
	mu   sync.Mutex
	subs map[string]map[*memorySub]struct{}
}

type memorySub struct {
	ch     chan harness.Event
	lagged bool
}

var _ Bus = (*MemoryBus)(nil)

func NewMemoryBus() *MemoryBus {
	return &MemoryBus{subs: map[string]map[*memorySub]struct{}{}}
}

func (b *MemoryBus) Publish(_ context.Context, conversationID string, ev harness.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs[conversationID] {
		e := ev
		e.Lagged = s.lagged
		select {
		case s.ch <- e:
			s.lagged = false
		default:
			s.lagged = true
		}
	}
	return nil
}

func (b *MemoryBus) Subscribe(_ context.Context, conversationID string) (<-chan harness.Event, func(), error) {
	s := &memorySub{ch: make(chan harness.Event, 256)}
	b.mu.Lock()
	if b.subs[conversationID] == nil {
		b.subs[conversationID] = map[*memorySub]struct{}{}
	}
	b.subs[conversationID][s] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			delete(b.subs[conversationID], s)
			if len(b.subs[conversationID]) == 0 {
				delete(b.subs, conversationID)
			}
			close(s.ch)
		})
	}
	return s.ch, stop, nil
}
