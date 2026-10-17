package harness

import (
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
	EventSummary     EventType = "session.summary"  // compaction
	EventDelta       EventType = "agent.delta"      // ephemeral
	EventReasoning   EventType = "agent.reasoning"  // ephemeral
	EventStepReset   EventType = "step.reset"       // ephemeral: drop MessageID's streamed text
	EventToolStart   EventType = "agent.tool_start" // ephemeral: a call is about to run
	EventToolDone    EventType = "agent.tool_done"  // ephemeral: a call finished
	EventStreamReady EventType = "stream.ready"     // ephemeral, pgharness.Follow only
)

// Ephemeral reports whether events of t are never persisted.
func (t EventType) Ephemeral() bool {
	switch t {
	case EventDelta, EventReasoning, EventStepReset, EventToolStart, EventToolDone, EventStreamReady:
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
	// ToolCallID is set on agent.tool_call, agent.tool_result,
	// agent.tool_start, agent.tool_done, agent.block, agent.confirm and
	// user.decision.
	ToolCallID string          `json:"toolCallId,omitempty"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"createdAt"`
	// Lagged is set by a bus that dropped events before this one; the
	// follower (pgharness.Follow) catches up from the log when it sees it.
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
	ApprovalID string   `json:"approvalId"`
	Decision   Decision `json:"decision"`
	// ByID is who decided, for the record.
	ByID string `json:"byId,omitempty"`
	Note string `json:"note,omitempty"`
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

// Status is a conversation's run state, carried by session.status.
type Status string

// Statuses.
const (
	StatusIdle          Status = "idle"
	StatusRunning       Status = "running"
	StatusAwaitingInput Status = "awaiting_input"
	StatusError         Status = "error"
)

// ToolStartPayload is agent.tool_start: a call the model just asked for,
// before it runs or is held.
type ToolStartPayload struct {
	Name    string      `json:"name"`
	Display ToolDisplay `json:"display"`
}

// ToolDonePayload is agent.tool_done: one call's outcome, as soon as it
// finishes.
type ToolDonePayload struct {
	OK         bool  `json:"ok"`
	Held       bool  `json:"held,omitempty"`
	DurationMs int64 `json:"durationMs"`
}
