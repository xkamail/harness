package harness

import (
	"context"
	"encoding/json"
	"time"
)

// Tier is how much a tool call needs a person's say-so before it runs.
type Tier int

const (
	// TierSafe tools run without asking.
	TierSafe Tier = iota
	// TierDisruptive tools pause the run for confirmation, or run when an
	// approved plan covers the exact call or a grant covers the tool.
	TierDisruptive
	// TierDestructive tools always pause for confirmation; no plan or grant
	// covers them, only a Decision on the held call.
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

// Handler runs one tool call. Its result is JSON-encoded for the model; an
// error goes back to the model as a tool error, never ends the run.
type Handler func(ctx context.Context, args json.RawMessage) (any, error)

// Tool is a capability exposed to the model.
type Tool struct {
	Name string
	// Description tells the model when to call the tool ("Call when …").
	Description string
	// Schema is the JSON Schema of the arguments; empty means any object.
	// Dispatch validates arguments against it before the handler runs.
	Schema  json.RawMessage
	Handler Handler

	// Tier gates the call; the zero value is TierSafe.
	Tier Tier
	// TierOf, when set, replaces Tier per call, for tools whose risk depends
	// on their arguments. It sees arguments not yet validated.
	TierOf func(args json.RawMessage) Tier
	// Check vets a gated call before it is held, so nobody is asked to
	// approve a call that would fail. Its error goes to the model; its
	// result, if non-nil, is the preview shown with the confirmation.
	Check Handler
	// GrantGroup names gated tools one confirmation may cover together for
	// the rest of the task. Empty means each call is approved on its own.
	GrantGroup string

	// Timeout bounds the handler's ctx; zero means 30s.
	Timeout time.Duration
	// MaxResultBytes caps the encoded result; zero means 16 KiB.
	MaxResultBytes int
	// CacheTTL caches successful results for identical arguments, shared by
	// every caller; set it only on caller-independent read tools.
	CacheTTL time.Duration

	// EndsTurn ends the turn after the call runs, with no further model call
	// (offer_choices waiting for a pick, suggest_followups closing).
	EndsTurn bool
	// Block marks a result that is a UI card for the user rather than data
	// for the model; the model gets a short acknowledgement instead.
	Block bool

	// Label is the short progress line shown while the call runs; empty
	// shows Name.
	Label string
	// Section is an app-defined UI grouping, opaque to the harness.
	Section string
	// Hidden keeps the call off progress lines.
	Hidden bool
	// Describe summarizes one call for confirm cards and plan steps. It sees
	// arguments not yet validated. Nil or empty means Label.
	Describe func(args json.RawMessage) string
}

// ToolDisplay is how a call is presented to the user.
type ToolDisplay struct {
	Label   string `json:"label"`
	Section string `json:"section,omitempty"`
	Hidden  bool   `json:"hidden,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// Skill is a playbook the agent loads on demand with load_skill, so the base
// prompt stays small.
type Skill struct {
	Topic       string
	Description string
	Body        string
}
