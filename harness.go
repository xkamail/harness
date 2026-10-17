// Package harness is a provider-neutral LLM agent harness: an Agent (prompt,
// tools, policy) compiled once and run as a manual tool-calling loop.
//
// The core knows nothing about any product, database, queue or transport.
// Projects plug in through small interfaces:
//
//   - Model: one model call (openaiharness is the adapter; tests use a fake)
//   - ToolRunner: runs one turn's tool calls (dbosharness checkpoints each)
//   - Inbox: messages that arrive mid-run
//   - Store, Bus: the conversation event log and its live fan-out
//   - MemoryStore: durable memories the agent recalls and saves
//
// An Agent is extended with decorators, func(Agent) Agent, never hooks.
//
// Files:
//
//   - message.go, model.go: neutral messages and the model seam
//   - tools.go: tools, tiers and how calls are shown
//   - agent.go: the Agent, Compile and Run
//   - policy.go: conversation and model policies
//   - run.go: RunConfig, RunResult, grants and the runner seams
//   - conversation.go: principal and conversation
//   - values.go: Values, the conversation context tools read from ctx
//   - event.go: events, Store, Bus and Follow
//   - memory.go: memories and MemoryStore
//   - compaction.go: history compaction
//   - errors.go: sentinels and error classes
//   - decorate.go: helpers for decorators
package harness
