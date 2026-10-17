// Package harness is a provider-neutral LLM agent harness: an Agent (prompt
// and tools) compiled once and run as a manual tool-calling loop. One Run is
// one turn: history in, new messages out; the caller persists them.
//
// The core knows nothing about any product, database, queue or transport.
// Projects plug in through three seams:
//
//   - Model: one model call (openaiharness is the adapter; tests use a fake)
//   - ToolRunner: runs one turn's tool calls (dbosharness checkpoints each)
//   - Inbox: messages that arrive mid-run
//
// Conversations, their storage, memory and live streaming belong to
// pgharness. An Agent is extended with decorators, func(Agent) Agent, never
// hooks.
//
// Files:
//
//   - message.go, model.go: neutral messages and the model seam
//   - tools.go: tools, tiers and how calls are shown
//   - agent.go: the Agent, Compile and Run
//   - dispatch.go: one turn's tool calls, gated calls held or granted
//   - run.go: RunConfig, RunResult, grants, callbacks and runner seams
//   - values.go: Values, the context tools read from ctx
//   - event.go: event types and the payloads the harness owns
//   - attachment.go: image and PDF attachments
//   - compaction.go: history compaction
//   - errors.go: sentinels and error classes
//   - decorate.go: helpers for decorators
package harness
