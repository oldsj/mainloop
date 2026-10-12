-- The owner's attention: what needs them now (attention_items) and the Activity log around it.
-- Every read of "what the owner should see" goes through the attention_visible view (migration
-- 00070), the one visibility rule: the list, the unread count and a session's needs_owner cannot
-- disagree.

-- name: ListOpenAttention :many
SELECT a.*
FROM attention_items a
JOIN attention_visible v ON v.id = a.id
WHERE v.owner_id = $1
ORDER BY a.created_at DESC, a.id;

-- The sessions that need the owner, in one read: those with a visible item, and whether one of
-- them is a native input request (the session then reads waiting_on_user).
-- name: ListSessionsNeedingOwner :many
SELECT v.session_id::text AS session_id, bool_or(v.source_kind IN ('hitl_request', 'merge_proposal'))::boolean AS waiting_on_input
FROM attention_visible v
WHERE v.owner_id = $1 AND v.session_id IS NOT NULL
GROUP BY v.session_id;

-- Whether a session has an open item the owner can see (the needs_owner rule).
-- name: SessionNeedsOwner :one
SELECT EXISTS (
    SELECT 1 FROM attention_visible v WHERE v.owner_id = $1 AND v.session_id = $2
) AS needs_owner;

-- name: GetAttentionBySource :one
SELECT * FROM attention_items WHERE owner_id = $1 AND source_kind = $2 AND source_id = $3;

-- name: InsertQueueAttention :execrows
INSERT INTO attention_items (id, owner_id, kind, source_kind, source_id, session_id, task_id, title, needed, href, detail)
VALUES ($1, $2, $3, 'queue', $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (source_kind, source_id) DO NOTHING;

-- The owner's answer. It matches only an open queue item, so a second answer changes nothing and
-- the caller reads the row to tell a repeat from a conflict.
-- name: RespondQueueAttention :one
UPDATE attention_items
SET state = 'resolved', resolved_at = now(), updated_at = now(), read_at = coalesce(read_at, now()),
    detail = detail || jsonb_build_object(
        'response', sqlc.arg(response)::text, 'responded_at', now(), 'metadata', sqlc.arg(metadata)::jsonb
    )
WHERE owner_id = sqlc.arg(owner_id) AND source_kind = 'queue' AND source_id = sqlc.arg(source_id) AND state = 'open'
RETURNING id;

-- name: DismissNotice :one
UPDATE hitl_diagnostics SET dismissed_at = now()
WHERE owner_id = $1 AND id = $2 AND dismissed_at IS NULL
RETURNING id;

-- name: GetNotice :one
SELECT id, dismissed_at FROM hitl_diagnostics WHERE owner_id = $1 AND id = $2;

-- The sweep. Open an item for each task that waits for the owner's decision. The decision belongs
-- to the task's current, unfinished attempt; the report that asked is the item's identity, so a
-- decision that was answered is never reopened and a new ask is a new item.
-- name: OpenTaskDecisionAttention :many
INSERT INTO attention_items (id, owner_id, kind, source_kind, source_id, session_id, task_id, title, needed, href)
SELECT 'attn_task_' || t.decision_report_id, t.owner_id, 'owner_decision', 'task_decision', t.decision_report_id,
       s.id, t.id, t.title, t.decision_action, '/tasks/' || t.id
FROM tasks t
JOIN task_attempts at ON at.id = t.decision_attempt_id AND at.task_id = t.id
LEFT JOIN sessions s ON s.id = at.session_id
WHERE t.owner_id = $1 AND t.status = 'blocked' AND t.reason = 'owner_decision'
  AND t.decision_report_id IS NOT NULL AND t.current_attempt_id = t.decision_attempt_id
  AND at.state IN ('creating', 'active', 'draining')
ON CONFLICT (source_kind, source_id) DO NOTHING
RETURNING id;

-- name: ResolveStaleTaskDecisions :many
UPDATE attention_items a
SET state = 'resolved', resolved_at = now(), updated_at = now()
WHERE a.owner_id = $1 AND a.source_kind = 'task_decision' AND a.state = 'open'
  AND NOT EXISTS (
      SELECT 1
      FROM tasks t
      JOIN task_attempts at ON at.id = t.decision_attempt_id AND at.task_id = t.id
      WHERE t.id = a.task_id AND t.owner_id = a.owner_id AND t.decision_report_id = a.source_id
        AND t.status = 'blocked' AND t.reason = 'owner_decision' AND t.current_attempt_id = t.decision_attempt_id
        AND at.state IN ('creating', 'active', 'draining')
  )
RETURNING a.id;

-- A question put to the owner for a task that has finished is moot.
-- name: ResolveQueueForFinishedTasks :many
UPDATE attention_items a
SET state = 'resolved', resolved_at = now(), updated_at = now()
WHERE a.owner_id = $1 AND a.source_kind = 'queue' AND a.state = 'open' AND a.task_id IS NOT NULL
  AND a.detail->>'item_type' IN ('question', 'approval', 'review', 'plan_review')
  AND EXISTS (
      SELECT 1 FROM tasks t
      WHERE t.id = a.task_id AND t.owner_id = a.owner_id AND t.status IN ('completed', 'failed', 'cancelled')
  )
RETURNING a.id;

-- Activity: provider reports, answered queue items and input notices, each capped in SQL.
-- name: ListActivityReports :many
SELECT r.id, r.summary, r.outcome, r.created_at, t.id AS task_id, t.title
FROM task_reports r
JOIN tasks t ON t.id = r.task_id
WHERE t.owner_id = $1
ORDER BY r.created_at DESC, r.id DESC
LIMIT 100;

-- name: ListActivityQueue :many
SELECT source_id, title, needed, href, detail, created_at, coalesce(resolved_at, updated_at)::timestamptz AS happened_at
FROM attention_items
WHERE owner_id = $1 AND source_kind = 'queue' AND state <> 'open'
ORDER BY coalesce(resolved_at, updated_at) DESC, id DESC
LIMIT 100;

-- name: ListActivityNotices :many
SELECT id, detail, observed_at
FROM hitl_diagnostics
WHERE owner_id = $1 AND dismissed_at IS NULL
ORDER BY id DESC
LIMIT 100;

-- An item of a session that has finished or been archived can never come back: close it, so the
-- row does not wait for a session that is gone.
-- name: ResolveFinishedSessionAttention :many
UPDATE attention_items a
SET state = 'resolved', resolved_at = now(), updated_at = now()
FROM sessions s
WHERE s.id = a.session_id AND a.owner_id = $1 AND a.state = 'open'
  AND (s.archived_at IS NOT NULL OR s.status IN ('completed', 'failed', 'cancelled', 'superseded'))
RETURNING a.id;

-- An item hidden for good by its task or attempt can never come back: close it. This mirrors the
-- task and attempt terms of attention_visible, and only those: a HITL item hidden by a live answer
-- (including a rejected one, which shows the request again) is not touched, nor is a session term,
-- which ResolveFinishedSessionAttention closes. An attempt that is still being created may not be
-- the task's current attempt yet, so "not current" counts only once it has left creating.
-- name: ResolveAttentionHiddenForGood :many
UPDATE attention_items a
SET state = 'resolved', resolved_at = now(), updated_at = now()
FROM (
    SELECT i.id
    FROM attention_items i
    LEFT JOIN task_attempts ta ON ta.session_id = i.session_id
    LEFT JOIN tasks t ON t.id = CASE
        WHEN ta.id IS NOT NULL THEN ta.task_id
        WHEN i.source_kind <> 'hitl_request' THEN i.task_id
    END
    WHERE i.owner_id = $1 AND i.state = 'open' AND i.source_kind <> 'task_reconciliation'
      AND (
          t.status IN ('completed', 'failed', 'cancelled')
          OR ta.archived_at IS NOT NULL OR ta.native_deleted_at IS NOT NULL
          OR ta.state NOT IN ('creating', 'active', 'draining')
          OR (ta.state <> 'creating' AND t.current_attempt_id IS DISTINCT FROM ta.id)
      )
) hidden
WHERE a.id = hidden.id
RETURNING a.id;

-- The unread count the owner's newest inbox:updated carried, or -1 when there is none (never
-- published, pruned, or without a count), which no real count equals.
-- name: LastPublishedUnread :one
SELECT coalesce(
    (SELECT (e.payload->>'unread_count')::int
     FROM events e
     WHERE e.user_id = $1 AND e.type = 'inbox:updated'
     ORDER BY e.id DESC
     LIMIT 1),
    -1)::int AS unread_count;

-- Reopen the same task item only when a need recurs; unchanged sweeps write nothing.
-- name: OpenTaskReconciliationAttention :many
INSERT INTO attention_items AS a (id, owner_id, kind, source_kind, source_id, task_id, title, needed, href, reason_code)
SELECT 'attn_reconciliation_' || n.task_id, n.owner_id, 'owner_decision', 'task_reconciliation', n.task_id,
       n.task_id, n.title,
       CASE n.reason_code WHEN 'runtime_stop_unresolved' THEN 'Runtime stop remains unconfirmed; reconcile the held claim and capacity.'
       ELSE 'Task needs reconciliation; inspect its attempt evidence.' END,
       '/tasks/' || n.task_id, n.reason_code
FROM task_reconciliation_needs n WHERE n.owner_id = $1
ON CONFLICT (source_kind, source_id) DO UPDATE
SET state = 'open', resolved_at = NULL, updated_at = now(),
    read_at = CASE WHEN a.state <> 'open' THEN NULL ELSE a.read_at END,
    title = EXCLUDED.title, needed = EXCLUDED.needed, reason_code = EXCLUDED.reason_code
WHERE a.state <> 'open' OR (a.title, a.needed, a.reason_code) IS DISTINCT FROM (EXCLUDED.title, EXCLUDED.needed, EXCLUDED.reason_code)
RETURNING id;

-- name: ResolveTaskReconciliationAttention :many
UPDATE attention_items a SET state = 'resolved', resolved_at = now(), updated_at = now()
WHERE a.owner_id = $1 AND a.source_kind = 'task_reconciliation' AND a.state = 'open'
  AND NOT EXISTS (SELECT 1 FROM task_reconciliation_needs n WHERE n.task_id = a.task_id AND n.owner_id = a.owner_id)
RETURNING a.id;

-- One proposal item linked to its latest live native approval request. The observer establishes
-- the trusted association; this sweep performs no runtime or GitHub I/O.
-- name: OpenMergeProposalAttention :many
INSERT INTO attention_items AS a (id, owner_id, kind, source_kind, source_id, session_id, title, needed, href, reason_code, detail)
SELECT DISTINCT ON (p.id) 'attn_merge_' || p.id, p.owner_id, 'approval', 'merge_proposal', p.id, r.session_id,
       coalesce(p.presentation->>'title', 'Merge proposal'),
       'Approve merge of ' || c.repository || ' #' || c.pr_number::text,
       '/sessions/' || r.session_id || '?hitl=' || r.id, 'merge_approval',
       jsonb_build_object('hitl_request_id', r.id)
FROM hitl_merge_tools m
JOIN hitl_requests r ON r.id = m.request_id
JOIN merge_proposals p ON p.id = m.proposal_id AND p.owner_id = r.owner_id AND p.session_id = r.session_id
JOIN merge_candidates c ON c.id = p.candidate_id AND c.owner_id = p.owner_id
JOIN sessions s ON s.id = r.session_id AND s.user_id = r.owner_id
JOIN agent_bindings b ON b.session_id = r.session_id AND b.kagent_session_id = r.kagent_session_id AND b.kagent_deleted_at IS NULL
WHERE r.owner_id = $1 AND r.availability IN ('pending', 'stale') AND c.route = 'approval' AND c.state = 'prepared'
  AND (c.deadline IS NULL OR c.deadline > now())
  AND s.archived_at IS NULL AND s.status NOT IN ('completed', 'failed', 'cancelled', 'superseded')
  AND s.project_id = c.project_id
  AND NOT EXISTS (
      SELECT 1 FROM task_attempts at JOIN tasks t ON t.id = at.task_id
      WHERE at.session_id = r.session_id AND (
          t.status IN ('completed', 'failed', 'cancelled') OR at.archived_at IS NOT NULL OR at.native_deleted_at IS NOT NULL
          OR at.state NOT IN ('creating', 'active', 'draining')
          OR (at.state <> 'creating' AND t.current_attempt_id IS DISTINCT FROM at.id)))
  AND NOT EXISTS (SELECT 1 FROM hitl_responses h WHERE h.request_id = r.id AND h.state IN ('recorded', 'sending', 'accepted', 'uncertain'))
ORDER BY p.id, r.created_at DESC, r.id
ON CONFLICT (source_kind, source_id) DO UPDATE
SET state = 'open', resolved_at = NULL, updated_at = now(), session_id = EXCLUDED.session_id,
    read_at = CASE WHEN a.state <> 'open' THEN NULL ELSE a.read_at END,
    href = EXCLUDED.href, detail = EXCLUDED.detail
WHERE a.state <> 'open' OR a.detail IS DISTINCT FROM EXCLUDED.detail
RETURNING id;

-- Approval/refusal ends the owner's need as soon as it is recorded; a rejected transport can
-- reopen it. Supersession, expiry and all later candidate states also end it.
-- name: ResolveMergeProposalAttention :many
UPDATE attention_items a SET state = 'resolved', resolved_at = now(), updated_at = now()
WHERE a.owner_id = $1 AND a.source_kind = 'merge_proposal' AND a.state = 'open'
  AND NOT EXISTS (
      SELECT 1 FROM merge_proposals p JOIN merge_candidates c ON c.id = p.candidate_id
      JOIN hitl_requests r ON r.id = a.detail->>'hitl_request_id'
      JOIN hitl_merge_tools m ON m.request_id = r.id AND m.proposal_id = p.id
      JOIN sessions s ON s.id = r.session_id AND s.user_id = r.owner_id
      JOIN agent_bindings b ON b.session_id = r.session_id AND b.kagent_session_id = r.kagent_session_id AND b.kagent_deleted_at IS NULL
      WHERE p.id = a.source_id AND p.owner_id = a.owner_id AND r.owner_id = p.owner_id AND p.session_id = r.session_id
        AND r.availability IN ('pending', 'stale') AND c.route = 'approval' AND c.state = 'prepared'
        AND (c.deadline IS NULL OR c.deadline > now())
        AND s.project_id = c.project_id AND s.archived_at IS NULL AND s.status NOT IN ('completed', 'failed', 'cancelled', 'superseded')
        AND NOT EXISTS (SELECT 1 FROM hitl_responses h WHERE h.request_id = r.id AND h.state IN ('recorded', 'sending', 'accepted', 'uncertain'))
  )
RETURNING a.id;
