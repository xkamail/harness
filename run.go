package harness

import (
	"context"
	"encoding/json"
	"time"
)

// RunConfig is one run's input beyond the model. Every field is optional.
type RunConfig struct {
	// Message ids derive from RunID, so a replayed durable run streams under the same ids. Empty uses "run".
	RunID   string
	History []Message
	Grants  Grants
	// Decision answers the call the previous run held or the plan it proposed (last in
	// History). The caller must check ErrStaleApproval and permissions first, add
	// GrantGroup to Grants.Tools on approve_all, and PlanCalls to Grants.Calls on a plan's approve.
	Decision *DecisionPayload
	// Nil runs calls concurrently in-process; dbosharness.Tools makes each a durable step.
	Tools ToolRunner
	// Checked before every model call after the first.
	Inbox       Inbox
	OnDelta     func(messageID, text string)
	OnReasoning func(messageID, text string)
	// OnReset: messageID's streamed text is void because the call is retried (step.reset).
	OnReset func(messageID string)
	// Called again for turns a durable run replays, so clients dedupe by ToolCallID.
	// OnToolDone may be called concurrently.
	OnToolStart func(messageID string, call ToolCall, d ToolDisplay)
	OnToolDone  func(call ToolCall, out ToolOutcome)
	Budget      Budget
}

// Budget bounds one run. Zero fields are unlimited.
type Budget struct {
	MaxInputTokens  int64
	MaxOutputTokens int64
	Deadline        time.Time
}

// ToolRunner returns outs[i] for fns[i]; a durable runner checkpoints each fn as a step.
type ToolRunner func(ctx context.Context, fns []func(context.Context) (string, error)) ([]string, error)

// Inbox returns messages since its last call; revoke drops the run's grants.
type Inbox func(ctx context.Context) (msgs []Message, revoke bool, err error)

// Grants come from approve_all decisions and approved plans since the last user instruction.
type Grants struct {
	// Callable with any arguments; never applies to TierDestructive tools.
	Tools []string
	// CallKeys of an approved plan's steps: each runs once, at any tier.
	Calls []string
}

type ToolOutcome struct {
	OK bool
	// Held: a gated call paused for confirmation; it did not run.
	Held     bool
	Duration time.Duration
}

type StopReason string

const (
	StopReply       StopReason = "reply"       // a plain-text turn
	StopInteraction StopReason = "interaction" // propose, ask_user or plan
	StopConfirm     StopReason = "confirm"     // a gated call is held
	StopEndsTurn    StopReason = "ends_turn"   // a tool with EndsTurn ran
	StopBudget      StopReason = "budget"      // the budget's last call replied
)

type RunResult struct {
	Provider string
	Model    string
	Usage    Usage
	// Prompt size of the last model call.
	ContextTokens int64
	Iterations    int
	Duration      time.Duration
	Stop          StopReason

	Reply       string
	Interaction *ToolCall
	Confirm     *ToolCall
	// ConfirmPreview is Confirm's Check result.
	ConfirmPreview json.RawMessage
	Blocks         []Block
	// Messages appended during this run; the caller persists them as events.
	// An interrupted run ends them with the cut-off call's streamed text.
	Messages []Message
}

type Block struct {
	ToolCallID string          `json:"toolCallId"`
	Tool       string          `json:"tool"`
	Data       json.RawMessage `json:"data"`
}
