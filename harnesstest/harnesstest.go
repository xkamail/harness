// Package harnesstest has a scripted model for tests and conformance suites
// every Store, Bus and MemoryStore implementation must pass:
//
//	func TestStore(t *testing.T) { harnesstest.Store(t, func(t *testing.T) harness.Store { return newPGStore(t) }) }
//	func TestBus(t *testing.T)   { harnesstest.Bus(t, func(t *testing.T) harness.Bus { return newRedisBus(t) }) }
package harnesstest

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/xkamail/harness"
)

// FakeModel replies from a script, one Response per call, and records every
// Request.
type FakeModel struct {
	mu       sync.Mutex
	script   []harness.Response
	Requests []harness.Request
}

var _ harness.Model = (*FakeModel)(nil)

// NewFakeModel returns a model that replies with script in order.
func NewFakeModel(script ...harness.Response) *FakeModel {
	return &FakeModel{script: script}
}

// Text is a plain-text reply.
func Text(s string) harness.Response {
	return harness.Response{Message: harness.Message{Role: harness.RoleAssistant, Content: s}}
}

// Call is a reply with one tool call; args is marshaled to JSON.
func Call(id, name string, args any) harness.Response {
	b, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return harness.Response{Message: harness.Message{
		Role:      harness.RoleAssistant,
		ToolCalls: []harness.ToolCall{{ID: id, Name: name, Arguments: b}},
	}}
}

func (f *FakeModel) Message(_ context.Context, req harness.Request) (*harness.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, req)
	if len(f.script) == 0 {
		return nil, fmt.Errorf("harnesstest: script exhausted after %d calls", len(f.Requests))
	}
	r := f.script[0]
	f.script = f.script[1:]
	if req.OnDelta != nil && r.Message.Content != "" {
		req.OnDelta(r.Message.Content)
	}
	return &r, nil
}

func (f *FakeModel) Provider() string { return "fake" }
func (f *FakeModel) Model() string    { return "fake" }

// Store checks a Store: Seq is monotonic per conversation, After pages in
// order, conversations are isolated, ephemeral events are rejected.
func Store(t *testing.T, newStore func(t *testing.T) harness.Store) {
	t.Helper()
	// TODO
	t.Fatal("harnesstest.Store: not implemented")
}

// Bus checks a Bus: a Publish right after Subscribe returns is delivered,
// conversations are isolated, stop closes the channel and may be called
// twice, no goroutine outlives stop.
func Bus(t *testing.T, newBus func(t *testing.T) harness.Bus) {
	t.Helper()
	// TODO
	t.Fatal("harnesstest.Bus: not implemented")
}

// Follow checks harness.Follow against a lossy, reordering Bus: every
// persisted event is yielded once and in order.
func Follow(t *testing.T, newStore func(t *testing.T) harness.Store) {
	t.Helper()
	// TODO
	t.Fatal("harnesstest.Follow: not implemented")
}

// MemoryStore checks a MemoryStore: results never cross owners or leave the
// queried scopes, Save with an id updates, Forget deletes.
func MemoryStore(t *testing.T, newStore func(t *testing.T) harness.MemoryStore) {
	t.Helper()
	// TODO
	t.Fatal("harnesstest.MemoryStore: not implemented")
}
