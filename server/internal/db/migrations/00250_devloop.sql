-- +goose Up
-- devloop: a task agent's live-sync lease on the shared mainloop-dev instance, and the durable
-- intents for the operations that touch Kubernetes (sync, status). Only the API
-- process (serve-api) holds a Kubernetes client; the MCP listener (serve-mcp) only reads and
-- writes these rows, the same boundary it already keeps for the credential Secrets. See
-- docs/specs/devloop.md.

-- The one lease. holder_session_id is plain text, not a foreign key to sessions: the lease row
-- outlives any one session, the way git_publications.workspace_id outlives a workspace.
CREATE TABLE devloop_lease (
    id text PRIMARY KEY,
    holder_session_id text,
    acquired_at timestamptz,
    last_used_at timestamptz,
    idle_timeout_seconds integer NOT NULL DEFAULT 1200,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((holder_session_id IS NULL) = (acquired_at IS NULL)),
    CHECK ((holder_session_id IS NULL) = (last_used_at IS NULL))
);
INSERT INTO devloop_lease (id, idle_timeout_seconds) VALUES ('mainloop-dev', 1200);

-- FIFO queue of sessions waiting for the lease. requested_at is set once by the first acquire
-- call and never bumped, so a retried or polling acquire never jumps the line. last_seen_at is
-- bumped by every poll: an entry that stops polling goes stale and is skipped, so a waiter that
-- left cannot be handed the lease.
CREATE TABLE devloop_lease_queue (
    lease_id text NOT NULL REFERENCES devloop_lease (id) ON DELETE CASCADE,
    session_id text NOT NULL,
    requested_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (lease_id, session_id)
);
CREATE INDEX devloop_lease_queue_order ON devloop_lease_queue (lease_id, requested_at, session_id);

-- One sync at a time across replicas: a sync takes this row before it copies and drops it when it
-- ends. held_until covers a crashed holder (a sync never runs past its own deadline).
CREATE TABLE devloop_sync_lock (
    id text PRIMARY KEY,
    op_id text,
    held_until timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((op_id IS NULL) = (held_until IS NULL))
);
INSERT INTO devloop_sync_lock (id) VALUES ('mainloop-dev');

-- Durable intents for the operations that reach Kubernetes or GitHub: sync and status.
-- The MCP listener writes one row (or reuses one by idempotency_key) and polls it; serve-api's
-- reconcile loop claims it, does the I/O with no transaction open, and records the outcome.
--
-- `unknown` is for a sync whose copy outcome is uncertain (the exec stream was cut mid-write).
-- Unlike a git push, overwriting the sync directory with the same commit's tree is naturally
-- idempotent, so the loop resolves `unknown` by safely re-running the copy rather than first
-- observing remote state, which is the one place this table departs from the reconcile package's
-- observe-before-retry rule (see docs/specs/devloop.md for the reasoning).
CREATE TABLE devloop_operations (
    id text PRIMARY KEY,
    idempotency_key text NOT NULL UNIQUE,
    kind text NOT NULL CHECK (kind IN ('sync', 'status')),
    session_id text NOT NULL,
    project_id text NOT NULL,
    params jsonb NOT NULL,
    result jsonb,
    detail text,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'succeeded', 'failed', 'unknown')),
    attempts integer NOT NULL DEFAULT 0,
    -- interrupted counts copies whose outcome was uncertain; attempts also counts claims that were
    -- put off while another sync held the sync lock.
    interrupted integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX devloop_operations_claim ON devloop_operations (next_attempt_at, id)
    WHERE state IN ('pending', 'unknown');

-- +goose Down
DROP TABLE IF EXISTS devloop_operations;
DROP TABLE IF EXISTS devloop_sync_lock;
DROP TABLE IF EXISTS devloop_lease_queue;
DROP TABLE IF EXISTS devloop_lease;
