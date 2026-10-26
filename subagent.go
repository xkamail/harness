package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

const (
	defaultSubAgentTimeout = 2 * time.Minute
	// Caps the task text shown as a call's summary.
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
	// Must be compiled with Headless set.
	Agent *Agent
	// Bounds the whole child run; zero means 2 minutes.
	Timeout time.Duration
}

// SubAgent returns a safe tool whose handler runs cfg.Agent as a nested run on the parent's
// model and returns its reply. Parallel calls share the parent's concurrency limit, and the
// child's tool calls go to the parent's OnToolStart and OnToolDone under RunID = the call's id.
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
			if rv == nil {
				return nil, errors.New("sub-agent called outside a run")
			}
			res, err := cfg.Agent.Run(withRunValues(ctx, &runValues{sem: rv.sem, sub: true}), rv.m, RunConfig{
				RunID:       rv.callID,
				History:     []Message{{Role: RoleUser, Content: in.Task}},
				OnToolStart: rv.onToolStart,
				OnToolDone:  rv.onToolDone,
			})
			rv.usage.add(res.Usage)
			if err != nil {
				return nil, err
			}
			return res.Reply, nil
		},
	}
}

// describeTask shows a call's task on one line, cut to maxTaskRunes.
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

// IsSubAgent reports whether ctx belongs to a sub-agent run, so a durable model adapter can
// skip checkpointing the child's calls: they already run inside the parent's tool step.
func IsSubAgent(ctx context.Context) bool {
	rv := runValuesFrom(ctx)
	return rv != nil && rv.sub
}

// runValues is what a run carries in its ctx for its sub-agents: its model, the concurrency
// limit shared with every run below it, and where their usage and tool calls go.
type runValues struct {
	m   Model
	sem chan struct{}
	sub bool

	onToolStart func(messageID string, call ToolCall, d ToolDisplay)
	onToolDone  func(call ToolCall, out ToolOutcome)
	// The sub-agent call this ctx was handed to; the child's RunID.
	callID string
	usage  *childUsage
}

// childUsage is the tokens a run's sub-agents spent, folded into its result after each turn.
type childUsage struct {
	mu sync.Mutex
	u  Usage
}

func (c *childUsage) add(u Usage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.u = c.u.Add(u)
}

func (c *childUsage) take() Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.u
	c.u = Usage{}
	return u
}

type runValuesKey struct{}

func runValuesFrom(ctx context.Context) *runValues {
	rv, _ := ctx.Value(runValuesKey{}).(*runValues)
	return rv
}

func withRunValues(ctx context.Context, rv *runValues) context.Context {
	return context.WithValue(ctx, runValuesKey{}, rv)
}
