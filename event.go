package harness

import (
	"encoding/json"
	"strconv"
	"time"
)

type EventType string

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

// Ephemeral reports whether t is never persisted.
func (t EventType) Ephemeral() bool {
	switch t {
	case EventDelta, EventReasoning, EventStepReset, EventToolStart, EventToolDone, EventStreamReady:
		return true
	}
	return false
}

// Event: persisted events have Seq > 0, monotonic per conversation; ephemeral have Seq == 0.
// Envelope ids let clients place events without decoding the payload.
type Event struct {
	Seq        int64           `json:"seq,omitempty"`
	Type       EventType       `json:"type"`
	RunID      string          `json:"runId,omitempty"`
	MessageID  string          `json:"messageId,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"createdAt"`
	// Lagged: the bus dropped events before this one; pgharness.Follow catches up from the log.
	Lagged bool `json:"-"`
}

// MessageID of a run's ith model call (from 1); deterministic so a replayed durable run reuses ids.
func MessageID(runID string, i int) string {
	return runID + "." + strconv.Itoa(i)
}

// DeltaPayload is agent.delta and agent.reasoning.
type DeltaPayload struct {
	Text string `json:"text"`
}

// MessagePayload is agent.message; it replaces the deltas streamed under the same MessageID.
type MessagePayload struct {
	Text  string `json:"text"`
	Model string `json:"model,omitempty"`
}

// ConfirmPayload is agent.confirm. ApprovalID is the held call's ToolCallID.
type ConfirmPayload struct {
	ApprovalID string          `json:"approvalId"`
	Call       ToolCall        `json:"call"`
	Tier       string          `json:"tier"`
	Display    ToolDisplay     `json:"display"`
	Preview    json.RawMessage `json:"preview,omitempty"`
	// Empty means approve_all is not offered.
	GrantGroup []string `json:"grantGroup,omitempty"`
}

type Decision string

const (
	DecisionApprove    Decision = "approve"
	DecisionApproveAll Decision = "approve_all" // the call's GrantGroup for the rest of the task
	DecisionReject     Decision = "reject"
)

// DecisionPayload is user.decision. An ApprovalID not naming the open confirmation or plan
// is refused with ErrStaleApproval, so a late click on an old button approves nothing.
type DecisionPayload struct {
	ApprovalID string   `json:"approvalId"`
	Decision   Decision `json:"decision"`
	ByID       string   `json:"byId,omitempty"`
	Note       string   `json:"note,omitempty"`
}

// StatusPayload is session.status; at run end it also carries stop reason and cost.
type StatusPayload struct {
	Status     Status     `json:"status"`
	Stop       StopReason `json:"stop,omitempty"`
	Usage      *Usage     `json:"usage,omitempty"`
	Iterations int        `json:"iterations,omitempty"`
	Model      string     `json:"model,omitempty"`
}

// ErrorPayload is agent.error: sanitized and user-renderable; raw detail stays in logs.
type ErrorPayload struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

type Status string

const (
	StatusIdle          Status = "idle"
	StatusRunning       Status = "running"
	StatusAwaitingInput Status = "awaiting_input"
	StatusError         Status = "error"
)

// ToolStartPayload is agent.tool_start, sent before the call runs or is held.
type ToolStartPayload struct {
	Name    string      `json:"name"`
	Display ToolDisplay `json:"display"`
}

// ToolDonePayload is agent.tool_done.
type ToolDonePayload struct {
	OK         bool  `json:"ok"`
	Held       bool  `json:"held,omitempty"`
	DurationMs int64 `json:"durationMs"`
}
