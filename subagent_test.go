package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xkamail/harness"
)

// routedModel answers parent and child runs from one goroutine-safe func, telling them apart
// by the system prompt.
type routedModel struct {
	mu       sync.Mutex
	subFlags map[bool][]bool // child? -> IsSubAgent per call
	fn       func(req harness.Request, child bool) (*harness.Response, error)
}

func (r *routedModel) Message(ctx context.Context, req harness.Request) (*harness.Response, error) {
	child := strings.Contains(req.Messages[0].Content, "CHILD")
	r.mu.Lock()
	if r.subFlags == nil {
		r.subFlags = make(map[bool][]bool)
	}
	r.subFlags[child] = append(r.subFlags[child], harness.IsSubAgent(ctx))
	r.mu.Unlock()
	return r.fn(req, child)
}

func (r *routedModel) Provider() string { return "fake" }
func (r *routedModel) Model() string    { return "fake" }

func last(req harness.Request) harness.Message { return req.Messages[len(req.Messages)-1] }

func text(in, out int64, s string) *harness.Response {
	return &harness.Response{
		Message: harness.Message{Role: harness.RoleAssistant, Content: s},
		Usage:   harness.Usage{InputTokens: in, OutputTokens: out},
	}
}

func calls(cs ...harness.ToolCall) *harness.Response {
	return &harness.Response{
		Message: harness.Message{Role: harness.RoleAssistant, ToolCalls: cs},
		Usage:   harness.Usage{InputTokens: 1, OutputTokens: 1},
	}
}

func call(id, name, args string) harness.ToolCall {
	return harness.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

func fanOut(n int) *harness.Response {
	cs := make([]harness.ToolCall, n)
	for i := range cs {
		cs[i] = call(fmt.Sprintf("c%d", i), "research", fmt.Sprintf(`{"task":"t%d"}`, i))
	}
	return calls(cs...)
}

func echoTool() harness.Tool {
	return harness.Tool{
		Name:    "echo",
		Handler: func(_ context.Context, args json.RawMessage) (any, error) { return json.RawMessage(args), nil },
	}
}

func headless(t *testing.T, tools ...harness.Tool) *harness.Agent {
	t.Helper()
	a, err := harness.Compile(harness.Agent{Name: "child", SystemPrompt: "CHILD", Tools: tools, Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func parentOf(t *testing.T, child *harness.Agent, extra ...harness.Tool) *harness.Agent {
	t.Helper()
	tools := append([]harness.Tool{harness.SubAgent(harness.SubAgentConfig{Name: "research", Description: "d", Agent: child})}, extra...)
	a, err := harness.Compile(harness.Agent{Name: "parent", SystemPrompt: "PARENT", Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func toolContents(res *harness.RunResult) []string {
	var out []string
	for _, m := range res.Messages {
		if m.Role == harness.RoleTool {
			out = append(out, m.Content)
		}
	}
	return out
}

// sequential stands in for a durable runner, which hands fns a ctx not derived from Run's.
func sequential(_ context.Context, fns []func(context.Context) (string, error)) ([]string, error) {
	outs := make([]string, len(fns))
	for i, fn := range fns {
		out, err := fn(context.Background())
		if err != nil {
			return nil, err
		}
		outs[i] = out
	}
	return outs, nil
}

func TestSubAgentFanOut(t *testing.T) {
	m := &routedModel{fn: func(req harness.Request, child bool) (*harness.Response, error) {
		switch {
		case child:
			return text(1, 1, "reply-"+last(req).Content), nil
		case last(req).Role == harness.RoleTool:
			return text(1, 1, "final"), nil
		default:
			return fanOut(2), nil
		}
	}}
	var (
		mu     sync.Mutex
		labels []string
	)
	res, err := parentOf(t, headless(t)).Run(t.Context(), m, harness.RunConfig{
		History: []harness.Message{user("go")},
		OnToolStart: func(_ string, _ harness.ToolCall, d harness.ToolDisplay) {
			mu.Lock()
			defer mu.Unlock()
			labels = append(labels, d.Label)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reply != "final" {
		t.Fatalf("reply = %q", res.Reply)
	}
	if got := toolContents(res); !slices.Equal(got, []string{`"reply-t0"`, `"reply-t1"`}) {
		t.Fatalf("tool results = %v", got)
	}
	if !slices.Equal(m.subFlags[false], []bool{false, false}) || !slices.Equal(m.subFlags[true], []bool{true, true}) {
		t.Fatalf("IsSubAgent = %v", m.subFlags)
	}
	// parallel calls are told apart by their task
	if !slices.Equal(labels, []string{"t0", "t1"}) {
		t.Fatalf("labels = %v", labels)
	}
}

func TestSubAgentChildErrorIsToolError(t *testing.T) {
	m := &routedModel{fn: func(req harness.Request, child bool) (*harness.Response, error) {
		switch {
		case child && last(req).Role == harness.RoleTool:
			return nil, errors.New("boom")
		case child:
			return calls(call("e", "echo", `{}`)), nil
		case last(req).Role == harness.RoleTool:
			return text(1, 1, "recovered"), nil
		default:
			return fanOut(1), nil
		}
	}}
	res, err := parentOf(t, headless(t, echoTool())).Run(t.Context(), m, harness.RunConfig{History: []harness.Message{user("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reply != "recovered" || len(res.Messages) != 3 || !strings.Contains(res.Messages[1].Content, `"error"`) {
		t.Fatalf("res = %+v", res)
	}
	// parent: fanOut (1,1) + recovered (1,1); the failed child's first turn: (1,1)
	if want := (harness.Usage{InputTokens: 3, OutputTokens: 3}); res.Usage != want {
		t.Fatalf("usage = %+v, want %+v", res.Usage, want)
	}
}

func TestSubAgentsTakeNoSlot(t *testing.T) {
	// more sub-agents than slots, each child calling a tool: children would starve if the
	// sub-agent calls held the slots.
	m := &routedModel{fn: func(req harness.Request, child bool) (*harness.Response, error) {
		switch {
		case child && last(req).Role == harness.RoleTool:
			return text(1, 1, "ok"), nil
		case child:
			return calls(call("e", "echo", `{}`)), nil
		case last(req).Role == harness.RoleTool:
			return text(1, 1, "final"), nil
		default:
			return fanOut(12), nil
		}
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := parentOf(t, headless(t, echoTool())).Run(ctx, m, harness.RunConfig{History: []harness.Message{user("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reply != "final" {
		t.Fatalf("reply = %q", res.Reply)
	}
}

func TestSubAgentDurableRunnerCtx(t *testing.T) {
	m := &routedModel{fn: func(req harness.Request, child bool) (*harness.Response, error) {
		switch {
		case child:
			return text(1, 1, "child-reply"), nil
		case last(req).Role == harness.RoleTool:
			return text(1, 1, "final"), nil
		default:
			return fanOut(1), nil
		}
	}}
	res, err := parentOf(t, headless(t)).Run(t.Context(), m, harness.RunConfig{
		History: []harness.Message{user("go")},
		Tools:   sequential,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolContents(res); !slices.Equal(got, []string{`"child-reply"`}) {
		t.Fatalf("tool results = %v", got)
	}
	if !slices.Equal(m.subFlags[true], []bool{true}) {
		t.Fatalf("IsSubAgent = %v", m.subFlags)
	}
}

func TestSubAgentReportsChildCalls(t *testing.T) {
	for _, tc := range []struct {
		name     string
		childErr bool
		runner   harness.ToolRunner
	}{
		{name: "reply"},
		{name: "child error keeps its calls", childErr: true},
		{name: "durable runner ctx", runner: sequential},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &routedModel{fn: func(req harness.Request, child bool) (*harness.Response, error) {
				switch {
				case child && last(req).Role == harness.RoleTool && tc.childErr:
					return nil, errors.New("boom")
				case child && last(req).Role == harness.RoleTool:
					return text(1, 1, "child-reply"), nil
				case child:
					return calls(call("ce", "echo", `{"q":"x"}`)), nil
				case last(req).Role == harness.RoleTool:
					return text(1, 1, "final"), nil
				default:
					return calls(call("c0", "research", `{"task":"t0"}`), call("e0", "echo", `{"q":"y"}`)), nil
				}
			}}
			var (
				mu      sync.Mutex
				started []string
				done    []string
			)
			_, err := parentOf(t, headless(t, echoTool()), echoTool()).Run(t.Context(), m, harness.RunConfig{
				RunID:   "r",
				History: []harness.Message{user("go")},
				Tools:   tc.runner,
				OnToolStart: func(messageID string, c harness.ToolCall, _ harness.ToolDisplay) {
					mu.Lock()
					defer mu.Unlock()
					started = append(started, messageID+"/"+c.ID)
				},
				OnToolDone: func(c harness.ToolCall, _ harness.ToolOutcome) {
					mu.Lock()
					defer mu.Unlock()
					done = append(done, c.ID)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(started, "c0.1/ce") || !slices.Contains(done, "ce") {
				t.Fatalf("child call not reported: started %v, done %v", started, done)
			}
			if len(started) != 3 || len(done) != 3 {
				t.Fatalf("started %v, done %v", started, done)
			}
		})
	}
}

func TestSubAgentUsageFolded(t *testing.T) {
	m := &routedModel{fn: func(req harness.Request, child bool) (*harness.Response, error) {
		switch {
		case child:
			return text(4, 5, "child-reply"), nil
		case last(req).Role == harness.RoleTool:
			return text(2, 3, "final"), nil
		default:
			return fanOut(1), nil
		}
	}}
	res, err := parentOf(t, headless(t)).Run(t.Context(), m, harness.RunConfig{History: []harness.Message{user("go")}})
	if err != nil {
		t.Fatal(err)
	}
	// parent: fanOut (1,1) + final (2,3); child: (4,5)
	if want := (harness.Usage{InputTokens: 1 + 2 + 4, OutputTokens: 1 + 3 + 5}); res.Usage != want {
		t.Fatalf("usage = %+v, want %+v", res.Usage, want)
	}
}

func TestCompileHeadless(t *testing.T) {
	gated := echoTool()
	gated.Tier = harness.TierDisruptive
	sub := func(a *harness.Agent) harness.Tool {
		return harness.SubAgent(harness.SubAgentConfig{Name: "research", Agent: a})
	}

	a := headless(t, echoTool())
	if defs := a.Defs(); len(defs) != 1 || defs[0].Name != "echo" {
		t.Fatalf("defs = %v", defs)
	}
	plain := harness.MustCompile(harness.Agent{Name: "plain"})
	for name, agent := range map[string]harness.Agent{
		"gated tool":      {Name: "h", Headless: true, Tools: []harness.Tool{gated}},
		"proposal schema": {Name: "h", Headless: true, ProposalSchema: json.RawMessage(`{"type":"object"}`)},
		"nesting":         {Name: "h", Headless: true, Tools: []harness.Tool{sub(headless(t))}},
		"not headless":    {Name: "p", Tools: []harness.Tool{sub(plain)}},
		"not compiled":    {Name: "p", Tools: []harness.Tool{sub(&harness.Agent{Name: "raw", Headless: true})}},
	} {
		if _, err := harness.Compile(agent); err == nil {
			t.Errorf("%s: compiled", name)
		}
	}

	defer func() {
		if recover() == nil {
			t.Error("SubAgent without an Agent did not panic")
		}
	}()
	harness.SubAgent(harness.SubAgentConfig{Name: "research"})
}
