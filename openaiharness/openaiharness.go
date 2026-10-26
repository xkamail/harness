// Package openaiharness is the harness.Model adapter for OpenAI-compatible Chat
// Completions endpoints; provider differences live here as Dialects, never in projects.
package openaiharness

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"

	"github.com/xkamail/harness"
)

type Client struct {
	provider string
	model    string
	baseURL  string
	apiKey   string
	dialect  Dialect
	client   openai.Client
}

var _ harness.Model = (*Client)(nil)

// Dialect is how an endpoint departs from OpenAI's.
type Dialect struct {
	// ReasoningField is the non-standard reasoning delta field (DeepSeek: "reasoning_content"); empty reads none.
	ReasoningField string
	// NoReasoningEffort is for endpoints that reject reasoning_effort.
	NoReasoningEffort bool
	// LegacyMaxTokens sends max_tokens instead of max_completion_tokens.
	LegacyMaxTokens bool
}

// Every dialect sends images as image_url data URIs and PDFs as base64 file parts.
var (
	OpenAI   = Dialect{}
	DeepSeek = Dialect{ReasoningField: "reasoning_content", NoReasoningEffort: true, LegacyMaxTokens: true}
	Ollama   = Dialect{NoReasoningEffort: true}
)

type Option func(*Client)

// WithBaseURL defaults to OpenAI.
func WithBaseURL(url string) Option { return func(c *Client) { c.baseURL = url } }

// WithProvider names the backend in RunResult and usage logs (default "openai").
func WithProvider(name string) Option { return func(c *Client) { c.provider = name } }

func WithDialect(d Dialect) Option { return func(c *Client) { c.dialect = d } }

func New(apiKey, model string, opts ...Option) *Client {
	c := &Client{provider: "openai", model: model, apiKey: apiKey}
	for _, o := range opts {
		o(c)
	}
	// retry policy belongs to callers, via harness.IsRetryable
	ro := []option.RequestOption{option.WithAPIKey(apiKey), option.WithMaxRetries(0), option.WithMiddleware(wrapStringError)}
	if c.baseURL != "" {
		ro = append(ro, option.WithBaseURL(c.baseURL))
	}
	c.client = openai.NewClient(ro...)
	return c
}

// wrapStringError rewrites {"error": "text"} (Ollama, some proxies) into OpenAI's
// {"error": {"message": "text"}}; the SDK fails on the string form and loses the status.
func wrapStringError(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	res, err := next(req)
	if err != nil || res.StatusCode < 400 {
		return res, err
	}
	body, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil {
		return nil, err
	}
	var e struct {
		Error json.RawMessage `json:"error"`
	}
	var text string
	if json.Unmarshal(body, &e) == nil && json.Unmarshal(e.Error, &text) == nil {
		body, _ = json.Marshal(map[string]any{"error": map[string]string{"message": text}})
		res.ContentLength = int64(len(body))
		res.Header.Del("Content-Length")
	}
	res.Body = io.NopCloser(bytes.NewReader(body))
	return res, nil
}

// Error's StatusCode is 0 for a network failure or a stream cut short.
type Error struct {
	StatusCode int
	Message    string
	err        error
}

func (e *Error) Error() string {
	if e.StatusCode == 0 {
		return "openaiharness: " + e.Message
	}
	return fmt.Sprintf("openaiharness: api error %d: %s", e.StatusCode, e.Message)
}

func (e *Error) Unwrap() error { return e.err }

// Retryable reports a rate limit, a 5xx or a network failure.
func (e *Error) Retryable() bool {
	return e.StatusCode == 0 || e.StatusCode == 429 || e.StatusCode >= 500
}

// ContextLength reports a 413, or a 400 whose message says the prompt
// overflowed the context window; providers word it differently.
func (e *Error) ContextLength() bool {
	switch e.StatusCode {
	case 413:
		return true
	case 400:
		msg := strings.ToLower(e.Message)
		for _, s := range []string{"context_length", "context length", "maximum context", "prompt is too long", "input is too long"} {
			if strings.Contains(msg, s) {
				return true
			}
		}
	}
	return false
}

// Message makes one streaming call with no sampling params.
func (c *Client) Message(ctx context.Context, req harness.Request) (*harness.Response, error) {
	params := openai.ChatCompletionNewParams{
		Model:    c.model,
		Messages: make([]openai.ChatCompletionMessageParamUnion, 0, len(req.Messages)),
	}
	// Some providers (Anthropic's compat API) reject an empty tools list, so send neither field.
	if len(req.Tools) > 0 {
		choice := "auto"
		if req.NoToolCalls {
			choice = "none"
		}
		params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: param.NewOpt(choice)}
	}
	for _, m := range req.Messages {
		om, err := toOpenAIMessage(m)
		if err != nil {
			return nil, err
		}
		params.Messages = append(params.Messages, om)
	}
	for _, t := range req.Tools {
		fn := shared.FunctionDefinitionParam{
			Name:        t.Name,
			Description: param.NewOpt(t.Description),
		}
		if len(t.Schema) > 0 {
			if err := json.Unmarshal(t.Schema, &fn.Parameters); err != nil {
				return nil, fmt.Errorf("openaiharness: tool %q schema: %w", t.Name, err)
			}
		}
		params.Tools = append(params.Tools, openai.ChatCompletionFunctionTool(fn))
	}
	if req.ReasoningEffort != "" && !c.dialect.NoReasoningEffort {
		params.ReasoningEffort = shared.ReasoningEffort(req.ReasoningEffort)
	}
	if req.MaxOutputTokens > 0 {
		if c.dialect.LegacyMaxTokens {
			params.MaxTokens = param.NewOpt(req.MaxOutputTokens)
		} else {
			params.MaxCompletionTokens = param.NewOpt(req.MaxOutputTokens)
		}
	}
	params.StreamOptions.IncludeUsage = param.NewOpt(true)

	stream := c.client.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()
	var (
		acc       openai.ChatCompletionAccumulator
		reasoning strings.Builder
	)
	for stream.Next() {
		chunk := stream.Current()
		if !acc.AddChunk(chunk) {
			return nil, errors.New("openaiharness: inconsistent stream chunk")
		}
		// the final usage-only chunk has no choices
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if req.OnDelta != nil && delta.Content != "" {
			req.OnDelta(delta.Content)
		}
		if text := c.reasoningDelta(delta); text != "" {
			reasoning.WriteString(text)
			if req.OnReasoning != nil {
				req.OnReasoning(text)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return nil, classify(ctx, err)
	}
	if len(acc.Choices) == 0 {
		return nil, errors.New("openaiharness: response has no choices")
	}
	// a dropped connection ends the stream cleanly with a partial message
	if acc.Choices[0].FinishReason == "" {
		return nil, &Error{Message: "stream ended before the reply finished"}
	}

	msg := acc.Choices[0].Message
	out := &harness.Response{
		Message: harness.Message{
			Role:      harness.RoleAssistant,
			Content:   msg.Content,
			ToolCalls: make([]harness.ToolCall, 0, len(msg.ToolCalls)),
		},
		Usage: harness.Usage{
			InputTokens:  acc.Usage.PromptTokens,
			CachedTokens: acc.Usage.PromptTokensDetails.CachedTokens,
			OutputTokens: acc.Usage.CompletionTokens,
		},
		Reasoning: reasoning.String(),
	}
	for _, tc := range msg.ToolCalls {
		// some backends (older Ollama) omit type; treat it as function
		if tc.Type != "" && tc.Type != "function" {
			continue
		}
		out.Message.ToolCalls = append(out.Message.ToolCalls, harness.ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: json.RawMessage(tc.Function.Arguments),
		})
	}
	return out, nil
}

func (c *Client) reasoningDelta(d openai.ChatCompletionChunkChoiceDelta) string {
	if c.dialect.ReasoningField == "" {
		return ""
	}
	f, ok := d.JSON.ExtraFields[c.dialect.ReasoningField]
	if !ok {
		return ""
	}
	var text string
	_ = json.Unmarshal([]byte(f.Raw()), &text) // null when absent
	return text
}

func classify(ctx context.Context, err error) error {
	if apiErr, ok := errors.AsType[*openai.Error](err); ok {
		return &Error{StatusCode: apiErr.StatusCode, Message: apiErr.Message, err: err}
	}
	if ctx.Err() != nil {
		return err
	}
	if _, ok := errors.AsType[net.Error](err); ok || errors.Is(err, io.ErrUnexpectedEOF) {
		return &Error{Message: err.Error(), err: err}
	}
	return err
}

func toOpenAIMessage(m harness.Message) (openai.ChatCompletionMessageParamUnion, error) {
	if len(m.Parts) > 0 && m.Role != harness.RoleUser {
		return openai.ChatCompletionMessageParamUnion{}, errors.New("openaiharness: only user messages may carry parts")
	}
	switch m.Role {
	case harness.RoleSystem:
		return openai.SystemMessage(m.Content), nil
	case harness.RoleUser:
		if len(m.Parts) == 0 {
			return openai.UserMessage(m.Content), nil
		}
		parts, err := toOpenAIParts(m)
		if err != nil {
			return openai.ChatCompletionMessageParamUnion{}, err
		}
		return openai.UserMessage(parts), nil
	case harness.RoleTool:
		return openai.ToolMessage(m.Content, m.ToolCallID), nil
	case harness.RoleAssistant:
		a := openai.ChatCompletionAssistantMessageParam{}
		if m.Content != "" {
			a.Content.OfString = param.NewOpt(m.Content)
		}
		for _, tc := range m.ToolCalls {
			a.ToolCalls = append(a.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID: tc.ID,
					Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name:      tc.Name,
						Arguments: string(tc.Arguments),
					},
				},
			})
		}
		return openai.ChatCompletionMessageParamUnion{OfAssistant: &a}, nil
	default:
		return openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("openaiharness: unknown message role %q", m.Role)
	}
}

func toOpenAIParts(m harness.Message) ([]openai.ChatCompletionContentPartUnionParam, error) {
	out := make([]openai.ChatCompletionContentPartUnionParam, 0, len(m.Parts)+1)
	if m.Content != "" {
		out = append(out, openai.TextContentPart(m.Content))
	}
	for _, p := range m.Parts {
		if p.Type != harness.PartText && len(p.Data) == 0 {
			return nil, fmt.Errorf("openaiharness: attachment %q not loaded", p.AttachmentID)
		}
		switch p.Type {
		case harness.PartText:
			out = append(out, openai.TextContentPart(p.Text))
		case harness.PartImage:
			out = append(out, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
				URL:    dataURL(p.MIME, p.Data),
				Detail: "auto",
			}))
		case harness.PartFile:
			out = append(out, openai.FileContentPart(openai.ChatCompletionContentPartFileFileParam{
				FileData: param.NewOpt(dataURL(p.MIME, p.Data)),
				Filename: param.NewOpt(p.Name),
			}))
		default:
			return nil, fmt.Errorf("openaiharness: unknown part type %q", p.Type)
		}
	}
	return out, nil
}

func dataURL(mimeType string, b []byte) string {
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(b)
}

func (c *Client) Provider() string { return c.provider }
func (c *Client) Model() string    { return c.model }
