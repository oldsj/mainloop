-- Human-in-the-loop: the observer's checkpoints and requests, and the response ledger
-- (internal/hitl).
--
-- The "fenced" response queries record a claimer's outcome: they match only the row's current
-- claim (id, lease_owner, attempts) and report rows affected, so a claimer that lost its lease
-- changes nothing. The ones that move to a final state also release the lease.

-- The bound kagent sessions the observer has not adopted yet, with what the sessions package needs
-- to resolve the Agent that runs each.
-- name: ListHITLBindingsToAdopt :many
SELECT b.session_id, b.kagent_session_id, s.user_id, b.kind, b.role,
       coalesce(b.kagent_agent_namespace, w.agent_namespace, '') AS resolved_agent_namespace,
       coalesce(b.kagent_agent_name, w.agent_name, '') AS resolved_agent_name
FROM agent_bindings b
JOIN sessions s ON s.id = b.session_id
LEFT JOIN workspaces w ON w.id = b.session_id
WHERE b.kagent_session_id IS NOT NULL AND s.archived_at IS NULL AND s.user_id = $1
  AND NOT EXISTS (SELECT 1 FROM hitl_sessions h WHERE h.kagent_session_id = b.kagent_session_id)
ORDER BY b.session_id;

-- name: InsertHITLSession :exec
INSERT INTO hitl_sessions (kagent_session_id, session_id, owner_id, agent_namespace, agent_name, context_id)
VALUES ($1, $2, $3, $4, $5, $1)
ON CONFLICT (kagent_session_id) DO NOTHING;

-- A session whose binding no longer names its kagent session (replaced or removed) is not scanned.
-- name: DeleteStaleHITLSessions :execrows
DELETE FROM hitl_sessions h
WHERE h.owner_id = $1
  AND NOT EXISTS (
      SELECT 1 FROM agent_bindings b
      WHERE b.session_id = h.session_id AND b.kagent_session_id = h.kagent_session_id
  );

-- name: ListHITLSessionsToScan :many
SELECT hs.* FROM hitl_sessions hs
JOIN sessions s ON s.id = hs.session_id AND s.archived_at IS NULL
WHERE hs.owner_id = $1
  AND (hs.scanned_at IS NULL OR hs.scanned_at <= now() - make_interval(secs => sqlc.arg(every_seconds)::float8))
  AND (hs.next_scan_at IS NULL OR hs.next_scan_at <= now())
ORDER BY hs.scanned_at NULLS FIRST, hs.kagent_session_id
LIMIT sqlc.arg(max_sessions);

-- name: SetHITLSessionScan :exec
-- A scan that is not done keeps its old scanned_at, so the session is picked again at once. A page
-- that was read clears the session's failure backoff.
UPDATE hitl_sessions
SET task_cursor = sqlc.narg(cursor), updated_at = now(), failures = 0, next_scan_at = NULL,
    scanned_at = CASE WHEN sqlc.arg(done)::boolean THEN now() ELSE scanned_at END
WHERE kagent_session_id = sqlc.arg(kagent_session_id);

-- name: FailHITLSessionScan :exec
-- This session alone failed: leave it alone for a while, longer after each failure in a row.
UPDATE hitl_sessions
SET failures = failures + 1, updated_at = now(),
    next_scan_at = now() + make_interval(secs => sqlc.arg(delay_seconds)::float8)
WHERE kagent_session_id = sqlc.arg(kagent_session_id);

-- name: ListHITLToRefresh :many
SELECT * FROM hitl_requests
WHERE owner_id = $1 AND availability IN ('pending', 'stale')
  AND attempted_at <= now() - make_interval(secs => sqlc.arg(every_seconds)::float8)
ORDER BY attempted_at, id
LIMIT sqlc.arg(max_requests);

-- name: GetHITLRequest :one
SELECT * FROM hitl_requests WHERE id = $1 AND owner_id = $2;

-- name: LockHITLRequest :one
SELECT * FROM hitl_requests WHERE id = $1 AND owner_id = $2 FOR UPDATE;

-- name: ListSessionHITLRequests :many
SELECT id FROM (
    SELECT id, created_at FROM hitl_requests
    WHERE session_id = $1 AND owner_id = $2
    ORDER BY created_at DESC, id DESC
    LIMIT $3
) newest
ORDER BY created_at, id;

-- name: InsertHITLRequest :execrows
INSERT INTO hitl_requests (
    id, owner_id, session_id, kagent_session_id, agent_namespace, agent_name, context_id, task_id,
    status_message_id, request_hash, kind, payload
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (id) DO NOTHING;

-- name: SupersedeLiveHITLForTask :many
UPDATE hitl_requests
SET availability = 'unavailable', unavailable_reason = 'A newer request replaced this one',
    updated_at = now()
WHERE kagent_session_id = $1 AND task_id = $2 AND id <> $3 AND availability IN ('pending', 'stale')
RETURNING id;

-- name: ConfirmHITLRequest :exec
UPDATE hitl_requests
SET availability = 'pending', unavailable_reason = NULL, checked_at = now(), attempted_at = now(),
    updated_at = now()
WHERE id = $1 AND availability IN ('pending', 'stale');

-- name: AttemptHITLRequest :exec
UPDATE hitl_requests SET attempted_at = now() WHERE id = $1;

-- name: SetHITLAvailability :execrows
UPDATE hitl_requests
SET availability = sqlc.arg(availability), unavailable_reason = sqlc.narg(reason),
    attempted_at = now(), updated_at = now()
WHERE id = sqlc.arg(id) AND availability = ANY(sqlc.arg(from_states)::text[]);

-- name: GetHITLContext :one
SELECT s.title, b.role, b.kind AS provider, p.id AS project_id, p.name AS project_name
FROM sessions s
LEFT JOIN agent_bindings b ON b.session_id = s.id
LEFT JOIN projects p ON p.id = s.project_id
WHERE s.id = $1 AND s.user_id = $2;

-- name: UpsertHITLAttention :exec
INSERT INTO attention_items (id, owner_id, kind, source_kind, source_id, session_id, task_id, title, needed, href)
VALUES ($1, $2, $3, 'hitl_request', $4, $5, $6, $7, $8, $9)
ON CONFLICT (source_kind, source_id) DO NOTHING;

-- name: ResolveHITLAttention :execrows
UPDATE attention_items
SET state = 'resolved', resolved_at = now(), updated_at = now()
WHERE source_kind = 'hitl_request' AND source_id = $1 AND state = 'open';

-- The unread count is the visible items the owner has not read (see attention_visible).
-- name: CountUnreadAttention :one
SELECT count(*)::int FROM attention_visible v WHERE v.owner_id = $1 AND v.read_at IS NULL;

-- name: InsertHITLDiagnostic :exec
INSERT INTO hitl_diagnostics (owner_id, runtime_session_id, detail)
SELECT $1, $2, $3
WHERE NOT EXISTS (
    SELECT 1 FROM hitl_diagnostics d
    WHERE d.owner_id = $1 AND d.runtime_session_id = $2 AND d.detail = $3
      AND d.observed_at > now() - make_interval(secs => sqlc.arg(repeat_seconds)::float8)
);

-- name: PruneHITLDiagnostics :exec
DELETE FROM hitl_diagnostics AS d
WHERE d.owner_id = $1 AND d.id NOT IN (
    SELECT n.id FROM hitl_diagnostics n WHERE n.owner_id = $1 ORDER BY n.id DESC LIMIT 100
);

-- name: ListHITLDiagnostics :many
SELECT runtime_session_id, detail, observed_at
FROM hitl_diagnostics WHERE owner_id = $1 ORDER BY id DESC LIMIT 100;

-- name: HITLObserverStats :one
SELECT count(*)::int AS pending, coalesce(min(checked_at), now())::timestamptz AS oldest_check
FROM hitl_requests WHERE owner_id = $1 AND availability = 'pending';

-- name: GetHITLResponse :one
SELECT * FROM hitl_responses WHERE id = $1 AND owner_id = $2;

-- name: GetLatestHITLResponse :one
SELECT * FROM hitl_responses
WHERE request_id = $1 AND owner_id = $2
ORDER BY (state <> 'rejected_transport') DESC, created_at DESC, id DESC
LIMIT 1;

-- name: GetLiveHITLResponse :one
SELECT * FROM hitl_responses
WHERE request_id = $1 AND state IN ('recorded', 'sending', 'accepted', 'uncertain');

-- name: InsertHITLResponse :exec
INSERT INTO hitl_responses (
    id, request_id, owner_id, idempotency_key, decision, decision_hash, outbound_message_id, receipt
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: GetHITLResponseWork :one
SELECT r.id, r.request_id, r.decision, r.outbound_message_id, r.state, r.owner_id,
       q.kagent_session_id, q.agent_namespace, q.agent_name, q.context_id, q.task_id,
       q.status_message_id, q.request_hash, q.availability, q.session_id,
       w.id AS workspace_id
FROM hitl_responses r
JOIN hitl_requests q ON q.id = r.request_id
LEFT JOIN workspaces w ON w.id = q.session_id
WHERE r.id = $1;

-- name: MarkHITLResponseSending :execrows
UPDATE hitl_responses
SET state = 'sending', detail = NULL, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state = 'recorded';

-- name: MarkHITLResponseUncertain :one
-- updated_at is when the outcome first became unknown, so a recheck keeps it. rechecks counts the
-- lookups since then, for the delay before the next one; no row means the claim was lost. updated_at
-- is returned so the delay can be cut to the hold time that remains.
UPDATE hitl_responses
SET state = 'uncertain', detail = $4,
    rechecks = CASE WHEN state = 'uncertain' THEN rechecks + 1 ELSE 0 END,
    updated_at = CASE WHEN state = 'uncertain' THEN updated_at ELSE now() END
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state IN ('recorded', 'sending', 'uncertain')
RETURNING rechecks, updated_at;

-- name: FinishHITLResponse :execrows
UPDATE hitl_responses
SET state = $4, detail = $5, lease_owner = NULL, lease_until = NULL, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3;

-- name: SetHITLResponseWaiting :execrows
-- The send has not started: say why the response is still waiting, and stay recorded.
UPDATE hitl_responses
SET detail = $4, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state = 'recorded';

-- name: MarkHITLResponseRecorded :execrows
-- The send is proven not to have left: back to recorded for another try.
UPDATE hitl_responses
SET state = 'recorded', detail = $4, updated_at = now()
WHERE id = $1 AND lease_owner = $2 AND attempts = $3 AND state = 'sending';

-- The proposal context is scoped to the native request's owner, session, current runtime and
-- project. A native payload naming another session's proposal is not a display association.
-- name: GetHITLMergeProposal :one
SELECT p.id AS proposal_id, p.presentation, p.summary_digest, c.state, c.deadline, b.kind AS provider
FROM merge_proposals p
JOIN merge_candidates c ON c.id = p.candidate_id AND c.owner_id = p.owner_id
JOIN sessions s ON s.id = p.session_id AND s.user_id = p.owner_id AND s.project_id = c.project_id
JOIN agent_bindings b ON b.session_id = s.id
WHERE p.id = $1 AND p.owner_id = $2 AND p.session_id = $3 AND b.kagent_session_id = $4
  AND b.kagent_deleted_at IS NULL AND c.route = 'approval';

-- name: InsertHITLMergeTool :execrows
INSERT INTO hitl_merge_tools (request_id, tool_id, proposal_id)
VALUES ($1, $2, $3)
ON CONFLICT (request_id, tool_id) DO NOTHING;
