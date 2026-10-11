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

-- Trusted template/tool associations learned by the observer. Proposals are created by the later
-- merge migration, so proposal_id has no foreign key here. Queries also check owner/session scope.
CREATE TABLE hitl_merge_tools (
    request_id text NOT NULL REFERENCES hitl_requests (id) ON DELETE CASCADE,
    tool_id text NOT NULL,
    proposal_id text NOT NULL,
    PRIMARY KEY (request_id, tool_id)
);
CREATE INDEX hitl_merge_tools_proposal ON hitl_merge_tools (proposal_id);

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
    reason_code text CHECK (reason_code IN ('reconciliation', 'runtime_stop_unresolved', 'merge_approval')),
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

-- One current need per task, even if a cancelled task or an older attempt still holds a runtime.
-- Stop evidence is historical: a confirmed stop clears its need without deleting the evidence.
CREATE VIEW task_reconciliation_needs AS
SELECT t.id AS task_id, t.owner_id, t.title,
       CASE WHEN EXISTS (
           SELECT 1 FROM task_attempts a WHERE a.task_id = t.id
             AND 'runtime-stop-unresolved' = ANY(a.evidence_refs) AND a.stop_state <> 'confirmed'
       ) THEN 'runtime_stop_unresolved' ELSE 'reconciliation' END AS reason_code
FROM tasks t
WHERE (t.status = 'blocked' AND t.reason = 'reconciliation') OR EXISTS (
    SELECT 1 FROM task_attempts a WHERE a.task_id = t.id
      AND 'runtime-stop-unresolved' = ANY(a.evidence_refs) AND a.stop_state <> 'confirmed'
);

-- The one visibility rule (G9b): the open attention items the owner should see now. The snapshot,
-- its count, the unread count and a session's needs_owner all read this view, so they cannot
-- disagree. An open item is hidden when
--   * its session is archived or finished;
--   * a HITL request already has a live answer (recorded, sending, accepted or uncertain), so
--     an answered request does not prompt again while the answer travels;
--   * its session belongs to a task attempt that is not the task's current, unfinished attempt
--     (replaced, ended, archived or its native session deleted), or its task is final.
-- The task join is read-only. A HITL item's task_id is kagent's A2A task, so only a queue or
-- task-decision item uses its own task_id.
CREATE VIEW attention_visible AS
SELECT a.id, a.owner_id, a.source_kind, a.session_id, a.read_at
FROM attention_items a
LEFT JOIN sessions s ON s.id = a.session_id
LEFT JOIN task_attempts ta ON ta.session_id = a.session_id
LEFT JOIN tasks t ON t.id = CASE
    WHEN ta.id IS NOT NULL THEN ta.task_id
    WHEN a.source_kind <> 'hitl_request' THEN a.task_id
END
WHERE a.state = 'open'
  AND (a.source_kind <> 'task_reconciliation' OR EXISTS (
      SELECT 1 FROM task_reconciliation_needs n WHERE n.task_id = a.task_id AND n.owner_id = a.owner_id))
  -- Proposal items render their entire native request. Suppress a generic duplicate while one is
  -- open; pure merge requests stay suppressed after their proposals end. A mixed batch's other
  -- input becomes visible again when its last proposal item resolves.
  AND (a.source_kind <> 'hitl_request' OR NOT EXISTS (
      SELECT 1 FROM attention_items m WHERE m.source_kind = 'merge_proposal' AND m.state = 'open'
        AND m.owner_id = a.owner_id AND m.detail->>'hitl_request_id' = a.source_id))
  AND (a.source_kind <> 'hitl_request' OR NOT EXISTS (
      SELECT 1 FROM hitl_requests r WHERE r.id = a.source_id
        AND r.kind = 'tool_approval' AND r.payload->>'nested' IS NULL
        AND jsonb_array_length(r.payload->'tools') > 0
        AND jsonb_array_length(r.payload->'tools') = (
            SELECT count(*) FROM hitl_merge_tools m WHERE m.request_id = r.id)))
  AND (a.source_kind <> 'merge_proposal' OR NOT EXISTS (
      SELECT 1 FROM hitl_responses r WHERE r.request_id = a.detail->>'hitl_request_id'
        AND r.state IN ('recorded', 'sending', 'accepted', 'uncertain')))
  AND (a.session_id IS NULL OR (s.archived_at IS NULL AND s.status NOT IN ('completed', 'failed', 'cancelled', 'superseded')))
  AND (a.source_kind <> 'hitl_request' OR NOT EXISTS (
      SELECT 1 FROM hitl_responses r
      WHERE r.request_id = a.source_id AND r.state IN ('recorded', 'sending', 'accepted', 'uncertain')))
  AND (a.source_kind = 'task_reconciliation' OR t.id IS NULL OR t.status NOT IN ('completed', 'failed', 'cancelled'))
  AND (ta.id IS NULL OR (
      ta.archived_at IS NULL AND ta.native_deleted_at IS NULL
      AND ta.state IN ('creating', 'active', 'draining') AND t.current_attempt_id = ta.id));

-- +goose Down
DROP VIEW IF EXISTS attention_visible;
DROP VIEW IF EXISTS task_reconciliation_needs;
DROP TABLE IF EXISTS attention_items;
DROP TABLE IF EXISTS hitl_diagnostics;
DROP TABLE IF EXISTS hitl_responses;
DROP TABLE IF EXISTS hitl_merge_tools;
DROP TABLE IF EXISTS hitl_requests;
DROP TABLE IF EXISTS hitl_sessions;
