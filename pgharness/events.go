package pgharness

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/acoshift/pgsql"
	"github.com/acoshift/pgsql/pgctx"
	"github.com/rs/xid"

	"github.com/xkamail/harness"
)

// Payloads pgharness writes beyond the ones the core owns.

// UserPayload is user.message and user.queued. Parts hold text parts and
// attachment references, never bytes.
type UserPayload struct {
	Text  string         `json:"text"`
	Parts []harness.Part `json:"parts,omitempty"`
	// Values are per-message values, such as the page the user is on,
	// layered under the conversation's context for the run that reads it.
	Values harness.Values `json:"values,omitempty"`
	// QueuedSeq is the user.queued a delivered message was sent as.
	QueuedSeq int64 `json:"queuedSeq,omitempty"`
}

// ToolCallPayload is agent.tool_call, one per call; calls of one assistant
// message share its MessageID.
type ToolCallPayload struct {
	Name      string              `json:"name"`
	Arguments json.RawMessage     `json:"arguments"`
	Display   harness.ToolDisplay `json:"display"`
}

// ToolResultPayload is agent.tool_result.
type ToolResultPayload struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// InteractionPayload is agent.question, agent.proposal and agent.plan: the
// interaction call's arguments, schema-valid.
type InteractionPayload struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// SummaryPayload is session.summary: Text replaces every message before
// the user.message at seq Through, which starts the tail kept verbatim.
type SummaryPayload struct {
	Text    string `json:"text"`
	Through int64  `json:"through"`
}

func newEvent(typ harness.EventType, runID string, payload any) harness.Event {
	b, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return harness.Event{Type: typ, RunID: runID, Payload: b}
}

func callEvent(typ harness.EventType, runID, toolCallID string, payload any) harness.Event {
	ev := newEvent(typ, runID, payload)
	ev.ToolCallID = toolCallID
	return ev
}

// appendEvents persists evs in order with the next seqs and publishes them
// once the transaction commits. The caller holds the conversation row lock.
func appendEvents(ctx context.Context, bus Bus, conversationID string, evs ...harness.Event) ([]harness.Event, error) {
	out := make([]harness.Event, 0, len(evs))
	for _, ev := range evs {
		err := pgctx.QueryRow(ctx, `
			insert into harness.events (
				conversation_id,
				seq,
				type,
				run_id,
				message_id,
				tool_call_id,
				payload
			)
			values (
				$1,
				(select coalesce(max(seq), 0) + 1 from harness.events where conversation_id = $1),
				$2,
				$3,
				$4,
				$5,
				$6
			)
			returning
				seq,
				created_at
		`,
			conversationID,
			ev.Type,
			pgsql.NullString(&ev.RunID),
			pgsql.NullString(&ev.MessageID),
			pgsql.NullString(&ev.ToolCallID),
			string(ev.Payload),
		).Scan(
			&ev.Seq,
			&ev.CreatedAt,
		)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	_, err := pgctx.Exec(ctx, `
		update harness.conversations
		set updated_at = now()
		where id = $1
	`, conversationID)
	if err != nil {
		return nil, err
	}
	pgctx.Committed(ctx, func(ctx context.Context) { publish(ctx, bus, conversationID, out...) })
	return out, nil
}

const publishTimeout = 2 * time.Second

// publish fans evs out. It is best effort: Follow repairs a lost publish
// from the log.
func publish(ctx context.Context, bus Bus, conversationID string, evs ...harness.Event) {
	if bus == nil {
		return
	}
	// the rows are committed: a cancelled run must still announce them
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()
	for _, ev := range evs {
		if err := bus.Publish(ctx, conversationID, ev); err != nil {
			slog.WarnContext(ctx, "pgharness: publish", "conversationId", conversationID, "error", err)
		}
	}
}

// Live publishes an ephemeral event from a run's callbacks (OnDelta,
// OnToolStart …) with the run's id.
func Live(ctx context.Context, bus Bus, conversationID, runID string, typ harness.EventType, messageID, toolCallID string, payload any) {
	ev := newEvent(typ, runID, payload)
	ev.MessageID, ev.ToolCallID = messageID, toolCallID
	ev.CreatedAt = time.Now()
	publish(ctx, bus, conversationID, ev)
}

const eventColumns = `
	seq,
	type,
	coalesce(run_id, ''),
	coalesce(message_id, ''),
	coalesce(tool_call_id, ''),
	payload,
	created_at
`

func scanEvent(scan pgsql.Scanner, ev *harness.Event) error {
	var payload []byte
	err := scan(
		&ev.Seq,
		&ev.Type,
		&ev.RunID,
		&ev.MessageID,
		&ev.ToolCallID,
		&payload,
		&ev.CreatedAt,
	)
	ev.Payload = payload
	return err
}

// Events returns up to limit events of the conversation with seq > after,
// in seq order.
func Events(ctx context.Context, conversationID string, after int64, limit int) ([]harness.Event, error) {
	out := make([]harness.Event, 0)
	err := pgctx.Iter(ctx, func(scan pgsql.Scanner) error {
		var ev harness.Event
		if err := scanEvent(scan, &ev); err != nil {
			return err
		}
		out = append(out, ev)
		return nil
	}, `
		select `+eventColumns+`
		from harness.events
		where conversation_id = $1
			and seq > $2
		order by seq
		limit $3
	`, conversationID, after, limit)
	return out, err
}

type queuedMessage struct {
	Seq int64
	UserPayload
}

// pendingQueued returns the user.queued messages no user.message has
// delivered yet, oldest first.
func pendingQueued(ctx context.Context, conversationID string) ([]queuedMessage, error) {
	out := make([]queuedMessage, 0)
	err := pgctx.Iter(ctx, func(scan pgsql.Scanner) error {
		var (
			q       queuedMessage
			payload []byte
		)
		if err := scan(&q.Seq, &payload); err != nil {
			return err
		}
		if err := json.Unmarshal(payload, &q.UserPayload); err != nil {
			return err
		}
		out = append(out, q)
		return nil
	}, `
		select
			q.seq,
			q.payload
		from harness.events q
		where q.conversation_id = $1
			and q.type = $2
			and not exists (
				select 1
				from harness.events m
				where m.conversation_id = q.conversation_id
					and m.type = $3
					and (m.payload->>'queuedSeq')::bigint = q.seq
			)
		order by q.seq
	`, conversationID, harness.EventUserQueued, harness.EventUserMessage)
	return out, err
}

// deliverPending appends every pending queued message as the user.message
// that delivers it.
func deliverPending(ctx context.Context, bus Bus, conversationID, runID string) error {
	qs, err := pendingQueued(ctx, conversationID)
	if err != nil || len(qs) == 0 {
		return err
	}
	evs := make([]harness.Event, 0, len(qs))
	for _, q := range qs {
		p := q.UserPayload
		p.QueuedSeq = q.Seq
		evs = append(evs, newEvent(harness.EventUserMessage, runID, p))
	}
	_, err = appendEvents(ctx, bus, conversationID, evs...)
	return err
}

// startRun moves the locked conversation to a new running run and returns
// its id.
func startRun(ctx context.Context, bus Bus, conversationID string) (string, error) {
	runID := xid.New().String()
	_, err := appendEvents(ctx, bus, conversationID,
		newEvent(harness.EventStatus, runID, harness.StatusPayload{Status: harness.StatusRunning}))
	if err != nil {
		return "", err
	}
	return runID, setStatus(ctx, conversationID, harness.StatusRunning, runID)
}

func setStatus(ctx context.Context, conversationID string, status harness.Status, runID string) error {
	_, err := pgctx.Exec(ctx, `
		update harness.conversations
		set status = $2,
			run_id = coalesce($3, run_id),
			updated_at = now()
		where id = $1
	`, conversationID, status, pgsql.NullString(&runID))
	return err
}

// openApproval returns the ToolCallID of the conversation's latest
// agent.confirm or agent.plan when no user.decision or user.message has
// answered it since, else "".
func openApproval(ctx context.Context, conversationID string) (string, error) {
	var (
		typ string
		id  string
	)
	err := pgctx.QueryRow(ctx, `
		select
			type,
			coalesce(tool_call_id, '')
		from harness.events
		where conversation_id = $1
			and type in ($2, $3, $4, $5)
		order by seq desc
		limit 1
	`, conversationID, harness.EventConfirm, harness.EventPlan, harness.EventDecision, harness.EventUserMessage).Scan(
		&typ,
		&id,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if harness.EventType(typ) == harness.EventConfirm || harness.EventType(typ) == harness.EventPlan {
		return id, nil
	}
	return "", nil
}
