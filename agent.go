package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Harness-owned interaction tools; a run ending on one waits for a person.
const (
	ToolPropose   = "propose"
	ToolAskUser   = "ask_user"
	ToolPlan      = "plan"
	ToolLoadSkill = "load_skill"
)

// Agent is a value; extend it with decorators (func(Agent) Agent), Compile once at startup, Run the result.
type Agent struct {
	// Unique within a project; stored on the conversation.
	Name string
	// Opens the system prompt; shared by a project's agents.
	PlatformPrompt string
	// Keep constant so the provider's prefix cache holds; live data goes in Context.
	SystemPrompt string
	// Injected after the system prompt every run; empty injects nothing.
	Context func(ctx context.Context) (string, error)

	Tools []Tool
	// Non-empty adds load_skill.
	Skills []Skill

	// Adds the propose tool when non-empty.
	ProposalSchema json.RawMessage
	// May return completed arguments; its error goes to the model verbatim.
	PrepareProposal func(ctx context.Context, args json.RawMessage) (json.RawMessage, error)

	// Runs only as a sub-agent: no ask_user, propose or plan, so no ProposalSchema and no
	// gated tools, and it cannot hold sub-agents.
	Headless bool

	// Empty keeps the provider default.
	ReasoningEffort string

	// Zero means 8.
	MaxIterations int
	// Zero means the caller's ctx decides.
	Timeout time.Duration
	// Nil never compacts.
	Compaction *CompactionConfig

	c *compiled
}

type compiled struct {
	defs        []ToolDef
	tools       map[string]*registered
	order       []string
	interaction map[string]*jsonschema.Schema
	cache       resultCache
}

const (
	defaultMaxIterations = 8

	askUserSchema = `{
	"type": "object",
	"properties": {
		"question": {"type": "string", "minLength": 1},
		"options": {"type": "array", "items": {"type": "string"}}
	},
	"required": ["question"],
	"additionalProperties": false
}`

	planSchema = `{
	"type": "object",
	"properties": {
		"title": {"type": "string"},
		"steps": {
			"type": "array",
			"minItems": 1,
			"items": {
				"type": "object",
				"properties": {
					"summary": {"type": "string", "minLength": 1},
					"tool": {"type": "string", "minLength": 1},
					"arguments": {"type": "object"}
				},
				"required": ["summary", "tool", "arguments"],
				"additionalProperties": false
			}
		}
	},
	"required": ["steps"],
	"additionalProperties": false
}`

	// Sent on the last call once a run is out of steps or budget.
	stepLimitPrompt = "You have reached this run's step limit and cannot call tools now. Reply in plain text: what you found, what you changed, and what is left. The user can send a message to continue."
)

// Compile validates a and compiles every schema.
func Compile(a Agent) (*Agent, error) {
	if a.Name == "" {
		return nil, errors.New("harness: agent name required")
	}
	if a.MaxIterations < 0 || a.Timeout < 0 {
		return nil, fmt.Errorf("harness: agent %q has a negative limit", a.Name)
	}
	gated := func(t Tool) bool { return t.Tier > TierSafe || t.TierOf != nil }
	if a.Headless && (len(a.ProposalSchema) > 0 || slices.ContainsFunc(a.Tools, gated)) {
		return nil, fmt.Errorf("harness: headless agent %q cannot propose or hold gated tools", a.Name)
	}
	c := &compiled{tools: make(map[string]*registered), interaction: make(map[string]*jsonschema.Schema)}
	tools := a.Tools
	for _, t := range tools {
		switch t.Name {
		case ToolPropose, ToolAskUser, ToolPlan, ToolLoadSkill:
			return nil, fmt.Errorf("harness: tool name %q is reserved", t.Name)
		}
		switch {
		case t.sub == nil:
		case a.Headless:
			return nil, fmt.Errorf("harness: sub-agents cannot nest (tool %q)", t.Name)
		case t.sub.c == nil:
			return nil, fmt.Errorf("harness: sub-agent %q is not compiled", t.Name)
		case !t.sub.Headless:
			return nil, fmt.Errorf("harness: sub-agent %q needs a headless agent", t.Name)
		}
	}
	if len(a.Skills) > 0 {
		st, err := skillTool(a.Skills)
		if err != nil {
			return nil, err
		}
		tools = append(slices.Clip(tools), st)
	}
	for _, t := range tools {
		if err := c.register(t); err != nil {
			return nil, err
		}
	}
	for _, name := range c.order {
		t := c.tools[name]
		c.defs = append(c.defs, ToolDef{Name: t.Name, Description: t.Description, Schema: t.Schema})
	}

	if len(a.ProposalSchema) > 0 {
		s, err := compileSchema(ToolPropose, a.ProposalSchema)
		if err != nil {
			return nil, err
		}
		c.interaction[ToolPropose] = s
		c.defs = append(c.defs, ToolDef{
			Name:        ToolPropose,
			Description: "Call when you have a complete, tool-verified proposal for the user to confirm. Ends your turn; do not call other tools afterwards.",
			Schema:      a.ProposalSchema,
		})
	}
	if slices.ContainsFunc(a.Tools, gated) {
		s, err := compileSchema(ToolPlan, json.RawMessage(planSchema))
		if err != nil {
			return nil, err
		}
		c.interaction[ToolPlan] = s
		c.defs = append(c.defs, ToolDef{
			Name:        ToolPlan,
			Description: "Call to propose a multi-step plan of exact tool calls (tool plus arguments) for the user to approve as a whole. Ends your turn. After they approve, call each step exactly as listed.",
			Schema:      json.RawMessage(planSchema),
		})
	}
	if !a.Headless {
		ask, err := compileSchema(ToolAskUser, json.RawMessage(askUserSchema))
		if err != nil {
			return nil, err
		}
		c.interaction[ToolAskUser] = ask
		c.defs = append(c.defs, ToolDef{
			Name:        ToolAskUser,
			Description: "Call when you need the user to choose or clarify something you cannot determine with tools. Ends your turn.",
			Schema:      json.RawMessage(askUserSchema),
		})
	}
	a.c = c
	return &a, nil
}

func (c *compiled) register(t Tool) error {
	switch {
	case t.Name == "":
		return errors.New("harness: tool name required")
	case c.tools[t.Name] != nil:
		return fmt.Errorf("harness: duplicate tool %q", t.Name)
	case t.Handler == nil:
		return fmt.Errorf("harness: tool %q has no handler", t.Name)
	case t.Timeout < 0 || t.MaxResultBytes < 0 || t.CacheTTL < 0:
		return fmt.Errorf("harness: tool %q has a negative limit", t.Name)
	case (t.Tier > TierSafe || t.TierOf != nil) && t.CacheTTL > 0:
		return fmt.Errorf("harness: tool %q is gated and must not be cached", t.Name)
	case t.GrantGroup != "" && (t.Tier != TierDisruptive || t.TierOf != nil):
		return fmt.Errorf("harness: tool %q has a grant group but is not disruptive", t.Name)
	}
	if len(t.Schema) == 0 {
		t.Schema = json.RawMessage(`{"type":"object"}`)
	}
	if t.Timeout == 0 {
		t.Timeout = defaultToolTimeout
	}
	if t.MaxResultBytes == 0 {
		t.MaxResultBytes = defaultMaxResultBytes
	}
	s, err := compileSchema(t.Name, t.Schema)
	if err != nil {
		return err
	}
	c.tools[t.Name] = &registered{Tool: t, schema: s}
	c.order = append(c.order, t.Name)
	return nil
}

func skillTool(skills []Skill) (Tool, error) {
	bodies := make(map[string]string, len(skills))
	topics := make([]string, 0, len(skills))
	var desc strings.Builder
	desc.WriteString("Call to load the playbook for a topic before working on it. Topics:")
	for _, s := range skills {
		if s.Topic == "" {
			return Tool{}, errors.New("harness: skill topic required")
		}
		if _, ok := bodies[s.Topic]; ok {
			return Tool{}, fmt.Errorf("harness: duplicate skill %q", s.Topic)
		}
		bodies[s.Topic] = s.Body
		topics = append(topics, s.Topic)
		fmt.Fprintf(&desc, "\n- %s: %s", s.Topic, s.Description)
	}
	enum, err := json.Marshal(topics)
	if err != nil {
		return Tool{}, err
	}
	return Tool{
		Name:        ToolLoadSkill,
		Description: desc.String(),
		Schema:      json.RawMessage(`{"type":"object","properties":{"topic":{"type":"string","enum":` + string(enum) + `}},"required":["topic"],"additionalProperties":false}`),
		Label:       "Loading skill",
		Handler: func(_ context.Context, args json.RawMessage) (any, error) {
			var in struct{ Topic string }
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, err
			}
			return bodies[in.Topic], nil
		},
		Describe: func(args json.RawMessage) string {
			var in struct{ Topic string }
			_ = json.Unmarshal(args, &in)
			return in.Topic
		},
	}, nil
}

func MustCompile(a Agent) *Agent {
	c, err := Compile(a)
	if err != nil {
		panic(err)
	}
	return c
}

// Run loops until a text reply, interaction tool, held call, EndsTurn tool or limit. On error
// the result is still non-nil with usage and messages so far. A held call gets no tool message.
// A model call cut off by ctx ends Messages with its streamed text, without tool calls.
func (a *Agent) Run(ctx context.Context, m Model, cfg RunConfig) (*RunResult, error) {
	if a == nil || a.c == nil {
		return &RunResult{}, ErrNotCompiled
	}
	start := time.Now()
	res := &RunResult{Provider: m.Provider(), Model: m.Model(), Messages: make([]Message, 0)}
	defer func() { res.Duration = time.Since(start) }()
	if a.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Timeout)
		defer cancel()
	}
	ctx = startRun(ctx, m)
	runID := cfg.RunID
	if runID == "" {
		runID = "run"
	}

	msgs := make([]Message, 0, len(cfg.History)+3)
	if sys := strings.TrimSpace(a.PlatformPrompt + "\n\n" + a.SystemPrompt); sys != "" {
		msgs = append(msgs, Message{Role: RoleSystem, Content: sys})
	}
	if a.Context != nil {
		live, err := a.Context(ctx)
		if err != nil {
			return res, fmt.Errorf("harness: agent context: %w", err)
		}
		if live != "" {
			msgs = append(msgs, Message{Role: RoleSystem, Content: live})
		}
	}
	msgs = append(msgs, cfg.History...)
	add := func(ms ...Message) {
		msgs = append(msgs, ms...)
		res.Messages = append(res.Messages, ms...)
	}

	granted := Grants{Tools: slices.Clone(cfg.Grants.Tools), Calls: slices.Clone(cfg.Grants.Calls)}
	if cfg.Decision != nil {
		t, msg, err := a.decide(ctx, msgs, cfg)
		if err != nil {
			return res, err
		}
		if msg != nil {
			add(*msg)
			res.Blocks = append(res.Blocks, t.blocks...)
			if t.endsTurn {
				res.Stop = StopEndsTurn
				return res, nil
			}
		}
	}

	aa := &afterAnswer{withdrawn: make(map[string]bool)}
	maxIter := a.MaxIterations
	if maxIter == 0 {
		maxIter = defaultMaxIterations
	}
	for res.Iterations < maxIter {
		if cfg.Inbox != nil && res.Iterations > 0 {
			in, revoke, err := cfg.Inbox(ctx)
			if err != nil {
				return res, fmt.Errorf("harness: inbox: %w", err)
			}
			add(in...)
			if revoke {
				granted = Grants{}
			}
		}
		id := MessageID(runID, res.Iterations+1)
		overBudget := cfg.Budget.spent(res.Usage)
		req := Request{Messages: msgs, Tools: a.c.defs, ReasoningEffort: a.ReasoningEffort}
		if len(aa.withdrawn) > 0 {
			req.Tools = slices.DeleteFunc(slices.Clone(a.c.defs), func(d ToolDef) bool { return aa.withdrawn[d.Name] })
		}
		// streamed text is kept if the call is interrupted
		var partial strings.Builder
		req.OnDelta = func(text string) {
			partial.WriteString(text)
			if cfg.OnDelta != nil {
				cfg.OnDelta(id, text)
			}
		}
		if cfg.OnReasoning != nil {
			req.OnReasoning = func(text string) { cfg.OnReasoning(id, text) }
		}
		req.OnReset = func() {
			partial.Reset()
			if cfg.OnReset != nil {
				cfg.OnReset(id)
			}
		}
		if overBudget || res.Iterations == maxIter-1 {
			// the last call ends the run with what was found instead of an error
			req.Messages = append(slices.Clip(msgs), Message{Role: RoleSystem, Content: stepLimitPrompt})
			req.NoToolCalls = true
		}
		resp, err := m.Message(ctx, req)
		if err != nil {
			if ctx.Err() != nil && strings.TrimSpace(partial.String()) != "" {
				add(Message{ID: id, Role: RoleAssistant, Content: partial.String()})
			}
			return res, fmt.Errorf("harness: model call: %w", err)
		}
		res.Iterations++
		res.Usage = res.Usage.Add(resp.Usage)
		if resp.Usage.InputTokens > 0 {
			res.ContextTokens = resp.Usage.InputTokens
		}

		msg := resp.Message
		msg.ID, msg.Role = id, RoleAssistant
		if len(msg.ToolCalls) == 0 {
			if strings.TrimSpace(msg.Content) == "" {
				return res, ErrNoFinalReply
			}
			res.Reply = msg.Content
			res.Stop = StopReply
			if overBudget {
				res.Stop = StopBudget
			}
			add(msg)
			return res, nil
		}
		if overBudget {
			return res, ErrBudgetExceeded
		}
		if req.NoToolCalls {
			return res, ErrMaxIterations
		}
		// Record the assistant turn only with its tool results, so Messages never holds unanswered calls but the held one.
		aa.replied = aa.replied || strings.TrimSpace(msg.Content) != ""
		t, err := a.runTurn(ctx, id, msg.ToolCalls, &granted, false, aa, cfg)
		if err != nil {
			return res, err
		}
		res.Usage = res.Usage.Add(t.usage)
		add(msg)
		for i, call := range msg.ToolCalls {
			// the decision answers a held call or a plan
			held := t.confirm != nil && call.ID == t.confirm.ID
			plan := t.interaction != nil && t.interaction.Name == ToolPlan && call.ID == t.interaction.ID
			if !held && !plan {
				add(Message{Role: RoleTool, Content: t.outs[i], ToolCallID: call.ID, SubCalls: t.subCalls[i]})
			}
		}
		res.Blocks = append(res.Blocks, t.blocks...)
		switch {
		case t.interaction != nil:
			res.Interaction, res.Stop = t.interaction, StopInteraction
			return res, nil
		case t.confirm != nil:
			res.Confirm, res.ConfirmPreview, res.Stop = t.confirm, t.preview, StopConfirm
			return res, nil
		case t.endsTurn:
			res.Stop = StopEndsTurn
			return res, nil
		}
	}
	return res, ErrMaxIterations
}

// decide returns a nil message when the latest assistant turn has no unanswered call with that id.
func (a *Agent) decide(ctx context.Context, msgs []Message, cfg RunConfig) (*turn, *Message, error) {
	d := cfg.Decision
	var (
		turnID string
		call   *ToolCall
	)
	for i := len(msgs) - 1; i >= 0 && call == nil; i-- {
		m := msgs[i]
		if m.Role == RoleTool && m.ToolCallID == d.ApprovalID {
			return nil, nil, nil // already answered
		}
		if m.Role != RoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}
		j := slices.IndexFunc(m.ToolCalls, func(c ToolCall) bool { return c.ID == d.ApprovalID })
		if j < 0 {
			return nil, nil, nil
		}
		turnID, call = m.ID, &m.ToolCalls[j]
	}
	if call == nil {
		return nil, nil, nil
	}
	// a plan only answers: its steps run as the model calls them, under Grants.Calls
	if d.Decision == DecisionReject || call.Name == ToolPlan {
		ok := d.Decision != DecisionReject
		if cfg.OnToolStart != nil {
			cfg.OnToolStart(turnID, *call, a.Display(*call))
		}
		if cfg.OnToolDone != nil {
			cfg.OnToolDone(*call, ToolOutcome{OK: ok})
		}
		out := map[string]string{"status": "rejected", "message": "The user rejected this call. Do not retry it unless they ask."}
		if ok {
			out = map[string]string{"status": "approved", "message": "The user approved the plan. Call each step exactly as listed."}
		}
		if d.Note != "" {
			out["note"] = d.Note
		}
		b, _ := json.Marshal(out)
		return &turn{}, &Message{Role: RoleTool, Content: string(b), ToolCallID: call.ID}, nil
	}
	t, err := a.runTurn(ctx, turnID, []ToolCall{*call}, nil, true, nil, cfg)
	if err != nil {
		return nil, nil, err
	}
	return t, &Message{Role: RoleTool, Content: t.outs[0], ToolCallID: call.ID}, nil
}

func (b Budget) spent(u Usage) bool {
	return b.MaxInputTokens > 0 && u.InputTokens >= b.MaxInputTokens ||
		b.MaxOutputTokens > 0 && u.OutputTokens >= b.MaxOutputTokens ||
		!b.Deadline.IsZero() && !time.Now().Before(b.Deadline)
}

// Defs includes interaction tools.
func (a *Agent) Defs() []ToolDef { return a.c.defs }

// Unknown tools are TierSafe.
func (a *Agent) Tier(call ToolCall) Tier {
	t, ok := a.c.tools[call.Name]
	switch {
	case !ok:
		return TierSafe
	case t.TierOf != nil:
		return t.TierOf(call.Arguments)
	default:
		return t.Tier
	}
}

// Interaction tools and unknown names are Hidden.
func (a *Agent) Display(call ToolCall) ToolDisplay {
	t, ok := a.c.tools[call.Name]
	if !ok {
		return ToolDisplay{Label: call.Name, Summary: call.Name, Hidden: true}
	}
	d := ToolDisplay{Label: t.Label, Section: t.Section, Hidden: t.Hidden}
	if d.Label == "" {
		d.Label = t.Name
	}
	d.Summary = d.Label
	if t.Describe != nil {
		if s := t.Describe(call.Arguments); s != "" {
			d.Summary = s
		}
	}
	return d
}

func (a *Agent) GrantGroup(name string) []string {
	t, ok := a.c.tools[name]
	if !ok || t.GrantGroup == "" {
		return nil
	}
	var out []string
	for _, n := range a.c.order {
		if a.c.tools[n].GrantGroup == t.GrantGroup {
			out = append(out, n)
		}
	}
	return out
}
