package harness

import (
	"context"
	"encoding/json"
	"time"
)

// Interaction tool names owned by the harness. A run that ends on one of
// them is waiting for a person.
const (
	ToolPropose   = "propose"
	ToolAskUser   = "ask_user"
	ToolPlan      = "plan"
	ToolLoadSkill = "load_skill"
	ToolRemember  = "remember"
	ToolForget    = "forget"
	ToolRecall    = "recall"
)

// Agent is one feature's assistant: prompt, tools and policy. Write it as a
// value, extend it with decorators (func(Agent) Agent), then Compile it once
// at startup and Run the compiled *Agent.
type Agent struct {
	// Name is unique within a project, such as "console" or
	// "tournament.helper"; it is stored on the conversation.
	Name string
	// PlatformPrompt opens the system prompt: what the product is and what
	// is out of scope. Shared by a project's agents.
	PlatformPrompt string
	// SystemPrompt is the agent's frozen instructions. Keep it constant so
	// the provider's prefix cache holds; live data goes in Context.
	SystemPrompt string
	// Context returns live context injected after the system prompt on every
	// run. Empty text injects nothing.
	Context func(ctx context.Context) (string, error)

	Tools []Tool
	// Skills adds load_skill when non-empty.
	Skills []Skill
	// Memory adds remember, forget and recall, and injects pinned and
	// scoped memories into every run. Nil means no memory.
	Memory MemoryStore

	// ProposalSchema adds the propose tool. Empty means nothing to propose.
	ProposalSchema json.RawMessage
	// PrepareProposal checks a schema-valid proposal and may return
	// completed arguments. Its error goes to the model verbatim.
	PrepareProposal func(ctx context.Context, args json.RawMessage) (json.RawMessage, error)

	// Scope is the scope kind conversations must be bound to, such as
	// "server" or "tournament"; empty means unbound.
	Scope  string
	Policy Policy
	// Approver reports whether p may confirm gated calls in c. Nil means
	// only the conversation's owner.
	Approver func(ctx context.Context, c Conversation, p Principal) bool

	// Models are the model ids this agent may run on, each one passed by its
	// evals; the first is the default.
	Models      []string
	ModelPolicy ModelPolicy
	// Route picks the model for a run from Models; nil means Models[0].
	// Under ModelPinned it is consulted only when the conversation starts.
	Route func(ctx context.Context, c Conversation, last Message) string
	// ReasoningEffort is sent on every model call; empty keeps the default.
	ReasoningEffort string

	// MaxIterations caps model calls per run; zero means 8.
	MaxIterations int
	// Timeout caps one run; zero means the caller's ctx decides.
	Timeout time.Duration
	// Compaction summarizes old history when the context grows; nil never
	// compacts.
	Compaction *CompactionConfig

	c *compiled
}

// compiled is what Compile derives once: validated schemas, the registry,
// the tool definitions sent to the model and the result cache.
type compiled struct {
	defs []ToolDef
}

// Compile validates a and compiles every schema. It fails on an empty name,
// a duplicate or reserved tool name, a tool without a Handler, a bad schema,
// or a Route without Models.
func Compile(a Agent) (*Agent, error) {
	// TODO: validate, compile schemas, add interaction and memory tools.
	a.c = &compiled{}
	return &a, nil
}

// MustCompile is Compile that panics, for a project's agent registry.
func MustCompile(a Agent) *Agent {
	c, err := Compile(a)
	if err != nil {
		panic(err)
	}
	return c
}

// Run drives the tool-calling loop until the model replies in text, calls an
// interaction tool, a gated call is held, a tool ends the turn, or a limit is
// reached. On error the result is still non-nil and carries the usage and
// messages accumulated so far, so spend is never lost.
func (a *Agent) Run(ctx context.Context, m Model, cfg RunConfig) (*RunResult, error) {
	if a == nil || a.c == nil {
		return &RunResult{}, ErrNotCompiled
	}
	// TODO: port the loop from mccloud pkg/ai/loop.go.
	return &RunResult{Provider: m.Provider(), Model: m.Model()}, ErrNotImplemented
}

// Defs returns the tool definitions sent to the model, interaction tools
// included.
func (a *Agent) Defs() []ToolDef { return a.c.defs }

// Tier returns call's tier; unknown tools are TierSafe.
func (a *Agent) Tier(call ToolCall) Tier {
	// TODO
	return TierSafe
}

// Display returns how call is shown to the user.
func (a *Agent) Display(call ToolCall) ToolDisplay {
	// TODO
	return ToolDisplay{Label: call.Name}
}

// GrantGroup returns the tools sharing name's grant group, or nil.
func (a *Agent) GrantGroup(name string) []string {
	// TODO
	return nil
}

// PickModel returns the model for the next run of c: c.Model under
// ModelPinned once set, else Route, else Models[0].
func (a *Agent) PickModel(ctx context.Context, c Conversation, last Message) string {
	if a.ModelPolicy == ModelPinned && c.Model != "" {
		return c.Model
	}
	if a.Route != nil {
		return a.Route(ctx, c, last)
	}
	if len(a.Models) > 0 {
		return a.Models[0]
	}
	return ""
}

// CanApprove reports whether p may confirm gated calls in c.
func (a *Agent) CanApprove(ctx context.Context, c Conversation, p Principal) bool {
	if a.Approver != nil {
		return a.Approver(ctx, c, p)
	}
	return p.ID != "" && p.ID == c.OwnerID
}
