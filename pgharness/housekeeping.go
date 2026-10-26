package pgharness

import (
	"context"
	"errors"
	"time"

	"github.com/acoshift/pgsql"
	"github.com/acoshift/pgsql/pgctx"

	"github.com/xkamail/harness"
)

// FailOrphanedRuns errors conversations running for >= idle whose run alive reports dead (e.g. unknown to DBOS).
// Call it with idle 0 after launch, then periodically.
func FailOrphanedRuns(ctx context.Context, bus Bus, idle time.Duration, alive func(runID string) bool) error {
	type run struct{ conversationID, runID string }
	var runs []run
	err := pgctx.Iter(ctx, func(scan pgsql.Scanner) error {
		var r run
		if err := scan(&r.conversationID, &r.runID); err != nil {
			return err
		}
		runs = append(runs, r)
		return nil
	}, `
		select
			id,
			coalesce(run_id, '')
		from harness.conversations
		where status = 'running'
			and deleted_at is null
			and updated_at <= now() - make_interval(secs => $1)
	`, idle.Seconds())
	if err != nil {
		return err
	}
	for _, r := range runs {
		if alive(r.runID) {
			continue
		}
		err := pgctx.RunInTx(ctx, func(ctx context.Context) error {
			c, err := getConversation(ctx, r.conversationID, true)
			if err != nil || c.Status != harness.StatusRunning || c.RunID != r.runID {
				return err
			}
			_, err = appendEvents(ctx, bus, c.ID,
				newEvent(harness.EventError, r.runID, harness.ErrorPayload{Message: msgError}),
				newEvent(harness.EventStatus, r.runID, harness.StatusPayload{Status: harness.StatusError}),
			)
			if err != nil {
				return err
			}
			return setStatus(ctx, c.ID, harness.StatusError, "")
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// DeleteContext deletes conversations (with events, attachments, blobs) and memories whose context contains v,
// e.g. DeleteContext(ctx, harness.Values{"serverId": id}) when the project deletes that subject.
func DeleteContext(ctx context.Context, v harness.Values) error {
	if len(v) == 0 {
		return errors.New("pgharness: DeleteContext needs a non-empty context")
	}
	var ids []string
	err := pgctx.RunInTx(ctx, func(ctx context.Context) error {
		ids = nil
		err := pgctx.Iter(ctx, func(scan pgsql.Scanner) error {
			var id string
			if err := scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
			return nil
		}, `
			delete from harness.conversations
			where context @> $1
			returning id
		`, pgsql.JSON(v))
		if err != nil {
			return err
		}
		_, err = pgctx.Exec(ctx, `
			delete from harness.memories
			where context @> $1
		`, pgsql.JSON(v))
		return err
	})
	if err != nil {
		return err
	}
	// blobs after commit: a failure leaves orphans, never rows pointing at missing bytes
	if blobs := blobsFrom(ctx); blobs != nil {
		for _, id := range ids {
			if err := blobs.DeletePrefix(ctx, blobPrefix(id)); err != nil {
				return err
			}
		}
	}
	return nil
}
