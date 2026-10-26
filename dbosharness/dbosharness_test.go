package dbosharness

import (
	"context"
	"testing"

	"github.com/xkamail/harness"
	"github.com/xkamail/harness/harnesstest"
)

// subOnly sends only sub-agent calls through the step model, so the parent needs no DBOS.
type subOnly struct {
	*harnesstest.FakeModel
	step harness.Model
}

func (s subOnly) Message(ctx context.Context, req harness.Request) (*harness.Response, error) {
	if harness.IsSubAgent(ctx) {
		return s.step.Message(ctx, req)
	}
	return s.FakeModel.Message(ctx, req)
}

func TestModelSubAgentSkipsStep(t *testing.T) {
	fake := harnesstest.NewFakeModel(
		harnesstest.Call("c1", "research", map[string]string{"task": "t"}),
		harnesstest.Text("child"),
		harnesstest.Text("done"),
	)
	var usage, calls int
	// a nil wctx fails any RunAsStep, so the child call must bypass it
	step := Model(nil, fake,
		WithOnUsage(func(context.Context, harness.Model, harness.Usage) { usage++ }),
		WithOnCalls(func([]harness.ToolCall) { calls++ }),
	)

	child, err := harness.Compile(harness.Agent{Name: "child", SystemPrompt: "CHILD", Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := harness.Compile(harness.Agent{Name: "parent", SystemPrompt: "PARENT", Tools: []harness.Tool{
		harness.SubAgent(harness.SubAgentConfig{Name: "research", Description: "d", Agent: child}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := parent.Run(t.Context(), subOnly{FakeModel: fake, step: step}, harness.RunConfig{
		History: []harness.Message{{Role: harness.RoleUser, Content: "go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reply != "done" {
		t.Fatalf("reply %q", res.Reply)
	}
	if usage != 1 || calls != 0 {
		t.Fatalf("usage %d calls %d, want 1 and 0", usage, calls)
	}
}
