# pgharness

Postgres storage and lifecycle for harness conversations: the event log, the
conversation row, attachments, memory and live streaming. The core
(`harness`) runs one turn; `pgharness` turns those turns into a conversation
that survives restarts, scales across stateless instances and streams to any
transport.

> Status: implemented (see Not yet for pgvector). Tested against Postgres with
> `TEST_DB_URL=postgres://…/%s go test ./pgharness`.

## Principles

- **Concrete, not interfaces.** Every project is Postgres with
  `acoshift/pgsql` + `pgctx`, so functions read the DB from `ctx`. That also
  puts every write in the caller's transaction when there is one
  (`pgctx.RunInTx`). The only interfaces are where real alternatives exist:
  `Bus` (Redis per connection, a pod-level hub, in-memory for tests) and
  `Blobs` (bytea, GCS, S3).
- **pgharness owns its schema.** Tables live in the Postgres schema
  `harness`, created by `pgharness.Migrate`, the way DBOS owns `dbos`. Project
  tables stay in `public`; the two never reference each other.
- **The log is the source of truth.** History, grants, the open approval and
  the inbox cursor are all rebuilt from `harness.events` on every run. No
  state lives in memory between runs.
- **One run per turn.** A run ends whenever it waits for a person (reply,
  question, proposal, plan, confirmation). Continuing is a new run with a new
  `run_id`; nothing stays parked.

## Dependencies

| Package | Why |
| --- | --- |
| `github.com/xkamail/harness` | `Agent`, `Message`, `Event`, `Values`, payloads |
| `github.com/acoshift/pgsql`, `pgsql/pgctx` | DB from `ctx`, transactions |
| `github.com/rs/xid` | conversation, run and attachment ids |

No DBOS, Redis or HTTP import. Durability comes from the caller wrapping the
run with `dbosharness`; fan-out comes from the caller's `Bus`.

## Schema

`Migrate` runs numbered, embedded files in order and records them in
`harness.schema_version`. Migrations are additive only within v0: add a
column or index, never drop or retype one.

```sql
create schema if not exists harness;

create table if not exists harness.schema_version (
    version    int primary key,
    applied_at timestamptz not null default now()
);

create table if not exists harness.conversations (
    id             text primary key,
    agent          text        not null,
    owner_id       text        not null,
    context        jsonb       not null default '{}',  -- harness.Values, flat strings
    title          text        not null default '',
    status         text        not null default 'idle'
        check (status in ('idle', 'running', 'awaiting_input', 'error')),
    run_id         text,                               -- current or last run; fences writes
    model          text        not null default '',
    context_tokens bigint      not null default 0,
    created_at     timestamptz not null default now(),
    updated_at     timestamptz not null default now(),
    deleted_at     timestamptz                         -- soft delete; see Delete
);
create index if not exists conversations_owner_idx
    on harness.conversations (owner_id, agent, updated_at desc);
create index if not exists conversations_context_idx
    on harness.conversations using gin (context jsonb_path_ops);

create table if not exists harness.events (
    conversation_id text        not null references harness.conversations (id) on delete cascade,
    seq             bigint      not null,
    type            text        not null,
    run_id          text,
    message_id      text,
    tool_call_id    text,
    payload         jsonb       not null,
    created_at      timestamptz not null default now(),
    primary key (conversation_id, seq)
);

create table if not exists harness.attachments (
    id              text primary key,
    conversation_id text        not null references harness.conversations (id) on delete cascade,
    type            text        not null check (type in ('image', 'file')),
    mime            text        not null,
    name            text        not null default '',
    size            int         not null check (size > 0),
    sha256          text        not null,
    data            bytea,                              -- null when Blobs stores it elsewhere
    created_at      timestamptz not null default now()
);
create index if not exists attachments_conversation_idx
    on harness.attachments (conversation_id);

create table if not exists harness.memories (
    id         text primary key,
    owner_id   text        not null,
    kind       text        not null check (kind in ('preference', 'fact', 'incident')),
    content    text        not null,
    context    jsonb       not null default '{}',      -- visible where conversation.context @> this
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now()
);
create index if not exists memories_owner_idx
    on harness.memories (owner_id, updated_at desc);
```

`seq` is assigned as `coalesce(max(seq), 0) + 1` under the conversation row
lock (`select … for update`), which every writer already takes.

## Context (`harness.Values`)

A conversation's `context` is set once at `Start` and never changes. It does
three jobs, all by plain jsonb operators:

| Job | SQL |
| --- | --- |
| Find the conversation to resume (`PerContext`) | `agent = $1 and owner_id = $2 and context = $3` |
| Delete a subject and everything under it | `context @> $1` (deleting `{"tournamentId": x}` takes its matches) |
| Memories visible to a conversation | `$conversation_context @> memory.context` (`{}` is owner-wide) |

`Load` puts the context in the run's `ctx` (`harness.WithValues`) so tools
read it with `harness.Value`. Per-message values, such as the page the user
is on, ride the `user.message` payload and are layered on for that run only.

## Lifecycle API

```go
// Opening and continuing (HTTP handlers)
func Start(ctx context.Context, bus Bus, p StartParams) (*Conversation, string, error) // runID to kick, or ""
func Send(ctx context.Context, bus Bus, p SendParams) (string, error)                  // runID to kick, or "" when queued
func Stop(ctx context.Context, bus Bus, conversationID string) (runID string, err error)
func Retry(ctx context.Context, bus Bus, conversationID string) (runID string, err error) // re-run after error, or ""
func Unqueue(ctx context.Context, bus Bus, conversationID string, queuedSeq int64) error // drop a pending user.queued
func Get(ctx context.Context, id string) (*Conversation, error)
func List(ctx context.Context, ownerID, agent string, limit int) ([]Conversation, error) // newest updated_at first; skips deleted
func SetTitle(ctx context.Context, conversationID, title string) error // e.g. an LLM-generated title
func Delete(ctx context.Context, conversationID string) error                          // soft delete; reads as ErrNotFound after
func Append(ctx context.Context, bus Bus, conversationID string, evs ...harness.Event) ([]harness.Event, error) // app-defined persisted events; Load skips them

// Inside the run (workflow)
func Load(ctx context.Context, conversationID, runID string) (*Loaded, error)
func Live(ctx context.Context, bus Bus, conversationID, runID string, typ harness.EventType, messageID, toolCallID string, payload any)
func Finish(ctx context.Context, bus Bus, a *harness.Agent, conversationID, runID string, res *harness.RunResult, runErr error) (next string, err error)

// Reading and housekeeping
func Events(ctx context.Context, conversationID string, after int64, limit int) ([]harness.Event, error)
func FailOrphanedRuns(ctx context.Context, bus Bus, idle time.Duration, alive func(runID string) bool) error
func DeleteContext(ctx context.Context, v harness.Values) error
```

The project keeps auth, rate limits and who may approve; it calls these after
its own checks and then kicks the run itself (DBOS workflow, or a goroutine
in tests). Writers take the `Bus` and publish after their transaction
commits (`pgctx.Committed`); a nil `Bus` publishes nothing and `Follow`
heals on its next tick.

A run, end to end:

```go
l, err := pgharness.Load(ctx, convID, runID) // ErrSuperseded: stopped, skip
res, runErr := agent.Run(l.Ctx, model, harness.RunConfig{
    RunID:    runID,
    History:  l.History,
    Grants:   l.Grants,
    Decision: l.Decision,
    Inbox:    l.Inbox,
    OnDelta: func(id, text string) {
        pgharness.Live(ctx, bus, convID, runID, harness.EventDelta, id, "", harness.DeltaPayload{Text: text})
    },
    OnToolStart: func(id string, call harness.ToolCall, d harness.ToolDisplay) {
        pgharness.Live(ctx, bus, convID, runID, harness.EventToolStart, id, call.ID,
            harness.ToolStartPayload{Name: call.Name, Display: d})
    },
})
next, err := pgharness.Finish(ctx, bus, agent, convID, runID, res, runErr)
// next != "": messages queued too late for the inbox; kick that run too
```

### Start

```go
type StartParams struct {
    Agent    string
    OwnerID  string
    Context  harness.Values
    Require  []string        // context keys that must be set; else ErrContext
    Policy   Policy          // PolicyNew | PolicyPerContext | PolicyShared
    Model    string          // pinned on the conversation
    Message  string
    Parts    []harness.Part  // validated with harness.ValidateParts
    Values   harness.Values  // per-message values (page)
}
```

`PolicyNew` always inserts. `PolicyPerContext` resumes the owner's
conversation with the same agent and context, else inserts. `PolicyShared`
resumes the one conversation for the agent and context whatever the owner.
In one transaction: insert or lock the row, store attachments, append
`user.message` and `session.status{running}`, set `run_id`. Returns the run
id to kick, or `""` when the resumed conversation was busy and the message
was queued.

The title is the first message, whitespace collapsed and cut to 80 runes.
`SetTitle` replaces it (normalized the same way) without touching
`updated_at`.

### Send

```go
type SendParams struct {
    ConversationID string
    Message        string
    Parts          []harness.Part
    Values         harness.Values
    Decision       *harness.DecisionPayload  // answers the open confirm or plan
}
```

Locks the row (`for update`) and branches on status:

| Status | Message | Decision |
| --- | --- | --- |
| `running` | append `user.queued` (cap per conversation, `ErrQueueFull`); return `""` | `ErrBusy` |
| `idle`, `error` | append `user.message`, deliver pending queued messages, start a run | `ErrStaleApproval` (nothing open) |
| `awaiting_input` | append `user.message` (the user moved on: the open approval lapses) | `ApprovalID` must name the open `agent.confirm` or `agent.plan`, else `ErrStaleApproval`; append `user.decision`, start a run |

Starting a run is always: append `session.status{running}` with the new
`run_id`, update the row, publish after commit.

### Unqueue

Drops a pending `user.queued` by its seq and appends
`user.unqueued{queuedSeq}`; `Inbox` and `deliverPending` skip it. Returns
`ErrNotQueued` if it was already delivered, dropped or never queued. A
message the running run's inbox already read still reaches the model:
`Finish` records its `user.message` after the `user.unqueued`.

### Load

```go
type Loaded struct {
    Conversation Conversation
    Ctx          context.Context          // ctx with conversation and message Values
    History      []harness.Message
    Grants       harness.Grants
    Decision     *harness.DecisionPayload // the decision this run answers, if any
    Inbox        harness.Inbox            // pending user.queued, oldest first
}
```

Rebuilds everything from the log, in one pass over `harness.events`:

- **History.** `user.message` → user `Message` (attachment refs become parts),
  `agent.tool_call` → assistant message with calls, `agent.tool_result` →
  tool message, `agent.message` → assistant text. UI-only events
  (`agent.question`, `agent.proposal`, `agent.plan`, `agent.confirm`,
  `agent.block`, `agent.memory`, `agent.error`, `session.status`) carry
  nothing the model needs: their call and result are already in
  `tool_call` / `tool_result`. A `session.summary` replaces everything before
  its `through` seq with one summary message.
- **Grants.** `Grants.Tools` is the union of `GrantGroup`s approved with
  `approve_all` since the last `user.message`. `Grants.Calls` are the
  `CallKey`s of an approved `agent.plan`'s steps not called yet, each good
  for one call at any tier. A new user line clears both.
- **Decision.** The `user.decision` after the latest `agent.confirm` or
  `agent.plan`, if the run was started by it. A retried run whose decided call
  already has a result in the log gets no Decision, so an approved call never
  runs twice.
- **Attachments.** Bytes are loaded newest first under a byte budget (default
  10 MiB); the newest message's always load. An attachment over budget, or
  whose blob is gone (`ErrBlobNotFound`), becomes `harness.OmittedPart`.
- **Lapsed calls.** A call without a result before the next message (an
  approval the user moved past, a question answered by the next message)
  gets a synthetic `not_run` tool result, so history stays valid for every
  provider.
- **Inbox.** Reads pending `user.queued` (not yet delivered by a
  `user.message`), oldest first, each once per run; `revoke` is true when
  any arrive (a new instruction ends pre-approval). Wrap it with
  `dbosharness.Inbox` in a durable run. `Finish` pairs the run's injected
  user messages with their `user.queued` by the seq the inbox puts in
  `Message.ID`, so no bookkeeping crosses from `Load` to `Finish`.

### Finish

In one transaction, and only if `run_id` is still `runID` (else a no-op: the
run was stopped or superseded):

| From `RunResult` | Event |
| --- | --- |
| injected user message | `user.message` (`queuedSeq` links the `user.queued` it came from) |
| assistant message with tool calls | `agent.tool_call` (text + calls + display), `message_id` set |
| tool message | `agent.tool_result` (`tool_call_id` set, a sub-agent call's child calls in `subCalls`) |
| `Blocks` | `agent.block` |
| remember / forget results | `agent.memory` |
| assistant text (`Reply`) | `agent.message` (`message_id` set) |
| `Interaction` | `agent.question`, `agent.proposal` or `agent.plan` |
| `Confirm` | `agent.confirm` (`approvalId` = the held call's id) |
| `runErr` | `agent.error` with a sanitized message; the raw error is logged |

Then `session.status` with the next status (`idle`, `awaiting_input` or
`error`), `stop`, `usage`, `iterations` and `model`, and the row update
(`status`, `context_tokens`, `updated_at`). Messages are kept even when the
run failed: they are complete call/result pairs and the spend is real.
Publishing happens after commit.

When the run ends idle with queued messages its inbox never read, `Finish`
delivers them and starts the next run, returning its id to kick.

### Compact

```go
func Compact(ctx context.Context, bus Bus, a *harness.Agent, m harness.Model, conversationID string) error
```

Call it after `Finish`, outside any transaction: it calls the model. Once
the conversation's `context_tokens` reach `a.Compaction.Threshold`,
`harness.Compact` summarizes everything before the last `KeepTail` user
turns and a `session.summary` is appended, its `through` the seq of the
first `user.message` kept verbatim. `Load` then starts history with the
summary followed by that tail. A no-op when the agent has no `Compaction`,
nothing is new to cut, or another summary landed while the model ran. In a
durable run, make it a step.

## Live streaming

```go
type Bus interface {
    Publish(ctx context.Context, conversationID string, ev harness.Event) error
    // Subscribe must be receiving when it returns. stop closes the channel
    // and is safe to call twice.
    Subscribe(ctx context.Context, conversationID string) (events <-chan harness.Event, stop func(), err error)
}

type FollowConfig struct {
    PageSize int            // default 200
    Heal     time.Duration  // default 15s
    OnTick   func() error   // keep-alive, e.g. ": ping"
}

func Follow(ctx context.Context, bus Bus, conversationID string, afterSeq int64,
    cfg FollowConfig, yield func(harness.Event) error) error
```

`Follow` gives every persisted event after `afterSeq` exactly once and in
order, then live events, until `ctx` ends or `yield` fails:

1. Subscribe first, so nothing published during the catch-up is missed.
2. Catch up: page `harness.events` where `seq > last`, yield each, advance
   `last`.
3. Yield `stream.ready`.
4. Live: an ephemeral event (`seq = 0`: delta, reasoning, step.reset,
   tool_start, tool_done) is yielded as it comes. A persisted event, or one
   with `Lagged`, is only a signal: catch up from the log again. Events with
   `seq <= last` are dropped.
5. Every `Heal`, catch up once more (repairs a lost publish) and call
   `OnTick`.

Ephemeral events go straight to the bus from the run's callbacks;
persisted events reach clients through the log. The project writes the
transport around `Follow`: ticket auth, limits, headers, frames.

```go
_ = pgharness.Follow(ctx, bus, convID, after, cfg, func(ev harness.Event) error {
    data, _ := json.Marshal(ev)
    if ev.Seq > 0 {
        fmt.Fprintf(w, "id: %d\n", ev.Seq) // ephemeral events never move Last-Event-ID
    }
    _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
    flusher.Flush()
    return err
})
```

Bus implementations: mccloud wraps its Redis `ai:session:<id>` channel;
verseclash wraps `pkg/realtime` with an `ai_conversation:<id>` topic and maps
its `Lagged` to `Event.Lagged`. `MemoryBus` ships for tests and single
instances.

## Attachments

```go
type Blobs interface {
    Put(ctx context.Context, key, mime string, data []byte) error
    Get(ctx context.Context, key string) ([]byte, error) // ErrBlobNotFound when gone
    DeletePrefix(ctx context.Context, prefix string) error
}
```

- Metadata always lives in `harness.attachments`. Bytes go to the `Blobs`
  set with `WithBlobs(ctx, b)`; without one they live in the row's `data`
  column.
- Keys are `harness/<conversationID>/<attachmentID>`, so a conversation's
  blobs go with one `DeletePrefix`.
- Retention is the store's job (a GCS lifecycle rule). A blob that is gone
  loads as `OmittedPart`; the conversation carries on.
- Models get bytes (data URIs), never signed URLs: bytes keep the prompt
  cache stable and work with every endpoint.
- `Load` keeps an in-process LRU (64 MiB) by `sha256` for `Blobs` bytes, so a conversation's next
  run does not download the same file again.
- Only images and PDFs (`harness.ValidateParts`). A project that accepts text
  files turns them into text parts before calling `Start` or `Send`.

## Memory

```go
func WithMemory(a harness.Agent, opt MemoryOptions) harness.Agent

type MemoryOptions struct {
    Max        int // memories injected per run; default 30
    MaxStored  int // per owner; default equal to Max, so every memory is visible
    MaxContent int // characters per memory; default 300
}
```

A decorator, like every extension: it adds `remember` and `forget` (Safe;
each change becomes an `agent.memory` event) and wraps the agent's `Context`
to inject the visible memories, newest first, after the system prompt.

- Visible = `owner_id` matches and the conversation context contains the
  memory's context. `remember` saves with the conversation's context or
  `{}` (owner-wide), chosen by the model from those two.
- Memories are context, not instructions; live tool results win.
- `MaxStored` defaults to `Max`: mccloud stores 100 but injects 30, so the
  agent cannot see, update or dedupe the other 70.
- `recall` is added when `MaxStored` is over `Max`: a pg_trgm
  `word_similarity` search (≥ 0.3, top 10) over the visible memories.
  Migration 0002 creates the `pg_trgm` extension.

## Housekeeping

- `FailOrphanedRuns(ctx, bus, idle, alive)`: marks `running` conversations idle
  for at least `idle` whose run `alive` reports dead (DBOS no longer knows
  it) as `error`, with an `agent.error`. Call after `dbos.Launch` with idle 0,
  then periodically.
- `Delete(ctx, id)`: soft-deletes one conversation (sets `deleted_at`). It then
  reads as `ErrNotFound` everywhere and `Start` no longer resumes it; rows,
  events and blobs stay. A run still in flight is not stopped; its `Finish`
  is a no-op. `DeleteContext` is the hard purge.
- `DeleteContext(ctx, v)`: deletes every conversation whose context contains
  `v`, their events and attachment rows (cascade), their blobs, and the
  memories whose context contains `v`. Call it where the project deletes the
  subject (a server, a tournament).

## Errors

| Error | When |
| --- | --- |
| `ErrNotFound` | conversation missing, deleted, or not the caller's |
| `ErrBusy` | a decision while a run is running |
| `ErrQueueFull` | too many queued messages |
| `ErrNotQueued` | `Unqueue` of a message that is not pending |
| `ErrStaleApproval` (core) | a decision naming anything but the open approval |
| `ErrContext` | `Start` without a `Require`d key |
| `ErrEmpty` | a message with neither text nor parts |
| `ErrSuperseded` | `Load` for a run that is no longer the running one |
| `ErrSubscriptionClosed` | `Follow` when the bus closed its subscription |
| `ErrBlobNotFound` | `Blobs.Get` on a missing object |

## Project responsibilities

| pgharness | Project |
| --- | --- |
| schema, migrations, events, seq | auth, rate limits, who may approve |
| start / send / queue / decision rules | HTTP routes and stream tickets |
| history, grants, decision, inbox from the log | kicking the run (DBOS workflow) |
| `run_id` fencing, status, usage on status | publishing deltas from run callbacks |
| `Follow` | SSE or WebSocket framing, stream limits |
| attachments metadata, byte budget, omitted parts | a `Blobs` for GCS or S3, text-file conversion |
| memory tools and injection | calling `DeleteContext` on deletes |

## Migrating mccloud

| mccloud today | pgharness |
| --- | --- |
| `ai_sessions` | `harness.conversations` (`surface` → `agent`, `account_id` → `owner_id`, `server_id` → `context.serverId`) |
| `ai_session_events` | `harness.events` (decision moves from `user.message.decision` to `user.decision`; `approvalId` backfilled from the confirm's `toolCallId`) |
| `ai_session_attachments` | `harness.attachments` (text files become text parts) |
| `ai_memories` | `harness.memories` (`server_id` → `context.serverId`) |
| `api/ai/ai.go` Start / Send | `Start` / `Send` |
| `api/ai/run.go` history rebuild, `grantTracker` | `Load` |
| `api/ai/run.go` outcome write | `Finish` |
| `api/ai/stream.go` catch-up | `Follow` |
| `api/ai/attachment.go` byte budget | `Load` |
| `api/ai/agent/memory.go` | `WithMemory` |
| LLM-generated session title | `SetTitle` |
| delete a session | `Delete` (soft) |
| FK cascade on `servers` | `DeleteContext(ctx, Values{"serverId": id})` in server delete |

Keep the `ai_*` tables read-only for a while after the copy, for rollback.

## Not yet

- **pgvector** for `recall`: needs an embedding model; trigram covers
  keyword and Thai substring matches.

## Open questions

- **Per-turn persistence.** Writing events at `Finish` means a client that
  reconnects mid-run sees no tool progress until the run ends (mccloud
  today). Persisting each turn as it completes would fix that.
- **Approval in shared conversations.** `PolicyShared` needs a rule for who
  may approve; today the project checks before `Send`.
