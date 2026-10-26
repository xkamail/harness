package pgharness

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/acoshift/pgsql"
	"github.com/acoshift/pgsql/pgctx"
	"github.com/rs/xid"

	"github.com/xkamail/harness"
)

// StartParams opens, or resumes under Policy, a conversation with a first message.
type StartParams struct {
	Agent   string
	OwnerID string
	Context harness.Values
	// Require are context keys that must be set; else ErrContext.
	Require []string
	Policy  Policy
	// Model is pinned on a new conversation.
	Model   string
	Message string
	// Parts are validated with harness.ValidateParts.
	Parts []harness.Part
	// Values are per-message (e.g. the user's current page).
	Values harness.Values
}

// Start opens or resumes a conversation and Sends p's message. It returns the run id to kick,
// or "" when the resumed conversation was busy and the message was queued.
func Start(ctx context.Context, bus Bus, p StartParams) (*Conversation, string, error) {
	for _, k := range p.Require {
		if p.Context[k] == "" {
			return nil, "", fmt.Errorf("%w: %s", ErrContext, k)
		}
	}
	if p.Context == nil {
		p.Context = harness.Values{}
	}
	var (
		c     *Conversation
		runID string
	)
	err := pgctx.RunInTx(ctx, func(ctx context.Context) error {
		id, err := findConversation(ctx, p)
		if err != nil {
			return err
		}
		if id == "" {
			id = xid.New().String()
			_, err = pgctx.Exec(ctx, `
				insert into harness.conversations (id, agent, owner_id, context, title, model)
				values ($1, $2, $3, $4, $5, $6)
			`, id, p.Agent, p.OwnerID, pgsql.JSON(p.Context), title(p.Message), p.Model)
			if err != nil {
				return err
			}
		}
		c, err = getConversation(ctx, id, true)
		if err != nil {
			return err
		}
		runID, err = send(ctx, bus, c, SendParams{ConversationID: id, Message: p.Message, Parts: p.Parts, Values: p.Values})
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return c, runID, nil
}

func findConversation(ctx context.Context, p StartParams) (string, error) {
	var (
		id  string
		err error
	)
	switch p.Policy {
	case PolicyPerContext:
		err = pgctx.QueryRow(ctx, `
			select id
			from harness.conversations
			where agent = $1
				and owner_id = $2
				and context = $3
			order by updated_at desc
			limit 1
		`, p.Agent, p.OwnerID, pgsql.JSON(p.Context)).Scan(&id)
	case PolicyShared:
		err = pgctx.QueryRow(ctx, `
			select id
			from harness.conversations
			where agent = $1
				and context = $2
			order by updated_at desc
			limit 1
		`, p.Agent, pgsql.JSON(p.Context)).Scan(&id)
	default:
		return "", nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

const maxTitleRunes = 80

func title(msg string) string {
	msg = strings.Join(strings.Fields(msg), " ")
	if r := []rune(msg); len(r) > maxTitleRunes {
		return string(r[:maxTitleRunes-1]) + "…"
	}
	return msg
}

// SetTitle replaces the title Start took from the first message, normalized the same way.
// It leaves updated_at alone, so it neither reorders resumes nor delays FailOrphanedRuns.
func SetTitle(ctx context.Context, conversationID, s string) error {
	res, err := pgctx.Exec(ctx, `
		update harness.conversations
		set title = $2
		where id = $1
	`, conversationID, title(s))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SendParams continues a conversation with a message or a decision.
type SendParams struct {
	ConversationID string
	Message        string
	Parts          []harness.Part
	Values         harness.Values
	// Decision answers the open agent.confirm/agent.plan; Message and Parts are then ignored (use its Note).
	Decision *harness.DecisionPayload
}

// Send returns the run id to kick, or "" if queued. The caller checks write access and a decision's ByID.
//
//	running:        message → user.queued; decision → ErrBusy
//	idle, error:    message → user.message + run; decision → ErrStaleApproval
//	awaiting_input: message → user.message + run (open approval lapses); decision on it → user.decision + run
func Send(ctx context.Context, bus Bus, p SendParams) (string, error) {
	var runID string
	err := pgctx.RunInTx(ctx, func(ctx context.Context) error {
		c, err := getConversation(ctx, p.ConversationID, true)
		if err != nil {
			return err
		}
		runID, err = send(ctx, bus, c, p)
		return err
	})
	return runID, err
}

// send is Send on a locked conversation.
func send(ctx context.Context, bus Bus, c *Conversation, p SendParams) (string, error) {
	if p.Decision != nil {
		if c.Status == harness.StatusRunning {
			return "", ErrBusy
		}
		open, err := openApproval(ctx, c.ID)
		if err != nil {
			return "", err
		}
		if open == "" || open != p.Decision.ApprovalID {
			return "", harness.ErrStaleApproval
		}
		switch p.Decision.Decision {
		case harness.DecisionApprove, harness.DecisionApproveAll, harness.DecisionReject:
		default:
			return "", fmt.Errorf("pgharness: unknown decision %q", p.Decision.Decision)
		}
		if _, err := appendEvents(ctx, bus, c.ID, callEvent(harness.EventDecision, "", open, p.Decision)); err != nil {
			return "", err
		}
		return startRun(ctx, bus, c.ID)
	}

	if strings.TrimSpace(p.Message) == "" && len(p.Parts) == 0 {
		return "", ErrEmpty
	}
	if err := harness.ValidateParts(p.Parts, harness.DefaultAttachmentLimits); err != nil {
		return "", err
	}
	if c.Status == harness.StatusRunning {
		pending, err := pendingQueued(ctx, c.ID)
		if err != nil {
			return "", err
		}
		if len(pending) >= maxQueued {
			return "", ErrQueueFull
		}
	}
	parts, err := storeParts(ctx, c.ID, p.Parts)
	if err != nil {
		return "", err
	}
	msg := UserPayload{Text: p.Message, Parts: parts, Values: p.Values}
	if c.Status == harness.StatusRunning {
		_, err := appendEvents(ctx, bus, c.ID, newEvent(harness.EventUserQueued, "", msg))
		return "", err
	}
	// messages queued for a run that ended without reading them come first
	if err := deliverPending(ctx, bus, c.ID, ""); err != nil {
		return "", err
	}
	if _, err := appendEvents(ctx, bus, c.ID, newEvent(harness.EventUserMessage, "", msg)); err != nil {
		return "", err
	}
	return startRun(ctx, bus, c.ID)
}

// Retry re-runs a conversation whose last run ended in error and returns the new run id to kick,
// or "" if the conversation is not in error. Queued messages the failed run never read come first.
func Retry(ctx context.Context, bus Bus, conversationID string) (string, error) {
	var runID string
	err := pgctx.RunInTx(ctx, func(ctx context.Context) error {
		runID = ""
		c, err := getConversation(ctx, conversationID, true)
		if err != nil || c.Status != harness.StatusError {
			return err
		}
		if err := deliverPending(ctx, bus, c.ID, ""); err != nil {
			return err
		}
		runID, err = startRun(ctx, bus, c.ID)
		return err
	})
	return runID, err
}

// Stop ends the running run and returns its id to cancel (its Finish becomes a no-op), or "" if not running.
// Queued messages the run never read are delivered for the next run.
func Stop(ctx context.Context, bus Bus, conversationID string) (string, error) {
	var runID string
	err := pgctx.RunInTx(ctx, func(ctx context.Context) error {
		runID = ""
		c, err := getConversation(ctx, conversationID, true)
		if err != nil || c.Status != harness.StatusRunning {
			return err
		}
		runID = c.RunID
		if err := deliverPending(ctx, bus, c.ID, ""); err != nil {
			return err
		}
		_, err = appendEvents(ctx, bus, c.ID,
			newEvent(harness.EventStatus, runID, harness.StatusPayload{Status: harness.StatusIdle}))
		if err != nil {
			return err
		}
		return setStatus(ctx, c.ID, harness.StatusIdle, "")
	})
	return runID, err
}
