package harness

import "context"

// Model is the inference seam: one call to one model. openaiharness is the
// real adapter, harnesstest.FakeModel scripts responses, and
// dbosharness.Model wraps any Model so each call is a durable step.
type Model interface {
	Message(ctx context.Context, req Request) (*Response, error)
	// Provider names the backend, such as "openai" or "deepseek".
	Provider() string
	// Model is the model id sent to the provider.
	Model() string
}

// Request is one model call. The adapter lets the model choose between a
// text reply and tool calls (tool_choice "auto").
type Request struct {
	Messages []Message
	Tools    []ToolDef
	// ReasoningEffort is none|minimal|low|medium|high; empty keeps the
	// model's default. Adapters drop it for providers that reject it.
	ReasoningEffort string
	// NoToolCalls keeps Tools in the request but forbids calling them, so the
	// model must answer in text (tool_choice "none").
	NoToolCalls bool
	// OnDelta hears each chunk of reply text as it streams; may be nil.
	OnDelta func(text string)
	// OnReasoning hears each chunk of reasoning text, for providers that
	// stream it; may be nil.
	OnReasoning func(text string)
	// OnReset is called by a retrying wrapper (dbosharness.Model) before it
	// re-sends a call whose text already streamed, so listeners discard it;
	// may be nil.
	OnReset func()
}

// Response is the model's reply to a Request.
type Response struct {
	Message Message
	Usage   Usage
	// Reasoning is the turn's reasoning text, when the provider returns it.
	// It is shown and logged, never sent back as history.
	Reasoning string
}
