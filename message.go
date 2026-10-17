package harness

import "encoding/json"

// Role is the author of a Message.
type Role string

// Message roles.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one provider-neutral chat message. SDK types never appear in
// history, events or results, so a conversation can move between models.
type Message struct {
	// ID is set by Run on assistant messages (MessageID) and kept by the
	// caller on the agent.message event; adapters ignore it.
	ID      string `json:"id,omitempty"`
	Role    Role   `json:"role"`
	Content string `json:"content"`
	// Parts is user content beyond plain text; only user messages carry it.
	Parts      []Part     `json:"parts,omitempty"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
}

// PartType is the kind of a message Part.
type PartType string

// Part types. Attachments are images and PDFs only (see attachment.go).
const (
	PartText  PartType = "text"
	PartImage PartType = "image"
	PartFile  PartType = "file" // application/pdf
)

// Part is one piece of user-message content. An image or PDF part read from
// history carries AttachmentID and no Data until the loader fills it; tools
// take the AttachmentID, never bytes, and resolve it within the
// conversation.
type Part struct {
	Type         PartType `json:"type"`
	Text         string   `json:"text,omitempty"`         // PartText
	MIME         string   `json:"mime,omitempty"`         // PartImage, PartFile
	Name         string   `json:"name,omitempty"`         // PartImage, PartFile
	AttachmentID string   `json:"attachmentId,omitempty"` // PartImage, PartFile
	Data         []byte   `json:"data,omitempty"`         // PartImage, PartFile
}

// ToolCall is a model request to invoke a tool. ID is the id the provider
// gave the call; adapters pass it through unchanged.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolDef describes a tool to the model.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// Usage is the exact token usage reported by the provider, never estimated.
type Usage struct {
	InputTokens int64 `json:"inputTokens"`
	// CachedTokens is the part of InputTokens served from the provider cache.
	CachedTokens int64 `json:"cachedTokens"`
	OutputTokens int64 `json:"outputTokens"`
}

// Add returns u plus v.
func (u Usage) Add(v Usage) Usage {
	return Usage{
		InputTokens:  u.InputTokens + v.InputTokens,
		CachedTokens: u.CachedTokens + v.CachedTokens,
		OutputTokens: u.OutputTokens + v.OutputTokens,
	}
}
