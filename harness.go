// Package harness is a provider-neutral LLM agent loop: an Agent (prompt and
// tools) is compiled once; each Run is one turn, history in, new messages out,
// and the caller persists them. Projects plug in via Model, ToolRunner and
// Inbox; storage and streaming live in pgharness. Extend an Agent with
// decorators (func(Agent) Agent), not hooks.
package harness
