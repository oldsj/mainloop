-- +goose Up
-- The owner chooses Claude or Codex for the main thread. The product session, conversation and
-- messages stay; the binding's native identity changes.
--
-- A switch is a durable intent on the binding before any kagent create: `kind` is the target
-- provider, `kagent_agent_*` its Agent, `kagent_request_id` the idempotency key (and
-- `kagent_create_*` the frozen create contract). `main_pending` stays set until the target Session
-- is confirmed ready. `main_error` is the bounded reason of the latest failed preparation, and
-- `main_recoverable` is set only once the target was seen absent, failed or deleted.
ALTER TABLE agent_bindings ADD COLUMN main_pending boolean NOT NULL DEFAULT false;
ALTER TABLE agent_bindings ADD COLUMN main_error text;
ALTER TABLE agent_bindings ADD COLUMN main_recoverable boolean NOT NULL DEFAULT false;
ALTER TABLE agent_bindings ADD CONSTRAINT agent_bindings_main_pending_is_main CHECK (
    role = 'main' OR (NOT main_pending AND main_error IS NULL AND NOT main_recoverable)
);
-- A main whose binding lost its coordination authority hands its owner slot to a replacement in
-- the transaction that creates it. The released row is kept for audit.
ALTER TABLE agent_bindings ADD COLUMN main_released_at timestamptz;
DROP INDEX agent_bindings_one_main;
CREATE UNIQUE INDEX agent_bindings_one_main ON agent_bindings ((true))
WHERE role = 'main' AND main_released_at IS NULL;

-- Native identities a main thread has had before its current one, oldest first. The kagent
-- Session of a retired identity is suspended and kept; the orphan sweep leaves it alone.
CREATE TABLE main_provider_history (
    id bigserial PRIMARY KEY,
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    provider text NOT NULL,
    kagent_session_id text,
    agent_namespace text,
    agent_name text,
    request_id text,
    create_request_id text,
    model text,
    turns integer NOT NULL DEFAULT 0,
    -- The identity was a pending target that was seen absent or failed, not a working main.
    abandoned boolean NOT NULL DEFAULT false,
    error text,
    retired_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX main_provider_history_session ON main_provider_history (session_id, retired_at, id);
CREATE INDEX main_provider_history_kagent_session ON main_provider_history (kagent_session_id)
WHERE kagent_session_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS main_provider_history;
DROP INDEX IF EXISTS agent_bindings_one_main;
CREATE UNIQUE INDEX agent_bindings_one_main ON agent_bindings ((true)) WHERE role = 'main';
ALTER TABLE agent_bindings DROP COLUMN IF EXISTS main_released_at;
ALTER TABLE agent_bindings DROP CONSTRAINT IF EXISTS agent_bindings_main_pending_is_main;
ALTER TABLE agent_bindings DROP COLUMN IF EXISTS main_recoverable;
ALTER TABLE agent_bindings DROP COLUMN IF EXISTS main_error;
ALTER TABLE agent_bindings DROP COLUMN IF EXISTS main_pending;
