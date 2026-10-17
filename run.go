package harness

import (
	"context"
	"encoding/json"
	"time"
)

// RunConfig is one run's input beyond the model. Every field is optional.
type RunConfig struct {
	// RunID names the run; message ids derive from it (MessageID), so a
	// replayed durable run streams under the same ids. Empty uses "run".
	RunID string
	// History is the conversation so far, oldest first, rebuilt from the
	// event log.
	History []Message
	// Grants are the gated tools the run may call without pausing.
	Grants Grants
	// Decision answers the call the previous run held (StopConfirm); Run
	// applies it to that call, the last one in History, before the first
	// model call. Approve and approve_all run it once whatever its tier;
	// reject returns it to the model as rejected, with the Note. The caller
	// checks who may approve and ErrStaleApproval first, and on approve_all adds
	// the call's GrantGroup to Grants.Tools.
	Decision *DecisionPayload
	// Tools runs each turn's tool calls; nil runs them concurrently
	// in-process. dbosharness.Tools makes each call a durable step.
	Tools ToolRunner
	// Inbox is checked before every model call after the first, so messages
	// sent mid-run steer it.
	Inbox Inbox
	// OnDelta and OnReasoning hear streamed text, under the MessageID of
	// the model call producing it.
	OnDelta     func(messageID, text string)
	OnReasoning func(messageID, text string)
	// OnReset hears that messageID's streamed text is void because the
	// call is being retried; emit it as step.reset.
	OnReset func(messageID string)
	// OnToolStart hears each call as soon as the model asks for it, before
	// it runs or is held; emit it as agent.tool_start. OnToolDone hears each
	// call's outcome as soon as it finishes; emit it as agent.tool_done.
	// Run calls both whatever the ToolRunner, and again for turns a durable
	// run replays, so clients dedupe by ToolCallID.
	OnToolStart func(messageID string, call ToolCall, d ToolDisplay)
	OnToolDone  func(call ToolCall, out ToolOutcome)
	// Budget bounds this run's spend.
	Budget Budget
}

// Budget bounds one run. Zero fields are unlimited.
type Budget struct {
	MaxInputTokens  int64
	MaxOutputTokens int64
	// Deadline ends the run with what was found once passed.
	Deadline time.Time
}

// ToolRunner runs one turn's tool calls and returns outs[i] for fns[i]. It is
// the durability seam: a durable runner checkpoints each fn as a step.
type ToolRunner func(ctx context.Context, fns []func(context.Context) (string, error)) ([]string, error)

// Inbox returns the messages that arrived since its last call. revoke drops
// the run's grants: a new instruction ends any pre-approval.
type Inbox func(ctx context.Context) (msgs []Message, revoke bool, err error)

// Grants are the gated tools a run may call without pausing, built from
// approve_all decisions since the last user instruction.
type Grants struct {
	// Tools may be called any number of times with any arguments. A
	// TierDestructive tool is never granted this way.
	Tools []string
}

// ToolOutcome is how one call ended.
type ToolOutcome struct {
	// OK is false when the model gets a tool error back (bad arguments,
	// handler error, timeout).
	OK bool
	// Held is true for a gated call paused for confirmation; it did not run.
	Held     bool
	Duration time.Duration
}

// StopReason is why a run ended without an error.
type StopReason string

// Stop reasons.
const (
	StopReply       StopReason = "reply"       // a plain-text turn
	StopInteraction StopReason = "interaction" // propose, ask_user or plan
	StopConfirm     StopReason = "confirm"     // a gated call is held
	StopEndsTurn    StopReason = "ends_turn"   // a tool with EndsTurn ran
	StopBudget      StopReason = "budget"      // the budget's last call replied
)

// RunResult is the outcome of a run.
type RunResult struct {
	Provider string
	Model    string
	Usage    Usage
	// ContextTokens is the prompt size of the last model call: the
	// conversation's current context.
	ContextTokens int64
	Iterations    int
	Duration      time.Duration
	Stop          StopReason

	// Reply is the final text, when the run ended on one.
	Reply string
	// Interaction is the propose, ask_user or plan call that ended the run.
	Interaction *ToolCall
	// Confirm is the gated call held for confirmation, and ConfirmPreview
	// its Check result.
	Confirm        *ToolCall
	ConfirmPreview json.RawMessage
	// Blocks are the UI cards tools emitted, in order.
	Blocks []Block
	// Messages are the assistant, tool and injected user messages appended
	// during this run, in order; the caller persists them as events.
	Messages []Message
}

// Block is one UI card a Block tool emitted.
type Block struct {
	ToolCallID string          `json:"toolCallId"`
	Tool       string          `json:"tool"`
	Data       json.RawMessage `json:"data"`
}

// CallKey identifies a call by name and key-order-independent arguments; it
// is how an approved plan's steps match calls.
func CallKey(call ToolCall) string {
	// TODO: name + sha256 of canonical JSON, as mccloud cacheKey.
	return call.Name + ":" + string(call.Arguments)
}
