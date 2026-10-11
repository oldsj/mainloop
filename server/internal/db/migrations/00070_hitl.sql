-- +goose Up
-- Human-in-the-loop (G9a): what the observer sees in kagent, what the owner answered, and the
-- inbox rows both leave behind. The Python backend spread this over twelve tables (requests,
-- aliases, leaf and association snapshots, receipts, observer bindings and checkpoints, queue
-- items, main threads). The leaf-only model here needs five.
--
-- Design choices:
--
--   * Ownership is the session binding. A kagent session is adopted only when an agent_bindings
--     row names it, and the owner is that session's user. The row stores the Agent that runs the
--     session (kagent scopes a conversation to its Agent), resolved by the sessions package. A
--     row whose binding no longer names its kagent session is deleted. There is no creator check
--     or alias table: nested (subagent) requests, associations and merge enrichment arrive with G10, which
--     adds its own tables beside these ones.
--   * A request's id is derived from its outer identity (gateway, session, task, status message,
--     request hash), so observing the same ask twice is a no-op, and a changed ask is a new row.
--     A task has at most one live request (pending or stale); a newer ask supersedes the older.
--   * A response is an intent row for the reconciler (see internal/reconcile): the same columns as
--     deliveries (state, idempotency key, attempts, lease). The receipt is written with the row and
--     never changes, so a duplicate POST reads back the same receipt. A request has at most one live
--     decision; a rejected_transport one frees it for another attempt.
--   * Availability and transport are separate columns of separate tables. A request being answered
--     (availability) follows from a response being accepted (transport), but neither implies the
--     other while the outcome is uncertain.
--   * checked_at is the last successful look at kagent; attempted_at is the last try. A request goes
--     stale only when it has not been confirmed for a while, so a kagent blip does not flip the inbox.
--   * attention_items replaces queue_items and main_threads: one row per thing that needs the owner,
--     keyed by (source_kind, source_id). Three sources write it: 'hitl_request' (the hitl package),
--     'task_decision' (the attention sweep, from tasks.decision_*; source_id is the report that asked)
--     and 'queue' (internal/attention Open, for owner questions raised by other slices; detail holds
--     item_type, priority, content, context, options and, once answered, response). source_kind is
--     not constrained here. Read state is presentation only (read_at) and never resolves an item.
--
-- State machines (the hitl package owns the transitions):
--   hitl_requests.availability
--     pending      the task is awaiting input and the ask is valid; answerable
--     stale        kagent has not confirmed the ask for a while; shown, not answerable, reopens on a read
--     unavailable  the task moved on, the ask is malformed, or kagent no longer has the task (final)
--     answered     a response was accepted (final)
--   hitl_responses.state
--     recorded           written; nothing has been sent
--     sending            the claimer is calling kagent; the outcome is not known yet
--     accepted           kagent shows the response message in the task (final)
--     uncertain          the send may or may not have reached kagent; resolved by looking the message
--                        up, never sent again
--     rejected_transport final: not sent, or kagent refused it, or the ask changed first
--   attention_items.state
--     open, resolved (the need ended), dismissed (the owner put it away)

-- Which Mainloop sessions the observer watches, with the per-session task-scan checkpoint. A row is
-- added from agent_bindings (ownership is the binding), so kagent is only asked about tasks.
CREATE TABLE hitl_sessions (
    kagent_session_id text PRIMARY KEY,
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    owner_id text NOT NULL,
    agent_namespace text NOT NULL,
    agent_name text NOT NULL,
    context_id text NOT NULL,
    -- The page token of the next task listing; NULL starts from the first page.
    task_cursor text,
    -- NULL until the first scan; the scan order is least recently scanned first.
    scanned_at timestamptz,
    -- A scan that failed for this session alone backs the session off: consecutive failures and
    -- the time before which it is not scanned again. A success clears both.
    failures integer NOT NULL DEFAULT 0,
    next_scan_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX hitl_sessions_scan ON hitl_sessions (scanned_at NULLS FIRST, kagent_session_id);
CREATE INDEX hitl_sessions_session ON hitl_sessions (session_id);

CREATE TABLE hitl_requests (
    id text PRIMARY KEY,
    owner_id text NOT NULL,
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    kagent_session_id text NOT NULL,
    agent_namespace text NOT NULL,
    agent_name text NOT NULL,
    context_id text NOT NULL,
    task_id text NOT NULL,
    status_message_id text NOT NULL,
    -- sha256 of the canonical request JSON.
    request_hash text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('tool_approval', 'ask_user')),
    -- The request exactly as kagent sent it (validated, at most 128 KiB).
    payload jsonb NOT NULL,
    availability text NOT NULL DEFAULT 'pending' CHECK (
        availability IN ('pending', 'stale', 'unavailable', 'answered')
    ),
    unavailable_reason text,
    observed_at timestamptz NOT NULL DEFAULT now(),
    checked_at timestamptz NOT NULL DEFAULT now(),
    attempted_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
-- One live ask per task: the backstop for the supersede step.
CREATE UNIQUE INDEX hitl_requests_one_live ON hitl_requests (kagent_session_id, task_id)
WHERE availability IN ('pending', 'stale');
CREATE INDEX hitl_requests_session ON hitl_requests (session_id, created_at, id);
CREATE INDEX hitl_requests_refresh ON hitl_requests (attempted_at, id)
WHERE availability IN ('pending', 'stale');

CREATE TABLE hitl_responses (
    -- The client's action id.
    id text PRIMARY KEY,
    request_id text NOT NULL REFERENCES hitl_requests (id) ON DELETE CASCADE,
    owner_id text NOT NULL,
    idempotency_key text NOT NULL UNIQUE,
    -- The native response as it is sent, and its canonical hash (tells a repeat of the same action
    -- from a different decision under the same action id).
    decision jsonb NOT NULL,
    decision_hash text NOT NULL,
    -- The A2A messageId of the response; the only handle for finding it in the task again.
    outbound_message_id text NOT NULL UNIQUE,
    -- The immutable receipt, written with the row.
    receipt jsonb NOT NULL,
    state text NOT NULL DEFAULT 'recorded' CHECK (
        state IN ('recorded', 'sending', 'accepted', 'uncertain', 'rejected_transport')
    ),
    detail text,
    attempts integer NOT NULL DEFAULT 0,
    -- Lookups made while uncertain, so the recheck delay can grow.
    rechecks integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
-- One live decision per request.
CREATE UNIQUE INDEX hitl_responses_one_live ON hitl_responses (request_id)
WHERE state IN ('recorded', 'sending', 'accepted', 'uncertain');
-- The claim index: only the states a loop works on.
CREATE INDEX hitl_responses_claim ON hitl_responses (next_attempt_at, id)
WHERE state IN ('recorded', 'sending', 'uncertain');

-- Why the observer could not look at something. The newest 100 are kept.
CREATE TABLE hitl_diagnostics (
    id bigserial PRIMARY KEY,
    owner_id text NOT NULL,
    runtime_session_id text NOT NULL,
    detail text NOT NULL,
    observed_at timestamptz NOT NULL DEFAULT now(),
    -- The owner put the notice away (G9b). A dismissed notice leaves Activity and is never shown again.
    dismissed_at timestamptz
);
CREATE INDEX hitl_diagnostics_owner ON hitl_diagnostics (owner_id, id DESC);

CREATE TABLE attention_items (
    id text PRIMARY KEY,
    owner_id text NOT NULL,
    -- The contract's AttentionItem kind.
    kind text NOT NULL CHECK (kind IN ('approval', 'question', 'owner_decision')),
    source_kind text NOT NULL,
    source_id text NOT NULL,
    session_id text REFERENCES sessions (id) ON DELETE CASCADE,
    task_id text,
    title text NOT NULL,
    needed text NOT NULL,
    href text NOT NULL,
    state text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'resolved', 'dismissed')),
    -- What a source needs beyond the columns above (G9b: queue content, context, options).
    detail jsonb NOT NULL DEFAULT '{}',
    read_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    UNIQUE (source_kind, source_id)
);
CREATE INDEX attention_items_open ON attention_items (owner_id, created_at DESC, id)
WHERE state = 'open';

-- +goose Down
DROP TABLE IF EXISTS attention_items;
DROP TABLE IF EXISTS hitl_diagnostics;
DROP TABLE IF EXISTS hitl_responses;
DROP TABLE IF EXISTS hitl_requests;
DROP TABLE IF EXISTS hitl_sessions;
