-- +goose Up
-- The main thread's message mirror and the delivery ledger (G6a). Sessions and their agent
-- bindings are in 00010_core.

-- Messages of a session's conversation, mirrored from what the user sent and what the agent
-- answered. The conversation id of the contract is the session id. The user message and its
-- delivery share one id, which is also the A2A messageId; the reply's id is derived from the
-- session and task, so mirroring it twice is a no-op.
CREATE TABLE session_messages (
    id text PRIMARY KEY,
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('user', 'assistant')),
    content text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX session_messages_session ON session_messages (session_id, created_at, id);

-- One row per message sent to a native session. It is an intent row for the reconciler: the row
-- is written with the user message, then a loop sends it, follows the task and settles it.
--
-- State machine (the sessions package owns the transitions):
--   recorded   written; nothing has been sent
--   sending    the claimer is calling kagent; the outcome is not known yet
--   delivered  a task exists for the message (task_id); the turn is running
--   completed  the task finished; the reply is mirrored
--   failed     final: not sent, rejected, or the task failed (detail says which)
--   cancelled  final: the turn was stopped
--   uncertain  the send may or may not have reached kagent; resolved by looking the message up
--              (never replayed)
CREATE TABLE deliveries (
    -- The message id; also the A2A messageId, so kagent dedups a retried send on it.
    id text PRIMARY KEY,
    idempotency_key text NOT NULL UNIQUE,
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    state text NOT NULL DEFAULT 'recorded' CHECK (
        state IN ('recorded', 'sending', 'delivered', 'completed', 'failed', 'cancelled', 'uncertain')
    ),
    source text NOT NULL DEFAULT 'user',
    task_id text,
    evidence_ref text,
    detail text,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
-- One open turn per session. This is the only thing that stops two writers on one session.
CREATE UNIQUE INDEX deliveries_one_open ON deliveries (session_id)
WHERE state IN ('recorded', 'sending', 'delivered');
-- The claim index: only the states a loop works on.
CREATE INDEX deliveries_claim ON deliveries (next_attempt_at, id)
WHERE state IN ('recorded', 'sending', 'delivered', 'uncertain');
CREATE INDEX deliveries_session ON deliveries (session_id, created_at DESC);

-- One main thread. The partial index is the backstop for the ensure step's advisory lock.
CREATE UNIQUE INDEX agent_bindings_one_main ON agent_bindings ((true)) WHERE role = 'main';

-- +goose Down
DROP INDEX IF EXISTS agent_bindings_one_main;
DROP TABLE IF EXISTS deliveries;
DROP TABLE IF EXISTS session_messages;
