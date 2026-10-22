-- +goose Up
-- Contract C4 of docs/plans/phase-1-lean-chat.md. The conductor's Postgres
-- SessionStore tests (conductor/test/pgtest.ts) apply only this Up section,
-- so keep it free of dollar-quoting and goose statement markers.

create table web_sessions (
    id_hash      bytea primary key,
    person_id    text not null,
    connector_id text not null,
    email        text not null,
    created_at   timestamptz not null default now(),
    last_seen_at timestamptz not null default now(),
    expires_at   timestamptz not null
);
create index web_sessions_expires_idx on web_sessions (expires_at);

-- The ref is 'u_' plus 10 random hex digits (P21).
create table person_refs (
    person_id  text primary key,
    ref        text not null unique check (ref ~ '^u_[0-9a-f]{10}$'),
    created_at timestamptz not null default now()
);

-- fork_at_message_id has no foreign key: messages reference sessions, and a
-- fork points into another session of the same tree.
create table sessions (
    id                 text primary key,
    root_id            text not null references sessions (id) on delete cascade,
    parent_id          text references sessions (id) on delete cascade,
    fork_at_message_id text,
    person_id          text not null,
    agent_id           text not null,
    agent_version      text not null,
    agent_snapshot     jsonb not null,
    tier               text not null,
    model              text not null,
    helper_model       text not null,
    effort             text not null,
    cache_ttl          text not null,
    workspace          text not null default 'none' check (workspace = 'none'),
    api_workspace      text not null check (api_workspace in ('interactive', 'ci')),
    source             text not null,
    incognito          boolean not null default false,
    bound_models       text[] not null default '{}',
    title              text not null default '',
    sdk_session_id     text,
    profile_snapshot   text not null default '',
    status             text not null default 'idle',
    tainted_at         timestamptz,
    last_totals        jsonb,
    archived_at        timestamptz,
    created_at         timestamptz not null default now(),
    updated_at         timestamptz not null default now(),
    last_activity_at   timestamptz not null default now()
);
create index sessions_roots_idx on sessions (person_id, archived_at, updated_at desc) where id = root_id;
create index sessions_root_idx on sessions (root_id);
create index sessions_parent_idx on sessions (parent_id);
create index sessions_status_idx on sessions (status);

-- parent_message_id has no foreign key, for the same reason as fork_at_message_id.
create table messages (
    id                text primary key,
    session_id        text not null references sessions (id) on delete cascade,
    parent_message_id text,
    turn_id           text not null,
    role              text not null check (role in ('user', 'assistant')),
    content           jsonb not null,
    sdk_uuid          text,
    model             text,
    status            text not null check (status in ('running', 'complete', 'error')),
    error_code        text,
    created_at        timestamptz not null default now()
);
create index messages_session_idx on messages (session_id, created_at, id);

create table events (
    id         bigint generated always as identity primary key,
    session_id text not null references sessions (id) on delete cascade,
    type       text not null,
    payload    jsonb not null,
    created_at timestamptz not null default now()
);
create index events_session_idx on events (session_id, id);

create table client_events (
    session_id text not null references sessions (id) on delete cascade,
    client_id  uuid not null,
    message_id text not null,
    primary key (session_id, client_id)
);

create table attachments (
    id         text primary key,
    person_id  text not null,
    root_id    text references sessions (id) on delete cascade,
    media_type text not null,
    filename   text not null,
    size_bytes bigint not null,
    width      integer,
    height     integer,
    pages      integer,
    sha256     text not null,
    data       bytea not null,
    created_at timestamptz not null default now()
);
create index attachments_root_idx on attachments (root_id);
create index attachments_person_idx on attachments (person_id);

create table memory_profiles (
    person_id  text primary key,
    content    text not null,
    version    integer not null default 1,
    updated_at timestamptz not null default now()
);

create table memory_writes (
    id         text primary key,
    person_id  text not null,
    session_id text references sessions (id) on delete set null,
    turn_id    text not null,
    op         text not null check (op in ('remember', 'forget')),
    text       text not null,
    matched    text[],
    untrusted  boolean not null default false,
    status     text not null check (status in ('applied', 'pending', 'rejected')),
    created_at timestamptz not null default now(),
    decided_at timestamptz
);
create index memory_writes_person_idx on memory_writes (person_id, created_at desc);
create index memory_writes_session_idx on memory_writes (session_id);

create table cost_ledger (
    id                    bigint generated always as identity primary key,
    session_id            text references sessions (id) on delete set null,
    person_id             text not null,
    agent_id              text not null,
    tier                  text not null,
    source                text not null,
    api_workspace         text not null check (api_workspace in ('interactive', 'ci')),
    model                 text not null,
    turn_id               text not null,
    input_tokens          bigint not null default 0,
    output_tokens         bigint not null default 0,
    cache_read_tokens     bigint not null default 0,
    cache_creation_tokens bigint not null default 0,
    web_search_requests   bigint not null default 0,
    cost_usd              numeric(12, 6) not null default 0,
    created_at            timestamptz not null default now()
);
create index cost_ledger_created_idx on cost_ledger (created_at);
create index cost_ledger_session_idx on cost_ledger (session_id);

-- session_id has no foreign key (C4); DeleteTree nulls it.
create table usage_anomalies (
    id         bigint generated always as identity primary key,
    session_id text,
    turn_id    text not null,
    reason     text not null check (reason in ('negative', 'oversized')),
    payload    jsonb not null,
    created_at timestamptz not null default now()
);
create index usage_anomalies_session_idx on usage_anomalies (session_id);

-- workspace is the Anthropic workspace name, e.g. luma-ci.
create table usage_reconciliations (
    day        date not null,
    workspace  text not null,
    api_usd    numeric(12, 6) not null,
    ledger_usd numeric(12, 6) not null,
    created_at timestamptz not null default now(),
    primary key (day, workspace)
);

create table eval_runs (
    id          bigint generated always as identity primary key,
    suite       text not null,
    sdk_version text not null,
    cli_version text not null,
    image       text not null,
    started_at  timestamptz not null,
    finished_at timestamptz not null,
    passed      integer not null,
    failed      integer not null,
    cost_usd    numeric(12, 6) not null,
    results     jsonb not null
);
create index eval_runs_suite_idx on eval_runs (suite, finished_at desc);

create table settings (
    key        text primary key,
    value      jsonb not null,
    updated_at timestamptz not null default now()
);

-- project_key is the conversation tree's root id, session_id the SDK session
-- id. entry is text, not jsonb: jsonb would reorder keys and the transcript
-- must round-trip byte for byte (P5).
create table sdk_transcript_entries (
    project_key text not null,
    session_id  text not null,
    subpath     text not null default '',
    seq         bigint generated always as identity,
    uuid        text,
    entry       text not null,
    primary key (project_key, session_id, subpath, seq)
);
create unique index sdk_transcript_entries_uuid_idx
    on sdk_transcript_entries (project_key, session_id, subpath, uuid)
    where uuid is not null;

-- +goose Down
drop table sdk_transcript_entries;
drop table settings;
drop table eval_runs;
drop table usage_reconciliations;
drop table usage_anomalies;
drop table cost_ledger;
drop table memory_writes;
drop table memory_profiles;
drop table attachments;
drop table client_events;
drop table events;
drop table messages;
drop table sessions;
drop table person_refs;
drop table web_sessions;
