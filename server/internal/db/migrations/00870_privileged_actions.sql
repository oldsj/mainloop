-- +goose Up
-- Privileged actions (docs/specs/privileged-actions.md): an agent requests a credentialed action,
-- the owner approves it from one Needs-you card bound to a digest, and Mainloop's executor runs
-- exactly that action once. The row is the durable intent: what was asked (type, canonical args),
-- what preflight found (frozen_facts), the digest the owner's approval must name, the decision, the
-- execution and the delivery of the outcome to the requesting session.
-- State classes the table's cross-column checks use. Functions rather than inline lists, so each
-- class is named once; the state column's own CHECK lists every state.
-- +goose StatementBegin
CREATE FUNCTION privileged_action_unfinished(state text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$ SELECT state IN ('requested', 'pending_approval', 'approved', 'executing') $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION privileged_action_unfrozen_allowed(state text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$ SELECT state IN ('requested', 'preflight_failed', 'cancelled', 'expired') $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION privileged_action_past_approval(state text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$ SELECT state IN ('approved', 'executing', 'succeeded', 'failed', 'stale', 'uncertain') $$;
-- +goose StatementEnd

CREATE TABLE privileged_actions (
    id text PRIMARY KEY,
    owner_id text NOT NULL,
    -- The requester's idempotency key, per session: the same request_id returns the same action.
    request_id text NOT NULL CHECK (char_length(request_id) BETWEEN 1 AND 100),
    -- Who asked, resolved by the server from MCP authentication. Plain ids, so the record outlives
    -- a pruned session or task.
    requester_role text NOT NULL CHECK (char_length(requester_role) BETWEEN 1 AND 40),
    requester_session_id text NOT NULL,
    requester_task_id text,
    type text NOT NULL CHECK (type IN ('github.pr_merge', 'github.pr_close')),
    version integer NOT NULL CHECK (version >= 1),
    -- Canonical arguments, and the hash of their canonical JSON (one undecided action per session
    -- and arguments).
    args jsonb NOT NULL,
    args_key text NOT NULL CHECK (args_key ~ '^[0-9a-f]{64}$'),
    -- The agent's claim; shown labelled on the card, never part of what the digest means.
    reason text NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 2000),
    state text NOT NULL CHECK (state IN (
        'requested', 'pending_approval', 'preflight_failed', 'approved', 'executing',
        'succeeded', 'failed', 'stale', 'denied', 'expired', 'cancelled', 'uncertain'
    )),
    expires_at timestamptz NOT NULL,
    preflight_attempts integer NOT NULL DEFAULT 0 CHECK (preflight_attempts >= 0),
    -- Written once, by preflight; a trigger refuses any later change.
    frozen_facts jsonb,
    digest text CHECK (digest ~ '^[0-9a-f]{64}$'),
    -- The owner's decision. decision_note is the denial's text, delivered to the agent.
    decided_at timestamptz,
    decision_note text CHECK (octet_length(decision_note) <= 8192),
    -- The digest the approval carried; equal to digest on every approval.
    approved_digest text CHECK (approved_digest ~ '^[0-9a-f]{64}$'),
    -- Execution: claimed with a lease, at most a few attempts, each starting with an observe.
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    claimed_until timestamptz,
    next_attempt_at timestamptz,
    result jsonb,
    last_error text CHECK (octet_length(last_error) <= 4096),
    finished_at timestamptz,
    -- Delivery of the final outcome to the requesting session, once (the action id is the
    -- message identity).
    delivery_state text NOT NULL DEFAULT 'none'
        CHECK (delivery_state IN ('none', 'pending', 'delivered', 'undeliverable')),
    delivery_attempts integer NOT NULL DEFAULT 0 CHECK (delivery_attempts >= 0),
    delivery_next_at timestamptz,
    delivery_detail text CHECK (octet_length(delivery_detail) <= 1024),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (requester_session_id, request_id),
    CHECK ((frozen_facts IS NULL) = (digest IS NULL)),
    CHECK (privileged_action_unfrozen_allowed(state) OR digest IS NOT NULL),
    -- Every state past an approval carries the digest it approved.
    CHECK (NOT privileged_action_past_approval(state) OR approved_digest IS NOT NULL),
    CHECK (approved_digest IS NULL OR approved_digest = digest),
    CHECK (privileged_action_unfinished(state) = (finished_at IS NULL))
);
-- One undecided action per session and arguments: a repeat returns the open one.
CREATE UNIQUE INDEX privileged_actions_one_open ON privileged_actions (requester_session_id, args_key)
WHERE state IN ('requested', 'pending_approval');
CREATE INDEX privileged_actions_open ON privileged_actions (owner_id, state, created_at)
WHERE finished_at IS NULL;
CREATE INDEX privileged_actions_delivery ON privileged_actions (delivery_next_at)
WHERE delivery_state = 'pending';

-- frozen_facts and digest are immutable once written: the owner approves what preflight saw.
-- +goose StatementBegin
CREATE FUNCTION privileged_actions_freeze() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.digest IS NOT NULL AND (NEW.digest IS DISTINCT FROM OLD.digest
        OR NEW.frozen_facts IS DISTINCT FROM OLD.frozen_facts) THEN
        RAISE EXCEPTION 'privileged action % facts and digest are immutable', OLD.id;
    END IF;
    IF NEW.args IS DISTINCT FROM OLD.args OR NEW.type IS DISTINCT FROM OLD.type
        OR NEW.version IS DISTINCT FROM OLD.version THEN
        RAISE EXCEPTION 'privileged action % type and args are immutable', OLD.id;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER privileged_actions_freeze BEFORE UPDATE ON privileged_actions
FOR EACH ROW EXECUTE FUNCTION privileged_actions_freeze();

-- The append-only audit: requested, preflighted, decided, claimed, executed, observed, delivered.
CREATE TABLE privileged_action_events (
    id bigserial PRIMARY KEY,
    action_id text NOT NULL REFERENCES privileged_actions (id) ON DELETE CASCADE,
    step text NOT NULL CHECK (char_length(step) BETWEEN 1 AND 40),
    detail text NOT NULL CHECK (octet_length(detail) <= 4096),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX privileged_action_events_action ON privileged_action_events (action_id, id);

-- +goose StatementBegin
CREATE FUNCTION privileged_action_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'privileged_action_events is append-only';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER privileged_action_events_append_only BEFORE UPDATE ON privileged_action_events
FOR EACH ROW EXECUTE FUNCTION privileged_action_events_append_only();

-- +goose Down
DROP TABLE IF EXISTS privileged_action_events;
DROP FUNCTION IF EXISTS privileged_action_events_append_only();
DROP TABLE IF EXISTS privileged_actions;
DROP FUNCTION IF EXISTS privileged_actions_freeze();
DROP FUNCTION IF EXISTS privileged_action_unfinished(text);
DROP FUNCTION IF EXISTS privileged_action_unfrozen_allowed(text);
DROP FUNCTION IF EXISTS privileged_action_past_approval(text);
