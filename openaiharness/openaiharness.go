// Package openaiharness is the harness.Model adapter for any
// OpenAI-compatible Chat Completions endpoint: OpenAI, DeepSeek, OpenRouter,
// Ollama, vLLM, and the compatibility endpoints of other providers.
//
// Provider differences live here as Options, never in projects.
package openaiharness

import (
	"context"

	"github.com/xkamail/harness"
)

// Client is a harness.Model over one endpoint and model.
type Client struct {
	provider string
	model    string
	baseURL  string
	apiKey   string
	dialect  Dialect
}

var _ harness.Model = (*Client)(nil)

// Dialect is how an endpoint departs from OpenAI's.
type Dialect struct {
	// ReasoningField is the non-standard delta field carrying reasoning,
	// such as "reasoning_content" on DeepSeek; empty reads none.
	ReasoningField string
	// NoReasoningEffort drops Request.ReasoningEffort for endpoints that
	// reject it.
	NoReasoningEffort bool
	// NoStreamUsage is for endpoints that ignore stream_options.include_usage;
	// usage is then read from a final non-stream call or left zero.
	NoStreamUsage bool
	// ToolCallIDPattern rewrites harness ids for endpoints that validate the
	// format; empty passes them through.
	ToolCallIDPattern string
}

// Dialects of known endpoints. Images go as image_url data URIs and PDFs as
// file content parts (base64 file_data) on every dialect.
var (
	OpenAI     = Dialect{}
	DeepSeek   = Dialect{ReasoningField: "reasoning_content", NoReasoningEffort: true}
	Ollama     = Dialect{NoReasoningEffort: true}
	OpenRouter = Dialect{}
)

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the client at another endpoint; the default is OpenAI.
func WithBaseURL(url string) Option { return func(c *Client) { c.baseURL = url } }

// WithProvider names the backend in RunResult and usage logs; the default is
// "openai".
func WithProvider(name string) Option { return func(c *Client) { c.provider = name } }

// WithDialect sets the endpoint's differences.
func WithDialect(d Dialect) Option { return func(c *Client) { c.dialect = d } }

// New returns a client for model.
func New(apiKey, model string, opts ...Option) *Client {
	c := &Client{provider: "openai", model: model, apiKey: apiKey}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Message makes one streaming Chat Completions call.
func (c *Client) Message(ctx context.Context, req harness.Request) (*harness.Response, error) {
	// TODO: port from mccloud pkg/ai/openai.go (openai-go/v3), plus
	// Dialect.ReasoningField from verseclash pkg/tournamentagent.
	return nil, harness.ErrNotImplemented
}

func (c *Client) Provider() string { return c.provider }
func (c *Client) Model() string    { return c.model }

// Models resolves model ids to clients, for Agent.Models and Route.
type Models map[string]harness.Model

// Get returns the client for id, or nil.
func (m Models) Get(id string) harness.Model { return m[id] }
