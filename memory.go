package harness

import (
	"context"
	"time"
)

// MemoryKind is what a memory records.
type MemoryKind string

// Memory kinds.
const (
	// MemoryPreference is how someone wants things done.
	MemoryPreference MemoryKind = "preference"
	// MemoryFact is what a subject is or is for.
	MemoryFact MemoryKind = "fact"
	// MemoryIncident is what broke and how it was fixed.
	MemoryIncident MemoryKind = "incident"
	// MemoryCase is a closed conversation's summary: symptom, cause, fix.
	MemoryCase MemoryKind = "case"
)

// Memory is one short, durable fact. Memories are context, not
// instructions: live tool results win when they disagree.
type Memory struct {
	ID      string     `json:"id"`
	OwnerID string     `json:"ownerId"`
	Kind    MemoryKind `json:"kind"`
	Content string     `json:"content"`
	// Context is what it is about, such as {"serverId": "…"}; empty means
	// owner-wide. It is visible in conversations whose Context contains it.
	Context Values `json:"context"`
	// Pinned memories are loaded on every run; the rest only by Search.
	Pinned    bool      `json:"pinned"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// MemoryQuery selects memories. Results never cross OwnerID and only
// include memories whose Context the query's Context contains (owner-wide
// ones always); implementations filter in the query, not after.
type MemoryQuery struct {
	OwnerID string
	// Context is the conversation's.
	Context Values
	Kinds   []MemoryKind
	Limit   int
}

// MemoryStore keeps memories. Projects own the table; harnesstest.MemoryStore
// checks an implementation.
type MemoryStore interface {
	// Pinned returns pinned and scoped memories, newest first.
	Pinned(ctx context.Context, q MemoryQuery) ([]Memory, error)
	// Search returns the memories most relevant to text. Implementations
	// may use trigram, full-text or vector search, or a hybrid.
	Search(ctx context.Context, q MemoryQuery, text string) ([]Memory, error)
	// Save creates m, or updates it when m.ID is set and belongs to
	// m.OwnerID.
	Save(ctx context.Context, m Memory) (Memory, error)
	// Forget deletes one of ownerID's memories.
	Forget(ctx context.Context, ownerID, id string) error
}
