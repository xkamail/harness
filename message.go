package harness

import "encoding/json"

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is a provider-neutral chat message; SDK types never leak into history.
type Message struct {
	// ID is set by Run on assistant messages only; adapters ignore it.
	ID      string `json:"id,omitempty"`
	Role    Role   `json:"role"`
	Content string `json:"content"`
	// Parts: user messages only.
	Parts      []Part     `json:"parts,omitempty"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
	// SubCalls: a sub-agent result's child tool calls, kept when the child fails. Adapters ignore it.
	SubCalls []ToolCall `json:"subCalls,omitempty"`
}

type PartType string

const (
	PartText  PartType = "text"
	PartImage PartType = "image"
	PartFile  PartType = "file" // application/pdf
)

// Part is user-message content. Attachment parts from history have
// AttachmentID but no Data until the loader fills it; tools take the ID, never bytes.
type Part struct {
	Type         PartType `json:"type"`
	Text         string   `json:"text,omitempty"`         // PartText
	MIME         string   `json:"mime,omitempty"`         // PartImage, PartFile
	Name         string   `json:"name,omitempty"`         // PartImage, PartFile
	AttachmentID string   `json:"attachmentId,omitempty"` // PartImage, PartFile
	Data         []byte   `json:"data,omitempty"`         // PartImage, PartFile
}

// ToolCall.ID is the provider's call id, passed through unchanged.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// Usage is provider-reported token usage, never estimated.
type Usage struct {
	InputTokens int64 `json:"inputTokens"`
	// CachedTokens is a subset of InputTokens.
	CachedTokens int64 `json:"cachedTokens"`
	OutputTokens int64 `json:"outputTokens"`
}

func (u Usage) Add(v Usage) Usage {
	return Usage{
		InputTokens:  u.InputTokens + v.InputTokens,
		CachedTokens: u.CachedTokens + v.CachedTokens,
		OutputTokens: u.OutputTokens + v.OutputTokens,
	}
}
