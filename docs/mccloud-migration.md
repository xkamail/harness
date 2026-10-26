# Hand-off: migrating mccloud onto harness

Status as of 2026-10-26: harness `v0.2.0`, mccloud `d773ec1`
(not started). `go vet ./...` and `go test ./...` pass on every harness
package, including `pgharness` against Postgres.

**Harness side is done.** Every gap found in the 2026-10-25 review has
landed (table below). What remains is mccloud work only, in two phases:

1. **Phase 1:** replace mccloud `pkg/ai` with `harness` + `openaiharness` +
   `dbosharness`. No DB changes.
2. **Phase 2:** replace mccloud `api/ai` (sessions/events, ~8.3k lines) with
   `pgharness`. No data migration: AI conversations and memories start fresh
   (see below).

Use `github.com/xkamail/harness v0.2.0` in mccloud `go.mod`. `v0.1.0` is too
old: it lacks `Unqueue` and `Append`.

| Gap | Landed in |
| --- | --- |
| `SubAgent`, `SubAgentConfig`, `Agent.Headless`, `IsSubAgent` | `8035bfc` |
| `Message.SubCalls` (child tool calls), persisted on `agent.tool_result` | `367cde0` |
| `dbosharness.Model` skips `RunAsStep` for sub-agent calls | `5cfcfa2` |
| `IsContextLength` | `31dc181` |
| `AwaitingConfirmation`: not exported, by design | `5d21fb9` |
| `pgharness.Retry` | `27939d8` |
| `pgharness.SetTitle` | `32b906b` |
| `pgharness.Delete` (soft delete, `deleted_at`) | `576a902` |
| `pgharness.List` | `6771b76` |
| `pgharness.Unqueue` + `harness.EventUserUnqueued` | `050deef` |
| `pgharness.Append` for app-defined persisted events | `567c48d` |

---

## Phase 1: `pkg/ai` → harness core

mccloud already imports `pkg/ai` as `harness` in 54 of 57 files, so the import
swap is mostly mechanical.

### 1a. Harness APIs that replace mccloud code

- **Sub-agents:** `harness.SubAgent(harness.SubAgentConfig{Name,
  Description, Label, Agent, Timeout})`. `Agent` must be compiled with
  `Headless: true`; `Compile` rejects a headless agent with a proposal
  schema or gated tools. Caller:
  `mccloud/api/ai/agent/console/investigate.go` (`Surface:` becomes
  `Agent:`, and `NewSurface` becomes `Compile`).
- **`harness.IsSubAgent(ctx)`:** same meaning as mccloud's. With
  `dbosharness.Model` there is no need to check it in `run.go`, because the
  wrapper already skips the DBOS step for child calls. Keep the check in
  `console/console.go:192`.
- **Child tool calls:** mccloud's `subCallsFrom(ctx)` slot and
  `stepLLM.onSubCalls` are replaced by `Message.SubCalls` on the tool
  result. pgharness persists it.
- **`harness.IsContextLength(err)`:** same check as
  `mccloud/pkg/ai/errors.go:47`, for errors that implement `ContextLength()`
  (`openaiharness.Error` does). Caller: `api/ai/run.go:403`.

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
  `WithOnUsage` writes `ai_usage`. Sub-agent calls are not checkpointed.
- `stepTools` (`run.go:750`) → `dbosharness.Tools(wctx)`.
- `stepInbox` (`run.go:614`) → `dbosharness.Inbox(wctx, read)`. mccloud's
  inbox also tracks `injected *[]queuedMessage`, so keep that in the `read`
  func.
- `FailOrphanedRuns` → `dbosharness.FailOrphanedRuns` with an `OrphanSweeper`
  over `ai_sessions`.

### 1d. Finish

- [ ] Delete `mccloud/pkg/ai`. Add `github.com/xkamail/harness v0.2.0` to
  mccloud `go.mod` (or use a `replace` to a local checkout while iterating).
- [ ] `go build ./... && go vet ./... && go test ./...` in mccloud (needs
  `TEST_DB_URL`, `TEST_REDIS_URL`).
- [ ] Run **both** evals. Gate/plan behavior changed in harness (e.g.
  `307a372`: an approved plan runs its steps without asking again):
  - `go test -tags eval -run TestDeployEval ./api/ai/agent/deploy/`
  - `go test -tags eval -run TestConsoleEval -parallel 8 ./api/ai/agent/console/`
- [ ] Update `mccloud/CLAUDE.md` and `docs/agentic-spec.md` (`pkg/ai` →
  harness).

---

## Phase 2: `api/ai` → pgharness

### pgharness APIs mccloud maps onto

| mccloud `api/ai` | pgharness |
| --- | --- |
| `Start` / `Send` / `Stop` | `Start` / `Send` / `Stop` (then kick the returned run id as a DBOS workflow) |
| `Retry` | `Retry` |
| `Unqueue` | `Unqueue(ctx, bus, convID, queuedSeq)` (records `user.unqueued`) |
| `List` | `List(ctx, ownerID, agent, limit)` |
| `Delete` (soft) | `Delete` (soft; `DeleteContext` purges) |
| LLM title (`api/ai/title.go`) | keep generating it in mccloud, store it with `SetTitle` |
| `Events` / SSE stream | `Events` + `Follow` over a `Bus` (Redis) |
| `ErrQueueFull` | already in pgharness |
| `agent.watch`, `agent.watch_ended`, `agent.todos`, `session.title` | `Append` (mccloud-owned types) |
| summary/compaction (`api/ai/summary.go`) | `pgharness.Compact` + `Agent.Compaction` |
| `ai_memories` + memory tools | `pgharness.WithMemory` |
| `FailOrphanedRuns` | `pgharness.FailOrphanedRuns` |

Still open, not blocking: **per-turn persistence** (pgharness README). A
client that reconnects mid-run sees no tool progress until the run ends.
mccloud has the same gap today.

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
- [ ] Event types harness does **not** define stay mccloud-owned and are
  persisted with `pgharness.Append`: `agent.watch`, `agent.watch_ended`,
  `agent.todos`, `session.title`. Keep the `agent.`/`session.` naming style.
  `Load` skips unknown types, so they never reach the model's history.
- [ ] Drop `agent.inbox`. pgharness delivers inbox-taken `user.queued`
  messages as `user.message` events, so web renders them like any other user
  message.
- [ ] `user.unqueued` is now harness-owned (`harness.EventUserUnqueued`,
  `pgharness.UnqueuedPayload{QueuedSeq}`).

### Fresh start: no data migration

Existing AI data is **not** carried over. Users start with no conversations
and no memories after the switch.

- [ ] Delete the AI tables from `mccloud/schema/0004.sql` (`ai_sessions`,
  `ai_session_events`, `ai_session_attachments`, `ai_memories`). Following
  mccloud's "edit `CREATE TABLE`, no `ALTER`" rule, remove the
  `create table` statements and add `drop table if exists … cascade` for
  databases that already have them. `ai_watches` and `ai_stream_tickets`
  reference `ai_sessions`. Drop that FK and keep a plain `varchar(20)`
  conversation id, because pgharness tables and project tables never
  reference each other. `ai_usage.session_id` → plain `varchar(20)`, no FK.
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

1. ~~Add the missing harness APIs, with tests, and tag a version.~~ Done:
   `v0.2.0`.
2. Do mccloud phase 1 on a branch, then run the tests and both evals.
3. Do mccloud phase 2: drop the old `ai_*` tables, rewrite `api/ai` on
   pgharness, then the `web/` event parser. Ship them together.
