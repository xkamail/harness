// Package pgharness is Postgres storage and lifecycle for harness conversations (see README.md).
// Functions use the db from ctx (pgctx), join the caller's tx, and publish to the Bus only after commit.
package pgharness

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/acoshift/pgsql"
	"github.com/acoshift/pgsql/pgctx"

	"github.com/xkamail/harness"
)

var (
	ErrNotFound = errors.New("pgharness: conversation not found")
	// ErrBusy: a decision sent while a run is running.
	ErrBusy = errors.New("pgharness: conversation is busy")
	// ErrQueueFull: too many messages queued while a run is busy.
	ErrQueueFull = errors.New("pgharness: queue is full")
	// ErrContext: Start without one of its Require keys.
	ErrContext = errors.New("pgharness: conversation context missing a key")
	// ErrSuperseded: Load for a run that is no longer current (stopped, failed or replaced).
	ErrSuperseded = errors.New("pgharness: run is superseded")
	// ErrEmpty: Send or Start with neither text nor parts.
	ErrEmpty = errors.New("pgharness: empty message")
)

// maxQueued caps user.queued messages waiting on one busy run.
const maxQueued = 10

// Policy is how Start finds the conversation it writes to.
type Policy int

const (
	PolicyNew Policy = iota
	// PolicyPerContext resumes the owner's conversation with the same agent and context.
	PolicyPerContext
	// PolicyShared resumes the agent+context conversation regardless of owner (e.g. a support thread).
	PolicyShared
)

type Conversation struct {
	ID      string `json:"id"`
	Agent   string `json:"agent"`
	OwnerID string `json:"ownerId"`
	// Context is set once at Start and put in every run's ctx.
	Context harness.Values `json:"context"`
	Title   string         `json:"title,omitempty"`
	Status  harness.Status `json:"status"`
	// RunID is the current or last run; only that run may Finish.
	RunID         string    `json:"runId,omitempty"`
	Model         string    `json:"model,omitempty"`
	ContextTokens int64     `json:"contextTokens"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

const conversationColumns = `
	id,
	agent,
	owner_id,
	context,
	title,
	status,
	coalesce(run_id, ''),
	model,
	context_tokens,
	created_at,
	updated_at
`

func scanConversation(scan pgsql.Scanner, c *Conversation) error {
	return scan(
		&c.ID,
		&c.Agent,
		&c.OwnerID,
		pgsql.JSON(&c.Context),
		&c.Title,
		&c.Status,
		&c.RunID,
		&c.Model,
		&c.ContextTokens,
		&c.CreatedAt,
		&c.UpdatedAt,
	)
}

// Get does not check the owner; the caller must.
func Get(ctx context.Context, id string) (*Conversation, error) {
	return getConversation(ctx, id, false)
}

// getConversation with lock takes FOR UPDATE; every writer locks first, which serializes seq allocation.
func getConversation(ctx context.Context, id string, lock bool) (*Conversation, error) {
	q := `select ` + conversationColumns + ` from harness.conversations where id = $1`
	if lock {
		q += ` for update`
	}
	var c Conversation
	err := scanConversation(pgctx.QueryRow(ctx, q, id).Scan, &c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

type conversationKey struct{}

// ConversationFrom returns the conversation Load put in a run's ctx.
func ConversationFrom(ctx context.Context) *Conversation {
	c, _ := ctx.Value(conversationKey{}).(*Conversation)
	return c
}

type blobsKey struct{}

// WithBlobs stores attachment bytes in b instead of the attachments row.
func WithBlobs(ctx context.Context, b Blobs) context.Context {
	return context.WithValue(ctx, blobsKey{}, b)
}

func blobsFrom(ctx context.Context) Blobs {
	b, _ := ctx.Value(blobsKey{}).(Blobs)
	return b
}
