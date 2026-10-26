# harness

A provider-neutral LLM agent loop for Go, meant to be the shared foundation for
the AI agents in every project.

You describe an agent once: its prompt, tools and skills. Each `Run` is one
turn: history goes in, new messages come out, and you decide where they are
stored. Tool approval, plans, sub-agents, budgets and streaming are built in.
The model provider, the storage and durable execution are separate packages
you plug in.

```sh
go get github.com/xkamail/harness@v0.1.0
```

## Packages

| Package | What it does |
| --- | --- |
| [`harness`](.) | The agent loop: `Agent`, `Tool`, `Run`, tool tiers and approvals, plans, skills, sub-agents, compaction, budgets |
| [`openaiharness`](openaiharness) | `harness.Model` for any OpenAI-compatible Chat Completions endpoint, with dialects for OpenAI, DeepSeek and Ollama |
| [`pgharness`](pgharness) | Postgres storage and lifecycle: event log, conversations, attachments, memory, live streaming ([README](pgharness/README.md)) |
| [`dbosharness`](dbosharness) | Durable runs on [DBOS](https://github.com/dbos-inc/dbos-transact-golang): model calls, tool calls, inbox and context reads become checkpointed steps |
| [`harnesstest`](harnesstest) | `FakeModel`, a scripted model for tests |

## Quick start

```go
var agent = harness.MustCompile(harness.Agent{
	Name:         "example",
	SystemPrompt: "You are a terse assistant. Use tools when they help.",
	Tools: []harness.Tool{{
		Name:        "now",
		Description: "Call when you need the current time.",
		Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
			return time.Now().Format(time.RFC3339), nil
		},
	}},
})

func main() {
	m := openaiharness.New(os.Getenv("OPENAI_API_KEY"), "gpt-5-mini")
	history := []harness.Message{{Role: harness.RoleUser, Content: "What time is it?"}}

	res, err := agent.Run(context.Background(), m, harness.RunConfig{
		History: history,
		OnDelta: func(_, text string) { fmt.Print(text) },
	})
	if err != nil {
		log.Fatal(err)
	}
	history = append(history, res.Messages...) // persist these
}
```

Try it in the terminal:

```sh
OPENAI_API_KEY=sk-... go run ./cmd/example -model gpt-5-mini
go run ./cmd/example -base-url http://localhost:11434/v1 -model qwen3 -dialect ollama
go run ./cmd/example -fake   # scripted model, no network
```

[`cmd/sse`](cmd/sse) serves a conversation to a web UI over Server-Sent Events.

## Concepts

**One run per turn.** A run stops when it needs a person, and `RunResult.Stop`
says why:

- `reply`: the model answered in plain text.
- `interaction`: the model called `ask_user`, `propose` or `plan`.
- `confirm`: a gated tool call is waiting for approval.
- `ends_turn`: a tool marked `EndsTurn` ran.
- `budget`: the token or time budget ran out.

To continue, start a new run with the updated history and, if needed, a
`Decision`. Nothing is kept in memory between runs.

**Tool tiers.** Every tool has a tier:

- `TierSafe` runs immediately.
- `TierDisruptive` waits for confirmation, unless an approved plan or a grant
  (`approve_all` for a `GrantGroup`) already covers it.
- `TierDestructive` always waits, and only a decision on that exact call can
  run it.

`Check` vets a call before it is held and returns the preview shown on the
confirmation card.

**Sub-agents.** `harness.SubAgent` turns a compiled agent marked `Headless`
into a tool. The child runs on the parent's model and its reply comes back as
the tool result. Calls in the same turn run in parallel.

**Decorators, not hooks.** Extend an agent with `func(Agent) Agent`, for
example `harness.AddTools` and `harness.WrapHandlers`, then `Compile` it once at
startup.

**Errors.** `harness.IsRetryable(err)` and `harness.IsContextLength(err)`
classify provider errors without depending on any one provider's types.

## Testing

```sh
go test ./...
# pgharness needs Postgres; %s is replaced with a database per test
TEST_DB_URL='postgres://postgres:postgres@localhost:5432/%s?sslmode=disable' go test ./...
```
