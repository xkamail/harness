package pgharness

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/acoshift/pgsql"
	"github.com/acoshift/pgsql/pgctx"

	"github.com/xkamail/harness"
)

// Loaded is everything a run needs, rebuilt from the log.
type Loaded struct {
	Conversation Conversation
	// Ctx carries the conversation (ConversationFrom) and its Values,
	// layered over the last user message's; run the agent with it.
	Ctx      context.Context
	History  []harness.Message
	Grants   harness.Grants
	Decision *harness.DecisionPayload
	// Inbox delivers messages queued while the run is busy, oldest first.
	// Wrap it with dbosharness.Inbox in a durable run.
	Inbox harness.Inbox
}

// supersededResult answers a call no result was written for because the
// user moved on: an approval that lapsed, or a question answered by the
// next message.
const supersededResult = `{"status":"not_run","note":"the user's next message answers or replaces this call"}`

// Load rebuilds the run runID of the conversation from its event log. It
// returns ErrSuperseded when runID is no longer the conversation's running
// run.
func Load(ctx context.Context, conversationID, runID string) (*Loaded, error) {
	c, err := Get(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	if c.Status != harness.StatusRunning || c.RunID != runID {
		return nil, ErrSuperseded
	}

	var (
		b         historyBuilder
		msgValues harness.Values
	)
	err = pgctx.Iter(ctx, func(scan pgsql.Scanner) error {
		var ev harness.Event
		if err := scanEvent(scan, &ev); err != nil {
			return err
		}
		if ev.Type == harness.EventUserMessage {
			var p UserPayload
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				return err
			}
			msgValues = p.Values
		}
		return b.add(ev)
	}, `
		select `+eventColumns+`
		from harness.events
		where conversation_id = $1
		order by seq
	`, conversationID)
	if err != nil {
		return nil, err
	}
	b.finish()
	if err := attachParts(ctx, conversationID, b.msgs, defaultAttachmentBudget); err != nil {
		return nil, err
	}

	rctx := harness.WithValues(ctx, msgValues)
	rctx = harness.WithValues(rctx, c.Context) // the conversation's own keys always win
	rctx = context.WithValue(rctx, conversationKey{}, c)
	return &Loaded{
		Conversation: *c,
		Ctx:          rctx,
		History:      b.msgs,
		Grants:       harness.Grants{Tools: b.grants},
		Decision:     b.decision,
		Inbox:        newInbox(conversationID),
	}, nil
}

// historyBuilder folds events into model history, grants and the pending
// decision.
type historyBuilder struct {
	msgs []harness.Message
	// unresolved are calls of the last assistant message without a result.
	unresolved []string
	results    map[string]bool
	// groups are the GrantGroups offered by each agent.confirm.
	groups   map[string][]string
	open     string
	grants   []string
	decision *harness.DecisionPayload
}

func (b *historyBuilder) add(ev harness.Event) error {
	if b.results == nil {
		b.results, b.groups = map[string]bool{}, map[string][]string{}
	}
	switch ev.Type {
	case harness.EventUserMessage:
		var p UserPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		b.supersede()
		b.open, b.grants, b.decision = "", nil, nil
		b.msgs = append(b.msgs, harness.Message{Role: harness.RoleUser, Content: p.Text, Parts: slices.Clone(p.Parts)})
	case harness.EventMessage:
		var p harness.MessagePayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		b.msgs = append(b.msgs, harness.Message{ID: ev.MessageID, Role: harness.RoleAssistant, Content: p.Text})
	case harness.EventToolCall:
		var p ToolCallPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		call := harness.ToolCall{ID: ev.ToolCallID, Name: p.Name, Arguments: p.Arguments}
		// calls of one model turn share its message id; its text came first
		if n := len(b.msgs); n > 0 && ev.MessageID != "" && b.msgs[n-1].Role == harness.RoleAssistant && b.msgs[n-1].ID == ev.MessageID {
			b.msgs[n-1].ToolCalls = append(b.msgs[n-1].ToolCalls, call)
		} else {
			b.supersede()
			b.msgs = append(b.msgs, harness.Message{ID: ev.MessageID, Role: harness.RoleAssistant, ToolCalls: []harness.ToolCall{call}})
		}
		b.unresolved = append(b.unresolved, call.ID)
	case harness.EventToolResult:
		var p ToolResultPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		b.msgs = append(b.msgs, harness.Message{Role: harness.RoleTool, Content: p.Content, ToolCallID: ev.ToolCallID})
		b.results[ev.ToolCallID] = true
		b.unresolved = slices.DeleteFunc(b.unresolved, func(id string) bool { return id == ev.ToolCallID })
	case harness.EventConfirm:
		var p harness.ConfirmPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		b.open = ev.ToolCallID
		b.groups[ev.ToolCallID] = p.GrantGroup
	case harness.EventPlan:
		b.open = ev.ToolCallID
	case harness.EventDecision:
		var p harness.DecisionPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		if p.ApprovalID != b.open {
			return nil
		}
		b.open = ""
		b.decision = &p
		if p.Decision == harness.DecisionApproveAll {
			b.grants = append(b.grants, b.groups[p.ApprovalID]...)
		}
	case harness.EventSummary:
		var p SummaryPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			return err
		}
		b.msgs = []harness.Message{{Role: harness.RoleSystem, Content: "Summary of the conversation so far:\n" + p.Text}}
		b.unresolved = nil
	}
	return nil
}

// supersede answers every unresolved call, so history never holds a call
// without a result before the next message.
func (b *historyBuilder) supersede() {
	for _, id := range b.unresolved {
		b.msgs = append(b.msgs, harness.Message{Role: harness.RoleTool, Content: supersededResult, ToolCallID: id})
		b.results[id] = true
	}
	b.unresolved = nil
}

func (b *historyBuilder) finish() {
	// a retried run whose decided call already ran must not run it again
	if b.decision != nil && b.results[b.decision.ApprovalID] {
		b.decision = nil
	}
}

// newInbox reads the pending user.queued messages the run has not taken
// yet. Any message is a new instruction, so it revokes the run's grants.
func newInbox(conversationID string) harness.Inbox {
	taken := map[int64]bool{}
	return func(ctx context.Context) ([]harness.Message, bool, error) {
		qs, err := pendingQueued(ctx, conversationID)
		if err != nil {
			return nil, false, err
		}
		msgs := make([]harness.Message, 0, len(qs))
		for _, q := range qs {
			if taken[q.Seq] {
				continue
			}
			taken[q.Seq] = true
			msgs = append(msgs, harness.Message{Role: harness.RoleUser, Content: q.Text, Parts: q.Parts})
		}
		if len(msgs) == 0 {
			return nil, false, nil
		}
		if err := attachParts(ctx, conversationID, msgs, defaultAttachmentBudget); err != nil {
			return nil, false, err
		}
		return msgs, true, nil
	}
}
