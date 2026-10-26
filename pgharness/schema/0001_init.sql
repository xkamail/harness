create schema if not exists harness;

create table if not exists harness.conversations (
    id             text primary key,
    agent          text        not null,
    owner_id       text        not null,
    context        jsonb       not null default '{}',
    title          text        not null default '',
    status         text        not null default 'idle'
        check (status in ('idle', 'running', 'awaiting_input', 'error')),
    run_id         text,
    model          text        not null default '',
    context_tokens bigint      not null default 0,
    created_at     timestamptz not null default now(),
    updated_at     timestamptz not null default now(),
    deleted_at     timestamptz
);
create index if not exists conversations_owner_idx
    on harness.conversations (owner_id, agent, updated_at desc);
create index if not exists conversations_context_idx
    on harness.conversations using gin (context jsonb_path_ops);
create index if not exists conversations_running_idx
    on harness.conversations (updated_at)
    where status = 'running';

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
    data            bytea,
    created_at      timestamptz not null default now()
);
create index if not exists attachments_conversation_idx
    on harness.attachments (conversation_id);

create table if not exists harness.memories (
    id         text primary key,
    owner_id   text        not null,
    kind       text        not null check (kind in ('preference', 'fact', 'incident')),
    content    text        not null,
    context    jsonb       not null default '{}',
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now()
);
create index if not exists memories_owner_idx
    on harness.memories (owner_id, updated_at desc);
