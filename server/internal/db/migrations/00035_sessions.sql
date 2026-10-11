-- +goose Up
-- The main thread's message mirror and the delivery ledger (G6a). Sessions and their agent
-- bindings are in 00010_core.

-- The request id of a kagent CreateSession that was sent (or may have been) for the binding, written
-- before the call. A create whose Session id never came back is found by it: the orphan sweep
-- counts the binding until the create is resolved, and the sessions sweep replays the create by
-- this id once the session has no open delivery.
--
-- The Agent the create named is stored with it, so a replay names the same Agent after the
-- provider or main-agent configuration has changed.
ALTER TABLE agent_bindings ADD COLUMN kagent_create_request_id text;
ALTER TABLE agent_bindings ADD COLUMN kagent_create_agent_namespace text;
ALTER TABLE agent_bindings ADD COLUMN kagent_create_agent_name text;
CREATE INDEX agent_bindings_create_unresolved ON agent_bindings (session_id)
WHERE kagent_create_request_id IS NOT NULL AND kagent_session_id IS NULL;

-- Every Agent Mainloop has sent a create to, kept for good. A Session can outlive the row that named
-- its Agent (a settled create, a purged workspace) and the configuration that chose it, and the
-- orphan sweep only looks at Sessions on Agents it knows. Written by triggers in the transaction
-- that records the Agent, so a create is never sent for an Agent missing here.
CREATE TABLE kagent_agents (
    namespace text NOT NULL,
    name text NOT NULL,
    PRIMARY KEY (namespace, name)
);
-- +goose StatementBegin
CREATE FUNCTION note_create_agent() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO kagent_agents (namespace, name) VALUES (NEW.kagent_create_agent_namespace, NEW.kagent_create_agent_name)
    ON CONFLICT DO NOTHING;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER agent_bindings_note_agent AFTER INSERT OR UPDATE OF kagent_create_agent_namespace, kagent_create_agent_name
ON agent_bindings FOR EACH ROW
WHEN (NEW.kagent_create_agent_namespace IS NOT NULL AND NEW.kagent_create_agent_name IS NOT NULL)
EXECUTE FUNCTION note_create_agent();

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
    -- The kagent Session (A2A contextId) the message was sent to, stored when the row moves to
    -- sending so a later lookup asks the Session the message went to, not whatever the binding
    -- names by then.
    kagent_session_id text,
    attempts integer NOT NULL DEFAULT 0,
    -- How many times creating the kagent Session failed for this delivery. Claims (attempts) also
    -- count waits for a credential cleanup and lease recoveries, which are not failures.
    create_failures integer NOT NULL DEFAULT 0,
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
-- The health collector's discovery walk: deliveries that carry a kagent task, in creation order.
CREATE INDEX deliveries_health ON deliveries (created_at, id)
WHERE task_id IS NOT NULL AND kagent_session_id IS NOT NULL
  AND state IN ('delivered', 'completed', 'failed', 'cancelled');

-- One main thread. The partial index is the backstop for the ensure step's advisory lock. It is
-- global, not per owner: agent_bindings has no owner column, and the deployment has one owner. The
-- ensure step's lock and lookup are per owner, so a second owner needs this index replaced.
CREATE UNIQUE INDEX agent_bindings_one_main ON agent_bindings ((true)) WHERE role = 'main';

-- The main session is never archived. An archived main session would still hold the index above,
-- and no new one could be created.
-- +goose StatementBegin
CREATE FUNCTION refuse_archiving_main_session() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.archived_at IS NOT NULL AND OLD.archived_at IS NULL AND EXISTS (
        SELECT 1 FROM agent_bindings b WHERE b.session_id = NEW.id AND b.role = 'main'
    ) THEN
        RAISE EXCEPTION 'the main session cannot be archived' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER sessions_keep_main BEFORE UPDATE OF archived_at ON sessions
FOR EACH ROW EXECUTE FUNCTION refuse_archiving_main_session();

-- +goose Down
DROP TRIGGER IF EXISTS agent_bindings_note_agent ON agent_bindings;
DROP FUNCTION IF EXISTS note_create_agent();
DROP TABLE IF EXISTS kagent_agents;
DROP INDEX IF EXISTS agent_bindings_create_unresolved;
ALTER TABLE agent_bindings DROP COLUMN IF EXISTS kagent_create_agent_name;
ALTER TABLE agent_bindings DROP COLUMN IF EXISTS kagent_create_agent_namespace;
ALTER TABLE agent_bindings DROP COLUMN IF EXISTS kagent_create_request_id;
DROP TRIGGER IF EXISTS sessions_keep_main ON sessions;
DROP FUNCTION IF EXISTS refuse_archiving_main_session();
DROP INDEX IF EXISTS agent_bindings_one_main;
DROP TABLE IF EXISTS deliveries;
DROP TABLE IF EXISTS session_messages;
