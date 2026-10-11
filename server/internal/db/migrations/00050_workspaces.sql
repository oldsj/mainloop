-- +goose Up
-- Workspaces: a session with a git checkout, run as a kagent Session. The row holds only what
-- Mainloop owns (the checkout request, preview ports, idle settings); the running state is read
-- from kagent. The session and its agent binding are the session slice's rows; a workspace row
-- shares the session's id.
--
-- Every lifecycle change is a workspace_ops row: an intent with an explicit state and an
-- idempotency key that the reconcile loop drives (see internal/workspaces). The kagent
-- CreateSession request id is stored once on the workspace and reused by every replay, so a
-- crash mid-create resumes into the one Session.

CREATE TABLE workspaces (
    id text PRIMARY KEY REFERENCES sessions (id) ON DELETE CASCADE,
    user_id text NOT NULL,
    project_id text NOT NULL REFERENCES projects (id),
    repo_url text NOT NULL,
    ref text NOT NULL DEFAULT '',
    branch text NOT NULL,
    depth integer NOT NULL DEFAULT 0 CHECK (depth BETWEEN 0 AND 1000),
    agent_kind text NOT NULL CHECK (agent_kind ~ '^[a-z][a-z0-9_-]{0,79}$'),
    -- The kagent Agent resolved from agent_kind when the workspace was created. Every replay of
    -- the create reuses it, so a config change cannot turn a retry into a different request, and
    -- the orphan sweep still recognises Sessions made under an earlier config.
    agent_namespace text NOT NULL,
    agent_name text NOT NULL,
    -- The ports the preview URL may reach: [{"name": ..., "number": ...}].
    ports jsonb NOT NULL DEFAULT '[]',
    idle_timeout_minutes integer NOT NULL DEFAULT 30 CHECK (idle_timeout_minutes BETWEEN 1 AND 1440),
    -- The development environment resolved when the workspace was created; NULL when the project
    -- has none selected.
    environment jsonb,
    -- The git-read credential reference the create carries, frozen before the first CreateSession
    -- RPC like the Agent and environment above: git_read_decided says the decision was made, and a
    -- NULL git_read_credential then means "no git read". Retries and the cleanup replay rebuild the
    -- request's credentials from this, never from the current git transport configuration, because
    -- kagent refuses a replayed request id whose credentials differ. A protojson SessionCredential.
    git_read_decided boolean NOT NULL DEFAULT false,
    git_read_credential jsonb,
    -- The CreateSession request id: stable for the life of the workspace.
    request_id text NOT NULL UNIQUE,
    -- Owner activity (resume, preview traffic). Delivery activity is read from the delivery ledger.
    last_active_at timestamptz,
    -- When the idle sweep (or a suspend) last parked the workspace.
    idle_suspended_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (git_read_decided OR git_read_credential IS NULL)
);
CREATE INDEX workspaces_user ON workspaces (user_id, created_at DESC);

-- The Agent is recorded for the orphan sweep (kagent_agents, 00035) when the workspace is made, so it
-- is still known after the row is purged.
-- +goose StatementBegin
CREATE FUNCTION note_workspace_agent() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO kagent_agents (namespace, name) VALUES (NEW.agent_namespace, NEW.agent_name) ON CONFLICT DO NOTHING;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER workspaces_note_agent AFTER INSERT OR UPDATE OF agent_namespace, agent_name ON workspaces FOR EACH ROW EXECUTE FUNCTION note_workspace_agent();
CREATE INDEX workspaces_project ON workspaces (project_id);

CREATE TABLE workspace_ops (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('create', 'suspend', 'resume', 'delete')),
    idempotency_key text NOT NULL UNIQUE,
    -- pending and retrying are claimable; the rest are final.
    state text NOT NULL DEFAULT 'pending' CHECK (
        state IN ('pending', 'retrying', 'succeeded', 'rejected', 'gave_up', 'cancelled')
    ),
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    -- Transient failures so far and when the first one happened: the retry window runs from it.
    failures integer NOT NULL DEFAULT 0,
    first_failed_at timestamptz,
    -- The classified reason of the last failure, never raw error text.
    last_error text CHECK (length(last_error) <= 500),
    -- An idle suspend only goes ahead if the workspace saw no activity after this time.
    quiet_since timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
-- One open operation per workspace.
CREATE UNIQUE INDEX workspace_ops_open ON workspace_ops (workspace_id) WHERE state IN ('pending', 'retrying');
CREATE INDEX workspace_ops_claim ON workspace_ops (next_attempt_at, id) WHERE state IN ('pending', 'retrying');
CREATE INDEX workspace_ops_workspace ON workspace_ops (workspace_id, created_at DESC, id);

-- +goose Down
DROP TABLE IF EXISTS workspace_ops;
DROP TRIGGER IF EXISTS workspaces_note_agent ON workspaces;
DROP FUNCTION IF EXISTS note_workspace_agent();
DROP TABLE IF EXISTS workspaces;
