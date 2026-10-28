package redisbus

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/acoshift/pgsql/pgctx"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
	"github.com/rs/xid"

	"github.com/xkamail/harness"
	"github.com/xkamail/harness/pgharness"
)

// setup returns a ctx on a fresh migrated database; TEST_DB_URL's %s is the database name.
func setup(t *testing.T) context.Context {
	t.Helper()
	tmpl := os.Getenv("TEST_DB_URL")
	if tmpl == "" || !strings.Contains(tmpl, "%s") {
		t.Fatalf("TEST_DB_URL must be set to a URL with a %%s database placeholder")
	}
	root, err := sql.Open("pgx", fmt.Sprintf(tmpl, "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	name := "test_redisbus_" + xid.New().String()
	if _, err := root.Exec(`create database ` + name); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", fmt.Sprintf(tmpl, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		root.Exec(`drop database ` + name + ` with (force)`)
		root.Close()
	})
	ctx := pgctx.NewContext(context.Background(), db)
	if err := pgharness.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return ctx
}

// newBus returns a Bus on its own Redis client, as a separate instance would have.
func newBus(t *testing.T, prefix string) *Bus {
	t.Helper()
	u := os.Getenv("TEST_REDIS_URL")
	if u == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	opt, err := redis.ParseURL(u)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { rdb.Close() })
	return New(rdb, prefix)
}

func TestFollowAcrossInstances(t *testing.T) {
	prefix := "test:redisbus:" + xid.New().String() + ":"
	a, b := newBus(t, prefix), newBus(t, prefix)
	ctx := setup(t)
	c, _, err := pgharness.Start(ctx, b, pgharness.StartParams{Agent: "test", OwnerID: "u1", Message: "hi"})
	if err != nil {
		t.Fatal(err)
	}

	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	got := make(chan harness.Event, 16)
	errc := make(chan error, 1)
	go func() {
		errc <- pgharness.Follow(fctx, a, c.ID, 0, pgharness.FollowConfig{Heal: time.Hour}, func(ev harness.Event) error {
			got <- ev
			return nil
		})
	}()
	next := func() harness.Event {
		t.Helper()
		select {
		case ev := <-got:
			return ev
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for an event")
			return harness.Event{}
		}
	}
	for next().Type != harness.EventStreamReady {
	}

	// ephemeral: only reachable through the bus
	pgharness.Live(ctx, b, c.ID, "r1", harness.EventDelta, "m1", "", harness.DeltaPayload{Text: "hi"})
	ev := next()
	if ev.Type != harness.EventDelta || ev.RunID != "r1" || ev.MessageID != "m1" || ev.Seq != 0 {
		t.Fatalf("delta = %+v", ev)
	}
	var d harness.DeltaPayload
	if err := json.Unmarshal(ev.Payload, &d); err != nil || d.Text != "hi" {
		t.Fatalf("delta payload = %s (%v)", ev.Payload, err)
	}

	// persisted: the publish on b triggers a catch-up from the log on a
	evs, err := pgharness.Append(ctx, b, c.ID, harness.Event{Type: "app.note", Payload: json.RawMessage(`{"n":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if ev := next(); ev.Type != "app.note" || ev.Seq != evs[0].Seq {
		t.Fatalf("persisted = %+v, want seq %d", ev, evs[0].Seq)
	}

	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Follow = %v", err)
	}
}

func TestSubscribeStop(t *testing.T) {
	ctx := context.Background()
	prefix := "test:redisbus:" + xid.New().String() + ":"
	a, b := newBus(t, prefix), newBus(t, prefix)
	events, stop, err := a.Subscribe(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(ctx, "c2", harness.Event{Type: harness.EventDelta}); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(ctx, "c1", harness.Event{Type: harness.EventToolStart}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-events:
		if ev.Type != harness.EventToolStart {
			t.Fatalf("got %s from another conversation", ev.Type)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
	stop()
	stop()
	if _, ok := <-events; ok {
		t.Fatal("channel open after stop")
	}
}

func TestSubscribeLagged(t *testing.T) {
	ctx := context.Background()
	prefix := "test:redisbus:" + xid.New().String() + ":"
	a, b := newBus(t, prefix), newBus(t, prefix)
	events, stop, err := a.Subscribe(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	for range 300 {
		if err := b.Publish(ctx, "c1", harness.Event{Type: harness.EventDelta}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond) // let the overflow drop before the marker
	if err := b.Publish(ctx, "c1", harness.Event{Type: harness.EventToolDone}); err != nil {
		t.Fatal(err)
	}
	for n := 0; ; n++ {
		select {
		case ev := <-events:
			if ev.Type != harness.EventToolDone {
				if ev.Lagged {
					t.Fatalf("event %d lagged before any drop", n)
				}
				continue
			}
			if n != 256 || !ev.Lagged {
				t.Fatalf("marker after %d events, lagged=%v; want 256, true", n, ev.Lagged)
			}
			return
		case <-time.After(5 * time.Second):
			t.Fatal("timed out")
		}
	}
}

func TestSubscribeClosedOnRedisError(t *testing.T) {
	a := newBus(t, "test:redisbus:"+xid.New().String()+":")
	events, stop, err := a.Subscribe(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	a.rdb.Close()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("got an event, want closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel still open after the client closed")
	}
}
