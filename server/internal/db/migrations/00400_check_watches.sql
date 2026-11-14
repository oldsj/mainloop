-- +goose Up
-- A check watch is an agent's request to be told when CI on one commit settles, so it can end its
-- turn (and let its workspace suspend) instead of holding a turn open while CI runs
-- (docs/specs/ci.md). The watch loop reads the checks; when nothing is pending, or at the
-- deadline, it records the outcome and the message, then delivers that message to the session
-- once, under the watch's id as the message id.
--
--   watching       the loop polls the checks
--   delivering     the outcome and message are recorded; the message is being delivered
--   delivered      final: the session has the message (steered into its turn, or queued)
--   undeliverable  final: the session ended or went away before the message reached it
--   superseded     final: a newer watch of the same session and ref replaced it
CREATE TABLE check_watches (
    -- Also the message id of the delivery, so a retried send dedups on it.
    id text PRIMARY KEY,
    -- The session and the caller's request id: a retried tool call finds the same watch.
    idempotency_key text NOT NULL UNIQUE,
    user_id text NOT NULL,
    project_id text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    -- The ref as asked: a pull request (its head is read on every poll) or one commit.
    pr_number integer CHECK (pr_number > 0),
    commit_sha text CHECK (commit_sha ~ '^[0-9a-f]{40}$'),
    deadline timestamptz NOT NULL,
    state text NOT NULL DEFAULT 'watching' CHECK (
        state IN ('watching', 'delivering', 'delivered', 'undeliverable', 'superseded')
    ),
    outcome text CHECK (outcome IN ('green', 'failed', 'pending_timeout', 'no_checks', 'not_found')),
    -- The commit the outcome is about, and the message delivered.
    head_sha text,
    message text,
    detail text,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((pr_number IS NULL) <> (commit_sha IS NULL))
);
CREATE INDEX check_watches_claim ON check_watches (next_attempt_at, id)
WHERE state IN ('watching', 'delivering');
CREATE INDEX check_watches_session ON check_watches (session_id) WHERE state = 'watching';

-- +goose Down
DROP TABLE check_watches;
