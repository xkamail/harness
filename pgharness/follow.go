package pgharness

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/xkamail/harness"
)

// Bus fans a conversation's events out to every instance. Delivery may be
// lossy and out of order; Follow repairs both from the log.
type Bus interface {
	Publish(ctx context.Context, conversationID string, ev harness.Event) error
	// Subscribe must be receiving when it returns, so a Publish right after
	// it is delivered. stop closes the channel and is safe to call twice.
	Subscribe(ctx context.Context, conversationID string) (events <-chan harness.Event, stop func(), err error)
}

// ErrSubscriptionClosed is returned by Follow when the bus closed its
// subscription; the client reconnects and resumes from its last seq.
var ErrSubscriptionClosed = errors.New("pgharness: bus subscription closed")

// FollowConfig tunes Follow; zero values use the defaults.
type FollowConfig struct {
	// PageSize is the catch-up page; zero means 200.
	PageSize int
	// Heal re-reads the log this often to repair a lost publish; zero means
	// 15s.
	Heal time.Duration
	// OnTick runs on every Heal tick after the catch-up, such as to send an
	// SSE ": ping".
	OnTick func() error
}

// Follow yields every persisted event after afterSeq exactly once and in seq
// order, then live events, until ctx ends or yield fails. It subscribes
// before reading the log, yields stream.ready once the backlog is sent,
// passes ephemeral events through as they come, and treats a persisted live
// event, or a Lagged one, only as a signal to catch up from the log.
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

// MemoryBus is an in-process Bus, for tests and single instances. A
// subscriber that falls behind loses events and gets the next one marked
// Lagged.
type MemoryBus struct {
	mu   sync.Mutex
	subs map[string]map[*memorySub]struct{}
}

type memorySub struct {
	ch     chan harness.Event
	lagged bool
}

var _ Bus = (*MemoryBus)(nil)

// NewMemoryBus returns an empty MemoryBus.
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
