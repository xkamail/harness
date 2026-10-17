package harness

import (
	"context"
	"time"
)

// Principal is a person acting in a conversation.
type Principal struct {
	ID string `json:"id"`
	// Role is project-defined, such as "owner", "customer", "staff".
	Role string `json:"role,omitempty"`
	// Name is for display and prompts only, never for authorization.
	Name string `json:"name,omitempty"`
}

// Status is a conversation's run state.
type Status string

// Statuses.
const (
	StatusIdle          Status = "idle"
	StatusRunning       Status = "running"
	StatusAwaitingInput Status = "awaiting_input"
	StatusError         Status = "error"
)

// Conversation is one instance of an agent.
type Conversation struct {
	ID string `json:"id"`
	// Agent is the Agent.Name it runs.
	Agent   string `json:"agent"`
	OwnerID string `json:"ownerId"`
	// Context is set once at start and put in every run's ctx (Values).
	Context Values `json:"context"`
	// ParentID is the conversation this one was handed off from.
	ParentID string `json:"parentId,omitempty"`
	Title    string `json:"title,omitempty"`
	Status   Status `json:"status"`
	// RunID is the current or last run; only that run may write its outcome.
	RunID string `json:"runId,omitempty"`
	// Model is the pinned model, under ModelPinned.
	Model         string    `json:"model,omitempty"`
	ContextTokens int64     `json:"contextTokens"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type (
	principalKey    struct{}
	conversationKey struct{}
)

// WithPrincipal returns ctx carrying the principal the run acts for.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal set by WithPrincipal.
func PrincipalFrom(ctx context.Context) Principal {
	p, _ := ctx.Value(principalKey{}).(Principal)
	return p
}

// WithConversation returns ctx carrying c.
func WithConversation(ctx context.Context, c Conversation) context.Context {
	return context.WithValue(ctx, conversationKey{}, c)
}

// ConversationFrom returns the conversation set by WithConversation.
func ConversationFrom(ctx context.Context) Conversation {
	c, _ := ctx.Value(conversationKey{}).(Conversation)
	return c
}
