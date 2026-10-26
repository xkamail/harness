package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xkamail/harness"
)

// routedModel answers parent and child runs from one goroutine-safe function,
// telling them apart by the system prompt.
type routedModel struct {
	mu       sync.Mutex
	subFlags map[string][]bool // "parent" or "child" -> IsSubAgent per call
	fn       func(req harness.Request, child bool) (*harness.Response, error)
}

func (r *routedModel) Message(ctx context.Context, req harness.Request) (*harness.Response, error) {
	child := strings.Contains(req.Messages[0].Content, "CHILD")
	key := "parent"
	if child {
		key = "child"
	}
	r.mu.Lock()
	if r.subFlags == nil {
		r.subFlags = make(map[string][]bool)
	}
	r.subFlags[key] = append(r.subFlags[key], harness.IsSubAgent(ctx))
	r.mu.Unlock()
	return r.fn(req, child)
}

func (r *routedModel) Provider() string { return "fake" }
func (r *routedModel) Model() string    { return "fake" }

func lastMessage(req harness.Request) harness.Message { return req.Messages[len(req.Messages)-1] }

func subCall(id, name, args string) harness.ToolCall {
	return harness.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

func textTurn(in, out int64, s string) *harness.Response {
	return &harness.Response{
		Message: harness.Message{Role: harness.RoleAssistant, Content: s},
		Usage:   harness.Usage{InputTokens: in, OutputTokens: out},
	}
}

func toolTurn(in, out int64, calls ...harness.ToolCall) *harness.Response {
	return &harness.Response{
		Message: harness.Message{Role: harness.RoleAssistant, ToolCalls: calls},
		Usage:   harness.Usage{InputTokens: in, OutputTokens: out},
	}
}

func fanOut(n int) *harness.Response {
	calls := make([]harness.ToolCall, n)
	for i := range calls {
		calls[i] = subCall(fmt.Sprintf("c%d", i), "research", fmt.Sprintf(`{"task":"t%d"}`, i))
	}
	return toolTurn(1, 1, calls...)
}

func echoOnly() harness.Tool {
	return harness.Tool{
		Name:    "echo",
		Schema:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
		Handler: func(_ context.Context, args json.RawMessage) (any, error) { return json.RawMessage(args), nil },
	}
}

func headlessAgent(t *testing.T, tools ...harness.Tool) *harness.Agent {
	t.Helper()
	a, err := harness.Compile(harness.Agent{Name: "child", SystemPrompt: "CHILD", Tools: tools, Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func parentAgent(t *testing.T, child *harness.Agent, extra ...harness.Tool) *harness.Agent {
	t.Helper()
	tools := append([]harness.Tool{harness.SubAgent(harness.SubAgentConfig{Name: "research", Description: "d", Agent: child})}, extra...)
	a, err := harness.Compile(harness.Agent{Name: "parent", SystemPrompt: "PARENT", Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func goCfg() harness.RunConfig {
	return harness.RunConfig{History: []harness.Message{user("go")}}
}

// inlineRunner hands fns a ctx that is not derived from Run's, like a durable runner.
func inlineRunner(_ context.Context, fns []func(context.Context) (string, error)) ([]string, error) {
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

func TestSubAgent(t *testing.T) {
	t.Run("fan-out returns each child reply as a tool result", func(t *testing.T) {
		m := &routedModel{fn: func(req harness.Request, child bool) (*harness.Response, error) {
			switch {
			case child:
				return textTurn(1, 1, "reply-"+lastMessage(req).Content), nil
			case lastMessage(req).Role == harness.RoleTool:
				return textTurn(1, 1, "final"), nil
			default:
				return fanOut(2), nil
			}
		}}
		res, err := parentAgent(t, headlessAgent(t)).Run(t.Context(), m, goCfg())
		if err != nil {
			t.Fatal(err)
		}
		if res.Reply != "final" {
			t.Fatalf("reply %q", res.Reply)
		}
		var tools []string
		for _, msg := range res.Messages {
			if msg.Role == harness.RoleTool {
				tools = append(tools, msg.Content)
			}
		}
		if !reflect.DeepEqual(tools, []string{`"reply-t0"`, `"reply-t1"`}) {
			t.Fatalf("tools %q", tools)
		}
		if !reflect.DeepEqual(m.subFlags["parent"], []bool{false, false}) || !reflect.DeepEqual(m.subFlags["child"], []bool{true, true}) {
			t.Fatalf("flags %v", m.subFlags)
		}
	})

	t.Run("child error is a tool error and the parent continues", func(t *testing.T) {
		m := &routedModel{fn: func(req harness.Request, child bool) (*harness.Response, error) {
			switch {
			case child:
				return nil, errors.New("boom")
			case lastMessage(req).Role == harness.RoleTool:
				return textTurn(1, 1, "recovered"), nil
			default:
				return fanOut(1), nil
			}
		}}
		res, err := parentAgent(t, headlessAgent(t)).Run(t.Context(), m, goCfg())
		if err != nil {
			t.Fatal(err)
		}
		if res.Reply != "recovered" || len(res.Messages) != 3 || !strings.Contains(res.Messages[1].Content, `"error"`) {
			t.Fatalf("got %+v", res)
		}
	})

	t.Run("sub-agents take no concurrency slot", func(t *testing.T) {
		// more sub-agents than slots, each child calling a tool: children
		// would starve if the sub-agent calls held the slots
		m := &routedModel{fn: func(req harness.Request, child bool) (*harness.Response, error) {
			switch {
			case child && lastMessage(req).Role == harness.RoleTool:
				return textTurn(1, 1, "ok"), nil
			case child:
				return toolTurn(1, 1, subCall("e", "echo", `{"q":"x"}`)), nil
			case lastMessage(req).Role == harness.RoleTool:
				return textTurn(1, 1, "final"), nil
			default:
				return fanOut(12), nil
			}
		}}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		res, err := parentAgent(t, headlessAgent(t, echoOnly())).Run(ctx, m, goCfg())
		if err != nil {
			t.Fatal(err)
		}
		if res.Reply != "final" {
			t.Fatalf("reply %q", res.Reply)
		}
	})

	t.Run("durable runner ctx", func(t *testing.T) {
		m := &routedModel{fn: func(req harness.Request, child bool) (*harness.Response, error) {
			switch {
			case child:
				return textTurn(1, 1, "child-reply"), nil
			case lastMessage(req).Role == harness.RoleTool:
				return textTurn(1, 1, "final"), nil
			default:
				return fanOut(1), nil
			}
		}}
		cfg := goCfg()
		cfg.Tools = inlineRunner
		res, err := parentAgent(t, headlessAgent(t)).Run(t.Context(), m, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if res.Reply != "final" || len(res.Messages) != 3 || res.Messages[1].Content != `"child-reply"` {
			t.Fatalf("got %+v", res.Messages)
		}
		if !reflect.DeepEqual(m.subFlags["child"], []bool{true}) {
			t.Fatalf("flags %v", m.subFlags)
		}
	})

	t.Run("sub-agent tool result carries the child's tool calls", func(t *testing.T) {
		m := &routedModel{fn: func(req harness.Request, child bool) (*harness.Response, error) {
			switch {
			case child && lastMessage(req).Role == harness.RoleTool:
				return textTurn(1, 1, "child-reply"), nil
			case child:
				return toolTurn(1, 1, subCall("ce", "echo", `{"q":"x"}`)), nil
			case lastMessage(req).Role == harness.RoleTool:
				return textTurn(1, 1, "final"), nil
			default:
				return toolTurn(1, 1,
					subCall("c0", "research", `{"task":"t0"}`),
					subCall("e0", "echo", `{"q":"y"}`),
				), nil
			}
		}}
		res, err := parentAgent(t, headlessAgent(t, echoOnly()), echoOnly()).Run(t.Context(), m, goCfg())
		if err != nil {
			t.Fatal(err)
		}
		if res.Reply != "final" || len(res.Messages) != 4 {
			t.Fatalf("got %+v", res.Messages)
		}
		sub := res.Messages[1]
		if sub.Role != harness.RoleTool || sub.ToolCallID != "c0" || sub.Content != `"child-reply"` {
			t.Fatalf("sub %+v", sub)
		}
		if want := []harness.ToolCall{subCall("ce", "echo", `{"q":"x"}`)}; !reflect.DeepEqual(sub.SubCalls, want) {
			t.Fatalf("subCalls %+v", sub.SubCalls)
		}
		plain := res.Messages[2]
		if plain.Role != harness.RoleTool || plain.ToolCallID != "e0" || len(plain.SubCalls) != 0 {
			t.Fatalf("plain %+v", plain)
		}
		if len(res.Messages[0].SubCalls) != 0 || len(res.Messages[3].SubCalls) != 0 {
			t.Fatalf("stray subCalls %+v", res.Messages)
		}
	})

	t.Run("child error keeps the tool calls it made", func(t *testing.T) {
		m := &routedModel{fn: func(req harness.Request, child bool) (*harness.Response, error) {
			switch {
			case child && lastMessage(req).Role == harness.RoleTool:
				return nil, errors.New("boom")
			case child:
				return toolTurn(1, 1, subCall("ce", "echo", `{"q":"x"}`)), nil
			case lastMessage(req).Role == harness.RoleTool:
				return textTurn(1, 1, "recovered"), nil
			default:
				return fanOut(1), nil
			}
		}}
		res, err := parentAgent(t, headlessAgent(t, echoOnly())).Run(t.Context(), m, goCfg())
		if err != nil {
			t.Fatal(err)
		}
		if res.Reply != "recovered" || len(res.Messages) != 3 {
			t.Fatalf("got %+v", res.Messages)
		}
		msg := res.Messages[1]
		if msg.ToolCallID != "c0" || !strings.Contains(msg.Content, `"error"`) {
			t.Fatalf("msg %+v", msg)
		}
		if want := []harness.ToolCall{subCall("ce", "echo", `{"q":"x"}`)}; !reflect.DeepEqual(msg.SubCalls, want) {
			t.Fatalf("subCalls %+v", msg.SubCalls)
		}
	})

	t.Run("durable runner ctx keeps the child's tool calls", func(t *testing.T) {
		m := &routedModel{fn: func(req harness.Request, child bool) (*harness.Response, error) {
			switch {
			case child && lastMessage(req).Role == harness.RoleTool:
				return textTurn(1, 1, "child-reply"), nil
			case child:
				return toolTurn(1, 1, subCall("ce", "echo", `{"q":"x"}`)), nil
			case lastMessage(req).Role == harness.RoleTool:
				return textTurn(1, 1, "final"), nil
			default:
				return fanOut(1), nil
			}
		}}
		cfg := goCfg()
		cfg.Tools = inlineRunner
		res, err := parentAgent(t, headlessAgent(t, echoOnly())).Run(t.Context(), m, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Messages) != 3 {
			t.Fatalf("got %+v", res.Messages)
		}
		if want := []harness.ToolCall{subCall("ce", "echo", `{"q":"x"}`)}; !reflect.DeepEqual(res.Messages[1].SubCalls, want) {
			t.Fatalf("subCalls %+v", res.Messages[1].SubCalls)
		}
	})

	t.Run("child usage is folded into the parent result", func(t *testing.T) {
		m := &routedModel{fn: func(req harness.Request, child bool) (*harness.Response, error) {
			switch {
			case child:
				return textTurn(4, 5, "child-reply"), nil
			case lastMessage(req).Role == harness.RoleTool:
				return textTurn(2, 3, "final"), nil
			default:
				return fanOut(1), nil
			}
		}}
		res, err := parentAgent(t, headlessAgent(t)).Run(t.Context(), m, goCfg())
		if err != nil {
			t.Fatal(err)
		}
		// parent: fan-out (1,1) + final (2,3); child: (4,5)
		if res.Usage.InputTokens != 1+2+4 || res.Usage.OutputTokens != 1+3+5 {
			t.Fatalf("usage %+v", res.Usage)
		}
	})

	t.Run("a replayed turn keeps the child's calls and usage", func(t *testing.T) {
		newModel := func() *routedModel {
			return &routedModel{fn: func(req harness.Request, child bool) (*harness.Response, error) {
				switch {
				case child && lastMessage(req).Role == harness.RoleTool:
					return textTurn(4, 5, "child-reply"), nil
				case child:
					return toolTurn(4, 5, subCall("ce", "echo", `{"q":"x"}`)), nil
				case lastMessage(req).Role == harness.RoleTool:
					return textTurn(2, 3, "final"), nil
				default:
					return fanOut(1), nil
				}
			}}
		}
		var stored []string
		record := func(ctx context.Context, fns []func(context.Context) (string, error)) ([]string, error) {
			outs, err := inlineRunner(ctx, fns)
			stored = outs
			return outs, err
		}
		replay := func(context.Context, []func(context.Context) (string, error)) ([]string, error) {
			return stored, nil
		}
		parent := parentAgent(t, headlessAgent(t, echoOnly()))

		cfg := goCfg()
		cfg.Tools = record
		first, err := parent.Run(t.Context(), newModel(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if len(stored) != 1 {
			t.Fatalf("stored %q", stored)
		}

		m := newModel()
		cfg = goCfg()
		cfg.Tools = replay
		second, err := parent.Run(t.Context(), m, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if len(second.Messages) != 3 {
			t.Fatalf("got %+v", second.Messages)
		}
		if want := []harness.ToolCall{subCall("ce", "echo", `{"q":"x"}`)}; !reflect.DeepEqual(second.Messages[1].SubCalls, want) {
			t.Fatalf("subCalls %+v", second.Messages[1].SubCalls)
		}
		// parent: fan-out (1,1) + final (2,3); child: two calls of (4,5)
		if second.Usage.InputTokens != 1+2+4+4 || second.Usage.OutputTokens != 1+3+5+5 {
			t.Fatalf("usage %+v", second.Usage)
		}
		if second.Usage != first.Usage {
			t.Fatalf("replayed usage %+v, first %+v", second.Usage, first.Usage)
		}
		if n := len(m.subFlags["child"]); n != 0 {
			t.Fatalf("child model called %d times in the replay", n)
		}
	})

	t.Run("a tool that calls Run itself does not share the parent's slots", func(t *testing.T) {
		inner, err := harness.Compile(harness.Agent{Name: "inner", SystemPrompt: "INNER", Tools: []harness.Tool{echoOnly()}})
		if err != nil {
			t.Fatal(err)
		}
		var (
			mu         sync.Mutex
			innerFlags []bool
		)
		innerModel := &routedModel{fn: func(req harness.Request, _ bool) (*harness.Response, error) {
			if lastMessage(req).Role == harness.RoleTool {
				return textTurn(1, 1, "inner-done"), nil
			}
			return toolTurn(1, 1, subCall("ie", "echo", `{"q":"x"}`)), nil
		}}
		nested := harness.Tool{
			Name:   "nested",
			Schema: json.RawMessage(`{"type":"object"}`),
			Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				res, err := inner.Run(ctx, innerModel, goCfg())
				if err != nil {
					return nil, err
				}
				mu.Lock()
				innerFlags = append(innerFlags, harness.IsSubAgent(ctx))
				mu.Unlock()
				return res.Reply, nil
			},
		}
		parent, err := harness.Compile(harness.Agent{Name: "parent", SystemPrompt: "PARENT", Tools: []harness.Tool{nested}})
		if err != nil {
			t.Fatal(err)
		}
		m := &routedModel{fn: func(req harness.Request, _ bool) (*harness.Response, error) {
			if lastMessage(req).Role == harness.RoleTool {
				return textTurn(1, 1, "final"), nil
			}
			calls := make([]harness.ToolCall, 12)
			for i := range calls {
				calls[i] = subCall(fmt.Sprintf("n%d", i), "nested", `{}`)
			}
			return toolTurn(1, 1, calls...), nil
		}}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		res, err := parent.Run(ctx, m, goCfg())
		if err != nil {
			t.Fatal(err)
		}
		if res.Reply != "final" {
			t.Fatalf("reply %q", res.Reply)
		}
		innerModel.mu.Lock()
		defer innerModel.mu.Unlock()
		if n := len(innerModel.subFlags["parent"]); n != 24 {
			t.Fatalf("inner model calls %d", n)
		}
		if slices.Contains(innerModel.subFlags["parent"], true) {
			t.Fatalf("inner run flagged as sub-agent: %v", innerModel.subFlags["parent"])
		}
		if len(innerFlags) != 12 || slices.Contains(innerFlags, true) {
			t.Fatalf("handler flags %v", innerFlags)
		}
	})
}

func TestCompileHeadless(t *testing.T) {
	gated := echoOnly()
	gated.Tier = harness.TierDisruptive
	sub := func(a *harness.Agent) harness.Tool {
		return harness.SubAgent(harness.SubAgentConfig{Name: "research", Agent: a})
	}

	t.Run("defs hold no ask_user", func(t *testing.T) {
		a := headlessAgent(t, echoOnly())
		if len(a.Defs()) != 1 {
			t.Fatalf("defs %+v", a.Defs())
		}
	})
	t.Run("rejects gated tool", func(t *testing.T) {
		if _, err := harness.Compile(harness.Agent{Name: "h", Tools: []harness.Tool{gated}, Headless: true}); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("rejects proposal schema", func(t *testing.T) {
		_, err := harness.Compile(harness.Agent{Name: "h", ProposalSchema: json.RawMessage(`{"type":"object"}`), Headless: true})
		if err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("rejects nesting", func(t *testing.T) {
		_, err := harness.Compile(harness.Agent{Name: "h", Tools: []harness.Tool{sub(headlessAgent(t))}, Headless: true})
		if err == nil || !strings.Contains(err.Error(), "cannot nest") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("sub-agent without an agent panics", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("want panic")
			}
		}()
		harness.SubAgent(harness.SubAgentConfig{Name: "research"})
	})
	t.Run("sub-agent needs a headless agent", func(t *testing.T) {
		plain, err := harness.Compile(harness.Agent{Name: "plain"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := harness.Compile(harness.Agent{Name: "p", Tools: []harness.Tool{sub(plain)}}); err == nil {
			t.Fatal("want error")
		}
	})
}
