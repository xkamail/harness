# Hand-off: migrating mccloud onto harness

Status as of 2026-10-25 (harness `307a372`, mccloud `d773ec1`). Static review
only: nothing below has been built or tested yet. Go is now available on the
machine, so it can be.

**Verdict:** a 100% migration is not possible today. Do it in two phases:

1. **Phase 1, doable now:** replace mccloud `pkg/ai` with `harness` +
   `openaiharness` + `dbosharness`. Two gaps must be filled in harness first.
   No DB changes.
2. **Phase 2, blocked:** replace mccloud `api/ai` (sessions/events, ~8.3k
   lines) with `pgharness`. This needs new pgharness API. No data migration:
   AI conversations and memories start fresh (see below).

---

## Phase 1: `pkg/ai` → harness core

mccloud already imports `pkg/ai` as `harness` in 54 of 57 files, so the import
swap is mostly mechanical.

### 1a. Fill in harness first (blockers)

- [ ] **Sub-agents.** Port from `mccloud/pkg/ai/subagent.go` (158 lines + 330
  lines of tests in `subagent_test.go`):
  - `SubAgentConfig{Name, Description, Label, Agent *Agent, Timeout}` (in
    mccloud the field is `Surface`; default timeout is 2m).
  - `SubAgent(cfg) Tool`: runs the child as a nested run on the parent's
    model, shares the parent's concurrency semaphore, adds child usage to the
    parent's `RunResult.Usage`, and keeps the child's tool calls even when it
    errors.
  - `Agent.Headless bool`: no `ask_user`/`propose`/`plan`, no gated tools,
    cannot hold sub-agents itself. `Compile` should enforce this and require a
    sub-agent's `Agent` to be headless.
  - `IsSubAgent(ctx) bool`. mccloud uses it in `api/ai/run.go:547` (the model
    step skips `dbos.RunAsStep` for child calls) and in
    `api/ai/agent/console/console.go:192`. **`dbosharness.Model` needs the
    same check**, or child calls become DBOS steps inside a tool step.
  - mccloud also has a "sub calls" slot (`subCallsFrom(ctx)`), which
    `stepLLM.onSubCalls` uses to surface child tool calls. Decide whether
    `RunConfig.OnToolStart` covers this instead.
  - Caller: `mccloud/api/ai/agent/console/investigate.go` (`investigate` tool).
- [ ] **`IsContextLength(err) bool`.** Port from `mccloud/pkg/ai/errors.go:47`:
  413, or 400 with a message containing `context_length`, `context length`,
  `maximum context`, `prompt is too long` or `input is too long`. Put it on
  `openaiharness.Error` (or as a top-level helper using an interface, like
  `IsRetryable`). Caller: `api/ai/run.go:403`. Bring over
  `pkg/ai/errors_test.go`.

### 1b. Rename map for mccloud

| mccloud `pkg/ai` | harness |
| --- | --- |
| `SurfaceConfig` | `Agent` |
| `NewSurface(cfg)` → `*Surface` | `Compile(a)` → `*Agent` (or `MustCompile`) |
| `harness.Run(ctx, m, s, cfg)` | `a.Run(ctx, m, cfg)` |
| `const platformPrompt` (`pkg/ai/surface.go:60`) | `Agent.PlatformPrompt` (set on every mccloud agent) |
| `RunResult.InputTokens` / `OutputTokens` | `RunResult.Usage.InputTokens` / `.OutputTokens` |
| `RunConfig.OnDelta func(text)` | `OnDelta func(messageID, text string)` |
| `harness.New(baseURL, key, model)` → `*Client` | `openaiharness.New(key, model, openaiharness.WithBaseURL(baseURL))` |
| `*APIError` | `*openaiharness.Error` |
| `MaxAttachments`, `MaxTextAttachmentSize`, `MaxBinaryAttachmentSize` | `harness.DefaultAttachmentLimits` |
| `ValidateParts(parts)` | `ValidateParts(parts, lim)` |
| `messager` interface (`pkg/tu/fakellm.go`) | `harness.Model` (same three methods) |
| `AwaitingConfirmation` (7 uses) | no equivalent, by design (see below) |
| `Registry` / `NewRegistry` | gone (inside `Compile`); mccloud only uses it in `pkg/ai` tests |

`AwaitingConfirmation` is not exported because harness has no placeholder
result to compare against. mccloud answers a held call with that string, but
harness writes no tool message for it: `RunResult.Messages` ends at the
assistant turn's other results, and the held call comes back as
`RunResult.Confirm` with `Stop == StopConfirm`. `OnToolDone` still fires for it
with `ToolOutcome.Held` (`agent.tool_done` carries `held: true`). Rework the
call sites:

- `api/ai/run.go:649` (grant replay): held calls emit no tool-result event, so
  drop the `AwaitingConfirmation` comparison. Keep the `{"error":"skipped:`
  prefix check, because skipped calls are still answered with a tool error.
- `pkg/ai/gate_test.go` and `api/ai/console*_test.go`: replace
  `Content == AwaitingConfirmation` with `res.Stop == StopConfirm` plus
  `res.Confirm.ID`. Where a test reads events, assert that the held call has
  no result event (or that its `tool_done` has `held: true`).

Same names, still check they behave the same: `Tool` (harness adds `EndsTurn`,
`Block`), `Tier*`, `Grants`, `Inbox`, `ToolRunner`, `CallKey`,
`ToolPropose/AskUser/Plan`, `Role*`, `Part*`, `Usage`.

### 1c. Swap the durable wrappers in `api/ai/run.go`

- `stepLLM` → `dbosharness.Model(wctx, m, WithRetry, WithOnUsage, WithOnCalls)`.
  `WithOnUsage` writes `ai_usage`; `WithOnCalls` replaces `onSubCalls`.
- `stepTools` (`run.go:750`) → `dbosharness.Tools(wctx)`.
- `stepInbox` (`run.go:614`) → `dbosharness.Inbox(wctx, read)`. mccloud's
  inbox also tracks `injected *[]queuedMessage`, so keep that in the `read`
  func.
- `FailOrphanedRuns` → `dbosharness.FailOrphanedRuns` with an `OrphanSweeper`
  over `ai_sessions`.

### 1d. Finish

- [ ] Delete `mccloud/pkg/ai`. Add `github.com/xkamail/harness` to mccloud
  `go.mod` (tag a harness version first, or use a `replace` while iterating).
- [ ] `go build ./... && go vet ./... && go test ./...` in mccloud (needs
  `TEST_DB_URL`, `TEST_REDIS_URL`).
- [ ] Run **both** evals. Gate/plan behavior changed in harness (e.g.
  `307a372`: an approved plan runs its steps without asking again):
  - `go test -tags eval -run TestDeployEval ./api/ai/agent/deploy/`
  - `go test -tags eval -run TestConsoleEval -parallel 8 ./api/ai/agent/console/`
- [ ] Update `mccloud/CLAUDE.md` and `docs/agentic-spec.md` (`pkg/ai` →
  harness).

---

## Phase 2: `api/ai` → pgharness (blocked)

### Blockers in pgharness

1. **No custom persisted events.** `appendEvents` is unexported. Only `Live`
   is public, and it is ephemeral. mccloud persists and the web client reads
   these event types, which harness doesn't have:
   `agent.watch`, `agent.watch_ended`, `agent.todos`, `agent.inbox`,
   `session.title`, `user.unqueued`.
   → Add `pgharness.Append(ctx, bus, conversationID, ev...)` for app-defined
   types. `Load` already skips unknown types when rebuilding history.
2. **Missing lifecycle ops** that mccloud exposes over arpc:
   - `Unqueue` (drop a `user.queued` message)
   - `Retry` (re-run after `error`)
   - `List` (owner's conversations by `updated_at`)
   - soft delete (`deleted_at`; pgharness only hard-deletes)
   - `SetTitle` (mccloud makes LLM titles in `api/ai/title.go`; pgharness
     truncates the first message to 80 runes)

   The queue cap already exists (`ErrQueueFull`).
3. **Per-turn persistence** is still an open question in the pgharness README.
   mccloud has the same gap today, so this doesn't block the migration.

### Stays in mccloud (project concerns, not harness)

Stream tickets (`ai_stream_tickets`), watches (`ai_watches` + DBOS
workflows), usage/pricing (`ai_usage`, via `dbosharness.WithOnUsage`), guest
limits, rate limits, and who may approve.

### Event names: harness is the source of truth

Where mccloud and harness disagree, **use the harness names and payloads**.
mccloud's `api/ai` and `web/` change; harness does not.

| mccloud (remove) | harness (use) |
| --- | --- |
| `agent.tools_start` / `agent.tools_done` (one per turn) | `agent.tool_start` / `agent.tool_done` (one per call, keyed by `toolCallId`; replayed durable turns repeat them, so dedupe) |
| `agent.summary` | `session.summary` (`SummaryPayload`) |
| mccloud payload structs for shared types | `harness.*Payload` / `pgharness.*Payload` |
| ids inside payloads | `run_id` / `message_id` / `tool_call_id` on the event itself |

- [ ] Delete mccloud's own constants for event types harness already defines
  (`api/ai` `event*` consts), and use `harness.Event*`.
- [ ] Rewrite the `web/app` stream parser and TS types against harness
  `Event` + payload JSON (`grep -rn '"agent\.\|"session\.\|"user\.' web/app`).
  Also handle `step.reset` (drop the message's streamed text) and
  `stream.ready`.
- [ ] Event types harness does **not** define stay mccloud-owned, and are
  persisted through the new `pgharness.Append`: `agent.watch`,
  `agent.watch_ended`, `agent.todos`, `session.title`. Keep their names in
  the same `agent.`/`session.` style. Two of them may not need app-defined
  types at all:
  - `agent.inbox`: check whether harness already records injected messages
    (`user.message` delivered from `user.queued`).
  - `user.unqueued`: if `Unqueue` lands in pgharness, it should own this type
    as `harness.EventUserUnqueued`.

### Fresh start: no data migration

Existing AI data is **not** carried over. Users start with no conversations
and no memories after the switch.

- [ ] Delete the AI tables from `mccloud/schema/0004.sql` (`ai_sessions`,
  `ai_session_events`, `ai_session_attachments`, `ai_memories`). Following
  mccloud's "edit `CREATE TABLE`, no `ALTER`" rule, remove the
  `create table` statements and add `drop table if exists … cascade` for
  databases that already have them. `ai_watches` and `ai_stream_tickets`
  reference `ai_sessions`, so point them at `harness.conversations(id)` or
  drop their FK. `ai_usage.session_id` → plain `varchar(20)`, no FK.
- [ ] Call `pgharness.Migrate(ctx)` at startup next to `schema.Migrate`, in
  `cmd/api`, `cmd/scheduler` and `pkg/tu` (the test harness).
- [ ] Requires `pg_trgm` on the production DB (`pgharness` creates the
  extension, so the DB user needs permission).
- [ ] Context mapping for new conversations: `owner_id = account_id`,
  `agent = surface`, `context = {"serverId": …}` for console (use
  `PolicyPerContext` if one conversation per server is intended), `{}` for
  deploy. Memories: account-wide = `{}`, per-server = `{"serverId": …}`.
- [ ] Cross-schema FKs are gone. Call `pgharness.DeleteContext` on server
  delete (`{"serverId": id}`) and on account delete. Today
  `on delete cascade` handles both.
- [ ] Ship web and API together. No client needs to read the old event
  format.

---

## Suggested order

1. Add sub-agents and `IsContextLength` to harness, with tests, then tag
   `v0.x`.
2. Do mccloud phase 1 on a branch, then run the tests and evals.
3. Add `pgharness.Append`, `Unqueue`, `Retry`, `List`, `SetTitle` and soft
   delete.
4. Do mccloud phase 2: drop the old `ai_*` tables, rewrite `api/ai` on
   pgharness, then the `web/` event parser. Ship them together.
