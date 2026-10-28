// Package harnesstest has a scripted model for tests.
package harnesstest

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/xkamail/harness"
)

// FakeModel replies with one scripted Response per call and records every Request.
type FakeModel struct {
	mu       sync.Mutex
	script   []harness.Response
	Requests []harness.Request
}

var _ harness.Model = (*FakeModel)(nil)

func NewFakeModel(script ...harness.Response) *FakeModel {
	return &FakeModel{script: script}
}

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

// Message fails with ctx's error if ctx is done once its text has streamed.
func (f *FakeModel) Message(ctx context.Context, req harness.Request) (*harness.Response, error) {
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &r, nil
}

func (f *FakeModel) Provider() string { return "fake" }
func (f *FakeModel) Model() string    { return "fake" }
