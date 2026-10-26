package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	defaultSubAgentTimeout = 2 * time.Minute
	// maxTaskRunes caps the task text shown as a call's summary.
	maxTaskRunes = 80

	subAgentSchema = `{
	"type": "object",
	"properties": {
		"task": {"type": "string", "minLength": 1}
	},
	"required": ["task"],
	"additionalProperties": false
}`
)

// SubAgentConfig describes a tool that hands a task to a headless agent.
type SubAgentConfig struct {
	Name        string
	Description string
	Label       string
	// Agent must be compiled and Headless.
	Agent *Agent
	// Timeout bounds the whole child run; zero means 2 minutes.
	Timeout time.Duration
}

// SubAgent returns a safe tool whose handler runs cfg.Agent as a nested Run on the
// parent's model and returns the child's reply. Calls in one turn run in parallel
// and share the parent's concurrency limit. The parent's Budget does not bound the
// child; Timeout and the child's own limits do.
func SubAgent(cfg SubAgentConfig) Tool {
	if cfg.Agent == nil {
		panic("harness: SubAgent " + cfg.Name + " needs an Agent")
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = defaultSubAgentTimeout
	}
	return Tool{
		Name:        cfg.Name,
		Description: cfg.Description,
		Label:       cfg.Label,
		Schema:      json.RawMessage(subAgentSchema),
		Timeout:     timeout,
		Describe:    describeTask,
		sub:         cfg.Agent,
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var in struct {
				Task string `json:"task"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, err
			}
			rv := runValuesFrom(ctx)
			if rv == nil || rv.m == nil {
				return nil, errors.New("sub-agent called outside a run")
			}
			res, err := cfg.Agent.Run(context.WithValue(ctx, childSemKey{}, rv.sem), rv.m, RunConfig{
				History: []Message{{Role: RoleUser, Content: in.Task}},
			})
			// kept on error too: a timed-out child's finished turns show what it did
			if slot := subSlotFrom(ctx); slot != nil {
				slot.usage = res.Usage
				for _, m := range res.Messages {
					slot.calls = append(slot.calls, m.ToolCalls...)
				}
			}
			if err != nil {
				return nil, err
			}
			return res.Reply, nil
		},
	}
}

// IsSubAgent reports whether ctx belongs to a sub-agent run (its model calls and
// tools), so a model adapter can skip checkpointing the child's calls.
func IsSubAgent(ctx context.Context) bool {
	rv := runValuesFrom(ctx)
	return rv != nil && rv.sub
}

func describeTask(args json.RawMessage) string {
	var in struct {
		Task string `json:"task"`
	}
	if json.Unmarshal(args, &in) != nil {
		return ""
	}
	task := []rune(strings.Join(strings.Fields(in.Task), " "))
	if len(task) > maxTaskRunes {
		return strings.TrimSpace(string(task[:maxTaskRunes-1])) + "…"
	}
	return string(task)
}

// runValues is what a run carries in its ctx: its model, the concurrency limit
// shared with every sub-agent below it, and whether it is one.
type runValues struct {
	m   Model
	sem chan struct{}
	sub bool
}

type runValuesKey struct{}

// childSemKey carries the parent's semaphore to the Run a SubAgent starts, and only that one.
type childSemKey struct{}

// startRun gives a run its values. Only a SubAgent's child shares its parent's semaphore: a
// tool that calls Run itself holds a slot, so sharing could deadlock.
func startRun(ctx context.Context, m Model) context.Context {
	rv := &runValues{m: m, sem: make(chan struct{}, maxParallelCalls)}
	if sem, ok := ctx.Value(childSemKey{}).(chan struct{}); ok {
		rv.sem, rv.sub = sem, true
		ctx = context.WithValue(ctx, childSemKey{}, nil)
	}
	return withRunValues(ctx, rv)
}

func runValuesFrom(ctx context.Context) *runValues {
	rv, _ := ctx.Value(runValuesKey{}).(*runValues)
	return rv
}

func withRunValues(ctx context.Context, rv *runValues) context.Context {
	return context.WithValue(ctx, runValuesKey{}, rv)
}

// subResult is where one sub-agent call leaves what its child did.
type subResult struct {
	calls []ToolCall
	usage Usage
}

type subSlotKey struct{}

func subSlotFrom(ctx context.Context) *subResult {
	s, _ := ctx.Value(subSlotKey{}).(*subResult)
	return s
}

func withSubSlot(ctx context.Context, s *subResult) context.Context {
	return context.WithValue(ctx, subSlotKey{}, s)
}
