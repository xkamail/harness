package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xkamail/harness"
	"github.com/xkamail/harness/harnesstest"
)

func testAgent(t *testing.T, extra ...harness.Tool) (*harness.Agent, *atomic.Int32) {
	t.Helper()
	var ran atomic.Int32
	tools := append([]harness.Tool{
		{
			Name:    "echo",
			Schema:  json.RawMessage(`{"type":"object","properties":{"s":{"type":"string"}},"required":["s"]}`),
			Handler: func(_ context.Context, args json.RawMessage) (any, error) { return json.RawMessage(args), nil },
		},
		{
			Name:       "restart",
			Tier:       harness.TierDisruptive,
			GrantGroup: "ops",
			Handler: func(context.Context, json.RawMessage) (any, error) {
				ran.Add(1)
				return "restarted", nil
			},
		},
		{
			Name: "wipe",
			Tier: harness.TierDestructive,
			Handler: func(context.Context, json.RawMessage) (any, error) {
				ran.Add(1)
				return "wiped", nil
			},
		},
	}, extra...)
	a, err := harness.Compile(harness.Agent{Name: "test", SystemPrompt: "sys", Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	return a, &ran
}

func user(s string) harness.Message { return harness.Message{Role: harness.RoleUser, Content: s} }

func TestRunReplyAfterTool(t *testing.T) {
	a, _ := testAgent(t)
	m := harnesstest.NewFakeModel(
		harnesstest.Call("c1", "echo", map[string]string{"s": "hi"}),
		harnesstest.Text("done"),
	)
	var started, finished []string
	res, err := a.Run(context.Background(), m, harness.RunConfig{
		RunID:       "r",
		History:     []harness.Message{user("go")},
		OnToolStart: func(id string, c harness.ToolCall, _ harness.ToolDisplay) { started = append(started, id+"/"+c.ID) },
		OnToolDone: func(c harness.ToolCall, o harness.ToolOutcome) {
			if o.OK {
				finished = append(finished, c.ID)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != harness.StopReply || res.Reply != "done" || res.Iterations != 2 {
		t.Fatalf("got %+v", res)
	}
	if len(res.Messages) != 3 || res.Messages[1].Content != `{"s":"hi"}` || res.Messages[2].ID != "r.2" {
		t.Fatalf("messages %+v", res.Messages)
	}
	if strings.Join(started, ",") != "r.1/c1" || strings.Join(finished, ",") != "c1" {
		t.Fatalf("events %v %v", started, finished)
	}
	if sys := m.Requests[0].Messages[0]; sys.Role != harness.RoleSystem || sys.Content != "sys" {
		t.Fatalf("system %+v", sys)
	}
}

func TestRunBadArgumentsGoBackToModel(t *testing.T) {
	a, _ := testAgent(t)
	m := harnesstest.NewFakeModel(
		harnesstest.Call("c1", "echo", map[string]int{"s": 1}),
		harnesstest.Text("ok"),
	)
	res, err := a.Run(context.Background(), m, harness.RunConfig{History: []harness.Message{user("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Messages[1].Content, "invalid arguments") {
		t.Fatalf("tool result %q", res.Messages[1].Content)
	}
}

func TestRunHoldsThenApproves(t *testing.T) {
	a, ran := testAgent(t)
	m := harnesstest.NewFakeModel(harnesstest.Call("c1", "restart", struct{}{}))
	res, err := a.Run(context.Background(), m, harness.RunConfig{History: []harness.Message{user("restart")}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != harness.StopConfirm || res.Confirm == nil || res.Confirm.ID != "c1" || ran.Load() != 0 {
		t.Fatalf("got %+v", res)
	}
	// the held call has no result yet
	if len(res.Messages) != 1 {
		t.Fatalf("messages %+v", res.Messages)
	}

	history := append([]harness.Message{user("restart")}, res.Messages...)
	m = harnesstest.NewFakeModel(harnesstest.Text("restarted it"))
	res, err = a.Run(context.Background(), m, harness.RunConfig{
		History:  history,
		Decision: &harness.DecisionPayload{ApprovalID: "c1", Decision: harness.DecisionApprove},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 || res.Reply != "restarted it" {
		t.Fatalf("got %+v ran=%d", res, ran.Load())
	}
	if tm := res.Messages[0]; tm.Role != harness.RoleTool || tm.ToolCallID != "c1" || tm.Content != `"restarted"` {
		t.Fatalf("result %+v", tm)
	}
}

func TestRunReject(t *testing.T) {
	a, ran := testAgent(t)
	history := []harness.Message{
		user("wipe"),
		{Role: harness.RoleAssistant, ToolCalls: []harness.ToolCall{{ID: "c1", Name: "wipe", Arguments: json.RawMessage(`{}`)}}},
	}
	m := harnesstest.NewFakeModel(harnesstest.Text("ok, not wiping"))
	res, err := a.Run(context.Background(), m, harness.RunConfig{
		History:  history,
		Decision: &harness.DecisionPayload{ApprovalID: "c1", Decision: harness.DecisionReject, Note: "keep it"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 0 || !strings.Contains(res.Messages[0].Content, "rejected") || !strings.Contains(res.Messages[0].Content, "keep it") {
		t.Fatalf("got %+v", res.Messages)
	}
}

func TestRunGrantsSkipDestructive(t *testing.T) {
	a, ran := testAgent(t)
	m := harnesstest.NewFakeModel(
		harnesstest.Call("c1", "restart", struct{}{}),
		harnesstest.Call("c2", "wipe", struct{}{}),
	)
	res, err := a.Run(context.Background(), m, harness.RunConfig{
		History: []harness.Message{user("go")},
		Grants:  harness.Grants{Tools: []string{"restart", "wipe"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 || res.Stop != harness.StopConfirm || res.Confirm.Name != "wipe" {
		t.Fatalf("got %+v ran=%d", res, ran.Load())
	}
}

func TestRunInboxRevokesGrants(t *testing.T) {
	a, ran := testAgent(t)
	m := harnesstest.NewFakeModel(
		harnesstest.Call("c1", "echo", map[string]string{"s": "x"}),
		harnesstest.Call("c2", "restart", struct{}{}),
	)
	res, err := a.Run(context.Background(), m, harness.RunConfig{
		History: []harness.Message{user("go")},
		Grants:  harness.Grants{Tools: []string{"restart"}},
		Inbox: func(context.Context) ([]harness.Message, bool, error) {
			return []harness.Message{user("actually wait")}, true, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 0 || res.Stop != harness.StopConfirm || res.Messages[2].Content != "actually wait" {
		t.Fatalf("got %+v ran=%d", res, ran.Load())
	}
}

func TestRunCheckPreview(t *testing.T) {
	a, _ := testAgent(t, harness.Tool{
		Name:    "deploy",
		Tier:    harness.TierDisruptive,
		Check:   func(context.Context, json.RawMessage) (any, error) { return map[string]int{"cost": 3}, nil },
		Handler: func(context.Context, json.RawMessage) (any, error) { return "ok", nil },
	})
	m := harnesstest.NewFakeModel(harnesstest.Call("c1", "deploy", struct{}{}))
	res, err := a.Run(context.Background(), m, harness.RunConfig{History: []harness.Message{user("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != harness.StopConfirm || string(res.ConfirmPreview) != `{"cost":3}` {
		t.Fatalf("got %+v", res)
	}
}

func TestRunInteraction(t *testing.T) {
	a, _ := testAgent(t)
	m := harnesstest.NewFakeModel(harnesstest.Call("c1", harness.ToolAskUser, map[string]string{"question": "which?"}))
	res, err := a.Run(context.Background(), m, harness.RunConfig{History: []harness.Message{user("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != harness.StopInteraction || res.Interaction.Name != harness.ToolAskUser || res.Messages[1].Content != `{"ok":true}` {
		t.Fatalf("got %+v", res)
	}
}

func TestRunBlockEndsTurn(t *testing.T) {
	a, _ := testAgent(t, harness.Tool{
		Name:     "offer_choices",
		Block:    true,
		EndsTurn: true,
		Handler:  func(context.Context, json.RawMessage) (any, error) { return []string{"a", "b"}, nil },
	})
	m := harnesstest.NewFakeModel(harnesstest.Call("c1", "offer_choices", struct{}{}))
	res, err := a.Run(context.Background(), m, harness.RunConfig{History: []harness.Message{user("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != harness.StopEndsTurn || len(res.Blocks) != 1 || string(res.Blocks[0].Data) != `["a","b"]` {
		t.Fatalf("got %+v", res)
	}
	if strings.Contains(res.Messages[1].Content, `"a"`) {
		t.Fatalf("the model saw the card: %q", res.Messages[1].Content)
	}
}

func TestRunStepLimit(t *testing.T) {
	a, err := harness.Compile(harness.Agent{
		Name:          "t",
		MaxIterations: 2,
		Tools:         []harness.Tool{{Name: "noop", Handler: func(context.Context, json.RawMessage) (any, error) { return nil, nil }}},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := harnesstest.NewFakeModel(harnesstest.Call("c1", "noop", struct{}{}), harnesstest.Text("summary"))
	res, err := a.Run(context.Background(), m, harness.RunConfig{History: []harness.Message{user("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if !m.Requests[1].NoToolCalls || res.Reply != "summary" {
		t.Fatalf("got %+v", res)
	}

	m = harnesstest.NewFakeModel(harnesstest.Call("c1", "noop", struct{}{}), harnesstest.Call("c2", "noop", struct{}{}))
	if _, err := a.Run(context.Background(), m, harness.RunConfig{}); !errors.Is(err, harness.ErrMaxIterations) {
		t.Fatalf("err %v", err)
	}
}

func TestRunBudget(t *testing.T) {
	a, _ := testAgent(t)
	call := harnesstest.Call("c1", "echo", map[string]string{"s": "x"})
	call.Usage.OutputTokens = 10
	m := harnesstest.NewFakeModel(call, harnesstest.Text("out of budget"))
	res, err := a.Run(context.Background(), m, harness.RunConfig{Budget: harness.Budget{MaxOutputTokens: 5}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != harness.StopBudget || !m.Requests[1].NoToolCalls || res.Usage.OutputTokens != 10 {
		t.Fatalf("got %+v", res)
	}
}

func TestRunEmptyTurn(t *testing.T) {
	a, _ := testAgent(t)
	m := harnesstest.NewFakeModel(harnesstest.Text(" "))
	if _, err := a.Run(context.Background(), m, harness.RunConfig{}); !errors.Is(err, harness.ErrNoFinalReply) {
		t.Fatalf("err %v", err)
	}
}

func TestRunNotCompiled(t *testing.T) {
	var a harness.Agent
	if _, err := a.Run(context.Background(), harnesstest.NewFakeModel(), harness.RunConfig{}); !errors.Is(err, harness.ErrNotCompiled) {
		t.Fatalf("err %v", err)
	}
}

func TestCompileErrors(t *testing.T) {
	h := func(context.Context, json.RawMessage) (any, error) { return nil, nil }
	for name, a := range map[string]harness.Agent{
		"no name":     {},
		"reserved":    {Name: "a", Tools: []harness.Tool{{Name: harness.ToolPlan, Handler: h}}},
		"duplicate":   {Name: "a", Tools: []harness.Tool{{Name: "x", Handler: h}, {Name: "x", Handler: h}}},
		"no handler":  {Name: "a", Tools: []harness.Tool{{Name: "x"}}},
		"bad schema":  {Name: "a", Tools: []harness.Tool{{Name: "x", Handler: h, Schema: json.RawMessage(`{"type":1}`)}}},
		"gated cache": {Name: "a", Tools: []harness.Tool{{Name: "x", Handler: h, Tier: harness.TierDisruptive, CacheTTL: 1}}},
	} {
		if _, err := harness.Compile(a); err == nil {
			t.Errorf("%s: compiled", name)
		}
	}
}

func TestDefsAndSkills(t *testing.T) {
	a := harness.MustCompile(harness.Agent{
		Name:   "a",
		Tools:  []harness.Tool{{Name: "x", Tier: harness.TierDisruptive, Handler: func(context.Context, json.RawMessage) (any, error) { return nil, nil }}},
		Skills: []harness.Skill{{Topic: "billing", Description: "invoices", Body: "do X"}},
	})
	var names []string
	for _, d := range a.Defs() {
		names = append(names, d.Name)
	}
	if got := strings.Join(names, ","); got != "x,load_skill,plan,ask_user" {
		t.Fatalf("defs %s", got)
	}
	m := harnesstest.NewFakeModel(harnesstest.Call("c1", harness.ToolLoadSkill, map[string]string{"topic": "billing"}), harnesstest.Text("ok"))
	res, err := a.Run(context.Background(), m, harness.RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Messages[1].Content != `"do X"` {
		t.Fatalf("skill %q", res.Messages[1].Content)
	}
}

func TestCallKey(t *testing.T) {
	k := func(args string) string {
		return harness.CallKey(harness.ToolCall{Name: "t", Arguments: json.RawMessage(args)})
	}
	if k(`{"a":1,"b":2}`) != k(`{ "b":2, "a":1 }`) {
		t.Fatal("key depends on key order")
	}
	if k(`{"a":1}`) == k(`{"a":2}`) || k("") != k("{}") {
		t.Fatal("bad key")
	}
}

func TestCompact(t *testing.T) {
	history := []harness.Message{
		user("u1"), {Role: harness.RoleAssistant, Content: "a1"},
		user("u2"), {Role: harness.RoleAssistant, Content: "a2"},
		user("u3"), {Role: harness.RoleAssistant, Content: "a3"},
	}
	m := harnesstest.NewFakeModel(harnesstest.Text("sum"))
	c, err := harness.Compact(context.Background(), m, history, harness.CompactionConfig{KeepTail: 2})
	if err != nil {
		t.Fatal(err)
	}
	if c.Summary != "sum" || c.Through != 1 {
		t.Fatalf("got %+v", c)
	}
	if !strings.Contains(m.Requests[0].Messages[1].Content, "User: u1") || strings.Contains(m.Requests[0].Messages[1].Content, "u2") {
		t.Fatalf("transcript %q", m.Requests[0].Messages[1].Content)
	}

	c, err = harness.Compact(context.Background(), m, history, harness.CompactionConfig{KeepTail: 3})
	if c != nil || err != nil {
		t.Fatalf("got %+v %v", c, err)
	}

	cut := harnesstest.Text("long")
	cut.Usage.OutputTokens = 50
	m = harnesstest.NewFakeModel(cut)
	if _, err := harness.Compact(context.Background(), m, history, harness.CompactionConfig{KeepTail: 1, MaxSummaryTokens: 50}); err == nil {
		t.Fatal("kept a cut-off summary")
	}
}

func TestRunApprovedPlan(t *testing.T) {
	a, ran := testAgent(t)
	plan := map[string]any{"steps": []map[string]any{
		{"summary": "restart a", "tool": "restart", "arguments": map[string]string{"svc": "a"}},
		{"summary": "wipe", "tool": "wipe", "arguments": map[string]string{}},
	}}
	res, err := a.Run(context.Background(), harnesstest.NewFakeModel(harnesstest.Call("p1", harness.ToolPlan, plan)), harness.RunConfig{History: []harness.Message{user("fix it")}})
	if err != nil || res.Interaction == nil || len(res.Messages) != 1 {
		t.Fatalf("plan run: %v %+v", err, res.Messages)
	}

	// approved: every step runs once at any tier; anything else is still held
	history := append([]harness.Message{user("fix it")}, res.Messages...)
	m := harnesstest.NewFakeModel(
		harnesstest.Call("c1", "restart", map[string]string{"svc": "a"}),
		harnesstest.Call("c2", "wipe", map[string]string{}),
		harnesstest.Call("c3", "restart", map[string]string{"svc": "a"}),
	)
	res, err = a.Run(context.Background(), m, harness.RunConfig{
		History:  history,
		Decision: &harness.DecisionPayload{ApprovalID: "p1", Decision: harness.DecisionApprove},
		Grants:   harness.Grants{Calls: harness.PlanCalls(*res.Interaction)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Messages[0].ToolCallID != "p1" || !strings.Contains(res.Messages[0].Content, "approved") {
		t.Fatalf("plan answer %+v", res.Messages[0])
	}
	if ran.Load() != 2 || res.Confirm == nil || res.Confirm.ID != "c3" {
		t.Fatalf("ran %d confirm %+v", ran.Load(), res.Confirm)
	}
}
