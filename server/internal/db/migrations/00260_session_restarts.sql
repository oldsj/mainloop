-- +goose Up
-- The owner's restart of a session whose turn cannot be stopped (issue #262): delete the native
-- kagent Session, then settle the turns that were open on it as failed so the queue moves on. The
-- row is the durable intent, written before the DeleteSession call: id is the caller's idempotency
-- key, kagent_session_id is the Session the restart deletes (null when the binding had none), and
-- state is requested until the delete is confirmed and the turns are settled (done). A requested row
-- is continued by repeating the request or by the resume sweep; it observes kagent before it deletes
-- again, and never targets a Session other than the one it recorded. At most one restart per session
-- is open at a time.
CREATE TABLE session_restarts (
    id text PRIMARY KEY,
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    kagent_session_id text,
    state text NOT NULL DEFAULT 'requested' CHECK (state IN ('requested', 'done')),
    -- The last error of an unconfirmed delete, for the log and the next attempt.
    detail text,
    -- The deliveries the restart failed, once done.
    failed_message_ids text[] NOT NULL DEFAULT '{}',
    attempts integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX session_restarts_one_open ON session_restarts (session_id) WHERE state = 'requested';

-- +goose Down
DROP TABLE session_restarts;
