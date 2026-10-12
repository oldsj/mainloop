-- +goose Up
-- Projects, sessions, their agent bindings, issued credentials and topics. States are text with
-- a CHECK; the Go named types live in internal/db/dbstate. Later files add reconcile columns.

CREATE TABLE projects (
    id text PRIMARY KEY,
    user_id text NOT NULL,
    owner text NOT NULL,
    name text NOT NULL,
    full_name text NOT NULL,
    description text,
    default_branch text NOT NULL DEFAULT 'main',
    avatar_url text,
    html_url text NOT NULL,
    merge_policy text NOT NULL DEFAULT 'auto' CHECK (merge_policy IN ('auto', 'approval')),
    merge_policy_version bigint NOT NULL DEFAULT 1 CHECK (merge_policy_version > 0),
    open_pr_count integer NOT NULL DEFAULT 0,
    open_issue_count integer NOT NULL DEFAULT 0,
    metadata_updated_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz NOT NULL DEFAULT now()
);
-- GitHub names are case-insensitive: one project per repository per owner.
CREATE UNIQUE INDEX projects_user_full_name ON projects (user_id, lower(full_name));
CREATE INDEX projects_last_used ON projects (user_id, last_used_at DESC);

CREATE TABLE topics (
    id text PRIMARY KEY,
    user_id text NOT NULL,
    name text NOT NULL,
    status_line text NOT NULL DEFAULT '',
    checkpoint text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, name)
);

CREATE TABLE sessions (
    id text PRIMARY KEY,
    user_id text NOT NULL,
    project_id text REFERENCES projects (id),
    title text NOT NULL,
    description text NOT NULL,
    prompt text NOT NULL,
    status text NOT NULL DEFAULT 'pending' CHECK (
        status IN (
            'pending', 'active', 'idle', 'waiting_on_user', 'superseded', 'completed', 'failed',
            'cancelled'
        )
    ),
    repo_url text,
    branch_name text,
    base_branch text NOT NULL DEFAULT 'main',
    model text,
    commit_sha text,
    summary text,
    error text,
    color text,
    result jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    completed_at timestamptz,
    -- Archived sessions leave the list but stay for audit.
    archived_at timestamptz
);
CREATE INDEX sessions_user_created ON sessions (user_id, created_at DESC);
CREATE INDEX sessions_status ON sessions (status);
CREATE INDEX sessions_project ON sessions (project_id);

-- One native agent binding per session: which kagent Session runs it and what it may do.
CREATE TABLE agent_bindings (
    session_id text PRIMARY KEY REFERENCES sessions (id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind ~ '^[a-z][a-z0-9_-]{0,79}$'),
    role text NOT NULL CHECK (role IN ('main', 'child', 'agent', 'supervisor')),
    mcp_grant_kind text NOT NULL DEFAULT 'none' CHECK (
        mcp_grant_kind IN ('none', 'coordination', 'workspace')
    ),
    parent_session_id text REFERENCES sessions (id) ON DELETE SET NULL,
    topic_id text REFERENCES topics (id) ON DELETE SET NULL,
    -- The kagent Session id (also the A2A contextId); NULL until kagent confirms the create.
    kagent_session_id text,
    -- CreateSession request id of a replacement Session; NULL means the derived one.
    kagent_request_id text,
    model text,
    standing_hash text,
    turns integer NOT NULL DEFAULT 0 CHECK (turns >= 0),
    -- The owner stopped a turn: queued messages wait for the next one.
    queue_held boolean NOT NULL DEFAULT false,
    child_start_failure text,
    reported_at timestamptz,
    -- Set once kagent confirmed DeleteSession for an archived session.
    kagent_deleted_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX agent_bindings_kagent_session ON agent_bindings (kagent_session_id)
WHERE kagent_session_id IS NOT NULL;
CREATE INDEX agent_bindings_parent ON agent_bindings (parent_session_id);
CREATE INDEX agent_bindings_topic ON agent_bindings (topic_id);

-- Bearer credentials issued to agents: MCP, git read and git push. Only the sha256 of the bearer
-- is stored; the bearer is derived from the HMAC key when it is published. The row is written as
-- an intent first ('planned'), then the Kubernetes Secret is created and its UID recorded
-- ('published'). 'revoked' means the Secret is still owed a delete; 'deleted' means it is gone.
-- binding_id has no foreign key on purpose: a revoked credential must outlive its binding so the
-- Secret is still cleaned up.
CREATE TABLE credentials (
    id text PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('mcp', 'git_read', 'git_push')),
    binding_id text NOT NULL,
    token_hash text NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    secret_name text NOT NULL,
    secret_uid text,
    state text NOT NULL DEFAULT 'planned' CHECK (
        state IN ('planned', 'published', 'revoked', 'deleted')
    ),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    CHECK (state <> 'published' OR secret_uid IS NOT NULL)
);
-- A Secret name is held by one credential until that credential is deleted.
CREATE UNIQUE INDEX credentials_secret_name ON credentials (secret_name) WHERE state <> 'deleted';
-- At most one live credential of a kind per binding.
CREATE UNIQUE INDEX credentials_live ON credentials (binding_id, kind)
WHERE state IN ('planned', 'published');
CREATE INDEX credentials_cleanup ON credentials (updated_at) WHERE state = 'revoked';

-- Notes, decisions, pending intent and child reports for a topic.
CREATE TABLE topic_records (
    id text PRIMARY KEY,
    topic_id text NOT NULL REFERENCES topics (id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('note', 'decision', 'pending', 'report')),
    text text NOT NULL,
    status text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'done')),
    session_id text REFERENCES sessions (id) ON DELETE SET NULL,
    evidence_ref text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX topic_records_topic ON topic_records (topic_id, created_at);

-- +goose Down
DROP TABLE IF EXISTS topic_records;
DROP TABLE IF EXISTS credentials;
DROP TABLE IF EXISTS agent_bindings;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS topics;
DROP TABLE IF EXISTS projects;
