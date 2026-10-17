package harness

import (
	"context"
	"encoding/json"
	"strconv"
	"time"
)

// EventType names a conversation event.
type EventType string

// Event types. All are persisted except the ephemeral ones (see Ephemeral).
const (
	EventUserMessage EventType = "user.message"
	EventUserQueued  EventType = "user.queued" // sent while a run was busy
	EventMessage     EventType = "agent.message"
	EventQuestion    EventType = "agent.question"
	EventProposal    EventType = "agent.proposal"
	EventPlan        EventType = "agent.plan"
	EventConfirm     EventType = "agent.confirm"
	EventDecision    EventType = "user.decision" // approve, approve_all, reject
	EventToolCall    EventType = "agent.tool_call"
	EventToolResult  EventType = "agent.tool_result"
	EventBlock       EventType = "agent.block"
	EventMemory      EventType = "agent.memory"
	EventError       EventType = "agent.error"
	EventStatus      EventType = "session.status"
	EventHandoff     EventType = "session.handoff"
	EventSummary     EventType = "session.summary" // compaction
	EventDelta       EventType = "agent.delta"     // ephemeral
	EventReasoning   EventType = "agent.reasoning" // ephemeral
	EventStepReset   EventType = "step.reset"      // ephemeral: drop MessageID's streamed text
	EventStreamReady EventType = "stream.ready"    // ephemeral, Follow only
)

// Ephemeral reports whether events of t are never persisted.
func (t EventType) Ephemeral() bool {
	switch t {
	case EventDelta, EventReasoning, EventStepReset, EventStreamReady:
		return true
	}
	return false
}

// Event is one entry of a conversation's log. Persisted events have Seq > 0,
// monotonic per conversation; ephemeral events have Seq == 0.
//
// The ids on the envelope let a client place an event without decoding its
// payload: RunID drops events of a superseded run, MessageID ties deltas,
// resets and the final agent.message together, and ToolCallID ties a call to
// its result, block, confirmation and decision.
type Event struct {
	Seq  int64     `json:"seq,omitempty"`
	Type EventType `json:"type"`
	// RunID is set on every event a run emits, and on session.status.
	RunID string `json:"runId,omitempty"`
	// MessageID is set on agent.delta, agent.reasoning, step.reset and
	// agent.message.
	MessageID string `json:"messageId,omitempty"`
	// ToolCallID is set on agent.tool_call, agent.tool_result, agent.block,
	// agent.confirm and user.decision.
	ToolCallID string          `json:"toolCallId,omitempty"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"createdAt"`
	// Lagged is set by a Bus that dropped events before this one; Follow
	// catches up from the Store when it sees it.
	Lagged bool `json:"-"`
}

// MessageID is the id of the assistant message a run's ith model call
// (from 1) produces. It is derived, not random, so a durable run replayed
// after a crash streams under the same ids.
func MessageID(runID string, i int) string {
	return runID + "." + strconv.Itoa(i)
}

// Payloads of the events whose shape the harness owns. Every other payload
// is the project's.

// DeltaPayload is agent.delta and agent.reasoning.
type DeltaPayload struct {
	Text string `json:"text"`
}

// MessagePayload is agent.message: the full text that replaces the deltas
// streamed under the same MessageID.
type MessagePayload struct {
	Text  string `json:"text"`
	Model string `json:"model,omitempty"`
}

// ConfirmPayload is agent.confirm: a gated call held for approval. Its
// ApprovalID is the held call's ToolCallID; a decision must name it.
type ConfirmPayload struct {
	ApprovalID string          `json:"approvalId"`
	Call       ToolCall        `json:"call"`
	Tier       string          `json:"tier"`
	Display    ToolDisplay     `json:"display"`
	Preview    json.RawMessage `json:"preview,omitempty"`
	// GrantGroup lists the tools approve_all would cover; empty means
	// approve_all is not offered.
	GrantGroup []string `json:"grantGroup,omitempty"`
}

// Decision is a person's answer to a confirmation or a plan.
type Decision string

// Decisions.
const (
	DecisionApprove    Decision = "approve"
	DecisionApproveAll Decision = "approve_all" // the call's GrantGroup for the rest of the task
	DecisionReject     Decision = "reject"
)

// DecisionPayload is user.decision. ApprovalID must name the conversation's
// open confirmation or plan (its ToolCallID); a decision naming any other is
// refused with ErrStaleApproval, so a late click on an old button in a shared
// conversation approves nothing.
type DecisionPayload struct {
	ApprovalID string    `json:"approvalId"`
	Decision   Decision  `json:"decision"`
	By         Principal `json:"by"`
	Note       string    `json:"note,omitempty"`
}

// StatusPayload is session.status. When a run ends it also carries why and
// what it cost, so a client shows both without another call.
type StatusPayload struct {
	Status     Status     `json:"status"`
	Stop       StopReason `json:"stop,omitempty"`
	Usage      *Usage     `json:"usage,omitempty"`
	Iterations int        `json:"iterations,omitempty"`
	Model      string     `json:"model,omitempty"`
}

// ErrorPayload is agent.error: a sanitized, user-renderable reason. Raw
// error detail stays in logs.
type ErrorPayload struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

// Store is a conversation's event log, the source of truth. Projects own the
// table; harnesstest.Store checks an implementation.
type Store interface {
	// Append persists evs in order, assigns each the next Seq and returns
	// them. Ephemeral events are rejected.
	Append(ctx context.Context, conversationID string, evs ...Event) ([]Event, error)
	// After returns up to limit events with Seq > seq, in Seq order.
	After(ctx context.Context, conversationID string, seq int64, limit int) ([]Event, error)
}

// Bus fans events out to every instance. Delivery may be lossy and out of
// order; Follow repairs both from the Store. harnesstest.Bus checks an
// implementation.
type Bus interface {
	Publish(ctx context.Context, conversationID string, ev Event) error
	// Subscribe must be receiving when it returns, so a Publish right after
	// it is delivered. stop closes the channel and is safe to call twice.
	Subscribe(ctx context.Context, conversationID string) (events <-chan Event, stop func(), err error)
}

// FollowConfig tunes Follow; zero values use the defaults.
type FollowConfig struct {
	// PageSize is the Store page; zero means 200.
	PageSize int
	// Heal re-reads the Store this often to repair a missed publish; zero
	// means 15s. Transports send their keep-alive on the same tick.
	Heal time.Duration
	// OnTick is called on every Heal tick, after the catch-up, such as to
	// send an SSE ": ping".
	OnTick func() error
}

// Follow yields every persisted event after afterSeq exactly once and in Seq
// order, then every live event, until ctx ends or yield fails. It subscribes
// before reading the Store, treats a persisted live event only as a signal
// to catch up, passes ephemeral events through as they come, and yields
// EventStreamReady once the backlog is sent. Every transport (SSE,
// WebSocket, a chat bot) builds on it rather than on Bus directly.
func Follow(ctx context.Context, st Store, bus Bus, conversationID string, afterSeq int64, cfg FollowConfig, yield func(Event) error) error {
	// TODO: port catchUp from mccloud api/ai/stream.go.
	return ErrNotImplemented
}
