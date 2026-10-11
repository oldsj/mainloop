-- The owner's attention: what needs them now (attention_items) and the Activity log around it.
-- The visibility rule is shared by the list, the count and the sessions behind the items: an item
-- of an archived or finished session is not shown, whatever its row says, so a late observer
-- cannot leave a badge behind. Items without a session (a task decision whose attempt has no
-- Mainloop session) are always shown.

-- name: ListOpenAttention :many
SELECT a.*
FROM attention_items a
LEFT JOIN sessions s ON s.id = a.session_id
WHERE a.owner_id = $1 AND a.state = 'open'
  AND (a.session_id IS NULL OR (s.archived_at IS NULL AND s.status NOT IN ('completed', 'failed', 'cancelled', 'superseded')))
ORDER BY a.created_at DESC, a.id;

-- name: CountOpenAttention :one
SELECT count(*)::int
FROM attention_items a
LEFT JOIN sessions s ON s.id = a.session_id
WHERE a.owner_id = $1 AND a.state = 'open'
  AND (a.session_id IS NULL OR (s.archived_at IS NULL AND s.status NOT IN ('completed', 'failed', 'cancelled', 'superseded')));

-- Whether a session has an open item the owner can see (the needs_owner rule).
-- name: SessionNeedsOwner :one
SELECT EXISTS (
    SELECT 1
    FROM attention_items a
    JOIN sessions s ON s.id = a.session_id
    WHERE a.owner_id = $1 AND a.session_id = $2 AND a.state = 'open'
      AND s.archived_at IS NULL AND s.status NOT IN ('completed', 'failed', 'cancelled', 'superseded')
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
