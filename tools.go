package harness

import (
	"context"
	"encoding/json"
	"time"
)

// Tier is how much a tool call needs a person's approval before it runs.
type Tier int

const (
	TierSafe Tier = iota
	// Pauses for confirmation unless an approved plan covers the exact call or a grant covers the tool.
	TierDisruptive
	// Always pauses; only a Decision on the held call runs it, never a plan or grant.
	TierDestructive
)

func (t Tier) String() string {
	switch t {
	case TierDisruptive:
		return "disruptive"
	case TierDestructive:
		return "destructive"
	default:
		return "safe"
	}
}

// Handler's result is JSON-encoded for the model; an error goes back as a tool error, never ends the run.
type Handler func(ctx context.Context, args json.RawMessage) (any, error)

type Tool struct {
	Name string
	// Tells the model when to call the tool ("Call when …").
	Description string
	// JSON Schema validated before the handler runs; empty means any object.
	Schema  json.RawMessage
	Handler Handler

	Tier Tier
	// TierOf overrides Tier per call; it sees unvalidated arguments.
	TierOf func(args json.RawMessage) Tier
	// Check vets a gated call before it is held. Its error goes to the model; a non-nil
	// result is the confirmation preview.
	Check Handler
	// Gated tools one confirmation may cover for the rest of the task; empty means per call.
	GrantGroup string

	// Zero means 30s.
	Timeout time.Duration
	// Zero means 16 KiB.
	MaxResultBytes int
	// Shared by every caller; set it only on caller-independent read tools.
	CacheTTL time.Duration

	// EndsTurn: no further model call after this tool runs.
	EndsTurn bool
	// Block: the result is a UI card; the model gets a short acknowledgement instead.
	Block bool

	// Empty shows Name.
	Label string
	// App-defined UI grouping, opaque to the harness.
	Section string
	Hidden  bool
	// Summary for confirm cards and plan steps; sees unvalidated arguments. Nil or empty means Label.
	Describe func(args json.RawMessage) string
}

type ToolDisplay struct {
	Label   string `json:"label"`
	Section string `json:"section,omitempty"`
	Hidden  bool   `json:"hidden,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// Skill is a playbook loaded on demand with load_skill.
type Skill struct {
	Topic       string
	Description string
	Body        string
}
