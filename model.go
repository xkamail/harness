package harness

import "context"

// Model is the inference seam: one call to one model.
type Model interface {
	Message(ctx context.Context, req Request) (*Response, error)
	Provider() string // e.g. "openai", "deepseek"
	Model() string
}

// Request is one model call (tool_choice "auto" unless NoToolCalls).
type Request struct {
	Messages []Message
	Tools    []ToolDef
	// ReasoningEffort is none|minimal|low|medium|high; empty = model default.
	ReasoningEffort string
	// NoToolCalls keeps Tools but forbids calling them (tool_choice "none").
	NoToolCalls bool
	// MaxOutputTokens: zero keeps the provider's limit.
	MaxOutputTokens int64
	// Stream callbacks; each may be nil. None may be called after Message returns.
	OnDelta     func(text string)
	OnReasoning func(text string)
	// OnReset is called before a retry re-sends a call whose text already
	// streamed, so listeners discard it.
	OnReset func()
}

type Response struct {
	Message Message
	Usage   Usage
	// Reasoning is shown and logged, never sent back as history.
	Reasoning string
}
