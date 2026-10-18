package pgharness

import (
	"context"

	"github.com/acoshift/pgsql/pgctx"

	"github.com/xkamail/harness"
)

// Compact writes a session.summary of the conversation's older turns once
// its context_tokens reach the agent's Compaction.Threshold, so later runs
// load the summary and the recent turns verbatim. Call it after Finish, not
// inside a transaction: it calls m (Compaction.Model when set). It is a
// no-op when the agent does not compact, the threshold is not reached,
// there is nothing to cut, or another summary was written meanwhile.
func Compact(ctx context.Context, bus Bus, a *harness.Agent, m harness.Model, conversationID string) error {
	cfg := a.Compaction
	if cfg == nil {
		return nil
	}
	c, err := Get(ctx, conversationID)
	if err != nil {
		return err
	}
	if c.ContextTokens < cfg.Threshold {
		return nil
	}
	b, _, err := loadHistory(ctx, conversationID)
	if err != nil {
		return err
	}
	cp, err := harness.Compact(ctx, m, b.msgs, *cfg)
	if err != nil || cp == nil {
		return err
	}
	// the cut is at a user message: the first one kept verbatim
	through := b.seqs[cp.Through+1]

	return pgctx.RunInTx(ctx, func(ctx context.Context) error {
		if _, err := getConversation(ctx, conversationID, true); err != nil {
			return err
		}
		var newer bool
		err := pgctx.QueryRow(ctx, `
			select exists (
				select 1
				from harness.events
				where conversation_id = $1
					and type = $2
					and seq > $3
			)
		`, conversationID, harness.EventSummary, b.last).Scan(&newer)
		if err != nil || newer {
			return err
		}
		_, err = appendEvents(ctx, bus, conversationID, newEvent(harness.EventSummary, "", SummaryPayload{Text: cp.Summary, Through: through}))
		return err
	})
}
