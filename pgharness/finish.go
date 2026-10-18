package pgharness

import (
	"context"
	"log/slog"

	"github.com/acoshift/pgsql/pgctx"

	"github.com/xkamail/harness"
)

// Messages agent.error shows; the raw error only goes to the log.
const (
	msgError = "Something went wrong. Try again."
	msgBusy  = "The model is busy right now. Try again in a moment."
)

// Finish persists run runID's messages, stop reason and next status; a no-op if runID is no longer running.
// If the run ends idle with messages queued too late for its Inbox, it starts the next run and returns its id to kick.
func Finish(ctx context.Context, bus Bus, a *harness.Agent, conversationID, runID string, res *harness.RunResult, runErr error) (string, error) {
	var next string
	// WithoutCancel so a stop cancelling the run's ctx cannot abort the write
	err := pgctx.RunInTx(context.WithoutCancel(ctx), func(ctx context.Context) error {
		next = ""
		c, err := getConversation(ctx, conversationID, true)
		if err != nil {
			return err
		}
		if c.Status != harness.StatusRunning || c.RunID != runID {
			return nil
		}
		pending, err := pendingQueued(ctx, conversationID)
		if err != nil {
			return err
		}

		// kept even on failure: messages are complete call/result pairs and the spend is real
		var evs []harness.Event
		names := map[string]string{}
		for _, m := range res.Messages {
			switch m.Role {
			case harness.RoleUser:
				// the inbox took pending messages oldest first
				p := pending[0].UserPayload
				p.QueuedSeq = pending[0].Seq
				pending = pending[1:]
				evs = append(evs, newEvent(harness.EventUserMessage, runID, p))
			case harness.RoleAssistant:
				if m.Content != "" {
					ev := newEvent(harness.EventMessage, runID, harness.MessagePayload{Text: m.Content, Model: res.Model})
					ev.MessageID = m.ID
					evs = append(evs, ev)
				}
				for _, call := range m.ToolCalls {
					names[call.ID] = call.Name
					ev := callEvent(harness.EventToolCall, runID, call.ID, ToolCallPayload{Name: call.Name, Arguments: call.Arguments, Display: a.Display(call)})
					ev.MessageID = m.ID
					evs = append(evs, ev)
				}
			case harness.RoleTool:
				evs = append(evs, callEvent(harness.EventToolResult, runID, m.ToolCallID, ToolResultPayload{Name: names[m.ToolCallID], Content: m.Content}))
				if p, ok := parseMemoryChange(names[m.ToolCallID], m.Content); ok {
					evs = append(evs, callEvent(harness.EventMemory, runID, m.ToolCallID, p))
				}
			}
		}
		for _, bl := range res.Blocks {
			evs = append(evs, callEvent(harness.EventBlock, runID, bl.ToolCallID, bl))
		}

		status := harness.StatusIdle
		switch {
		case runErr != nil:
			msg := msgError
			if harness.IsRetryable(runErr) {
				slog.WarnContext(ctx, "pgharness: run transient error", "conversationId", conversationID, "runId", runID, "error", runErr)
				msg = msgBusy
			} else {
				slog.ErrorContext(ctx, "pgharness: run failed", "conversationId", conversationID, "runId", runID, "error", runErr)
			}
			evs = append(evs, newEvent(harness.EventError, runID, harness.ErrorPayload{Message: msg}))
			status = harness.StatusError
		case res.Confirm != nil:
			call := *res.Confirm
			evs = append(evs, callEvent(harness.EventConfirm, runID, call.ID, harness.ConfirmPayload{
				ApprovalID: call.ID,
				Call:       call,
				Tier:       a.Tier(call).String(),
				Display:    a.Display(call),
				Preview:    res.ConfirmPreview,
				GrantGroup: a.GrantGroup(call.Name),
			}))
			status = harness.StatusAwaitingInput
		case res.Interaction != nil:
			call := *res.Interaction
			typ := harness.EventQuestion
			switch call.Name {
			case harness.ToolPropose:
				typ = harness.EventProposal
			case harness.ToolPlan:
				typ = harness.EventPlan
			}
			evs = append(evs, callEvent(typ, runID, call.ID, InteractionPayload{Name: call.Name, Arguments: call.Arguments}))
			status = harness.StatusAwaitingInput
		}
		evs = append(evs, newEvent(harness.EventStatus, runID, harness.StatusPayload{
			Status:     status,
			Stop:       res.Stop,
			Usage:      &res.Usage,
			Iterations: res.Iterations,
			Model:      res.Model,
		}))
		if _, err := appendEvents(ctx, bus, conversationID, evs...); err != nil {
			return err
		}
		_, err = pgctx.Exec(ctx, `
			update harness.conversations
			set status = $2,
				context_tokens = case when $3 > 0 then $3 else context_tokens end,
				updated_at = now()
			where id = $1
		`, conversationID, status, res.ContextTokens)
		if err != nil {
			return err
		}

		// a message queued after the run's last inbox read still needs an answer
		if status != harness.StatusIdle || len(pending) == 0 {
			return nil
		}
		if err := deliverPending(ctx, bus, conversationID, ""); err != nil {
			return err
		}
		next, err = startRun(ctx, bus, conversationID)
		return err
	})
	return next, err
}
