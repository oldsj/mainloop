-- +goose Up
-- Checkpoint and fork, on top of kagent's CheckpointService (issue #288, "warm-state comparison").
-- Experimental and owner-only: see docs/specs/warm-state.md. Both tables are durable intents keyed
-- by the caller's request_id, following the reconciler rule (AGENTS.md): written before the kagent
-- call, which is itself idempotent by request_id (like CreateSession; see
-- server/internal/kagent/checkpoints.go), so a retry is a repeat of the same call rather than an
-- observe-then-act dance.

-- A checkpoint of a session's native kagent Session at a terminal task. expected_head_task_id is
-- frozen at the first write and resent on every retry, so a retry never asks kagent to checkpoint a
-- different turn than the one the caller's request was refused or left uncertain for.
CREATE TABLE session_checkpoints (
    id text PRIMARY KEY,
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    kagent_session_id text NOT NULL,
    expected_head_task_id text NOT NULL,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'done')),
    -- Set once kagent confirms the checkpoint; NULL while pending or on a definite failure.
    checkpoint_id text,
    checkpoint_name text,
    -- Set only on a definite kagent refusal (not an uncertain outcome, which stays pending and is
    -- retried): the request will fail the same way every time, so it is not retried forever.
    failure_reason text CHECK (failure_reason IN ('invalid_argument', 'not_found', 'failed_precondition')),
    detail text,
    attempts integer NOT NULL DEFAULT 0,
    requested_at timestamptz NOT NULL DEFAULT now(),
    confirmed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (state = 'pending' OR checkpoint_id IS NOT NULL OR failure_reason IS NOT NULL)
);
CREATE INDEX session_checkpoints_session ON session_checkpoints (session_id, created_at DESC);
CREATE INDEX session_checkpoints_due ON session_checkpoints (updated_at) WHERE state = 'pending';

-- A fork of a checkpoint into a new Mainloop session bound to the forked kagent Session. Timestamps
-- let time-to-running be read back: requested_at (this row written), accepted_at (kagent's
-- ForkSession confirmed and the new session/binding exist), ready_at (the forked runtime observed
-- READY). new_session_id is set at accepted_at; a failure leaves it NULL.
CREATE TABLE checkpoint_forks (
    id text PRIMARY KEY,
    checkpoint_id text NOT NULL,
    source_session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    fresh_conversation boolean NOT NULL DEFAULT false,
    title text NOT NULL DEFAULT '',
    state text NOT NULL DEFAULT 'requested' CHECK (state IN ('requested', 'accepted', 'ready', 'failed')),
    forked_kagent_session_id text,
    new_session_id text REFERENCES sessions (id) ON DELETE SET NULL,
    failure_reason text CHECK (failure_reason IN ('invalid_argument', 'not_found', 'failed_precondition')),
    detail text,
    attempts integer NOT NULL DEFAULT 0,
    requested_at timestamptz NOT NULL DEFAULT now(),
    accepted_at timestamptz,
    ready_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (state NOT IN ('accepted', 'ready') OR (forked_kagent_session_id IS NOT NULL AND new_session_id IS NOT NULL)),
    CHECK (state <> 'failed' OR failure_reason IS NOT NULL)
);
CREATE INDEX checkpoint_forks_due ON checkpoint_forks (updated_at) WHERE state IN ('requested', 'accepted');

-- +goose Down
DROP TABLE IF EXISTS checkpoint_forks;
DROP TABLE IF EXISTS session_checkpoints;
