-- Push notifications (internal/notify). The enqueue statements are idempotent: each derives the
-- row's key from what it is about and skips a key that exists.

-- name: EnsurePushFloor :exec
INSERT INTO push_floor DEFAULT VALUES ON CONFLICT DO NOTHING;

-- A Needs-you item (a real owner decision, never a 'problem') that opened since push was turned
-- on and is visible now. One notification per item, ever.
-- name: EnqueueAttentionPush :execrows
INSERT INTO push_notifications (id, idempotency_key, owner_id, kind, source_id, source_state, title, body, href, priority)
SELECT 'push_' || md5('attention:' || a.id), 'attention:' || a.id, a.owner_id, 'attention', a.id, a.kind,
       a.title, a.needed, a.href, CASE a.kind WHEN 'approval' THEN 4 ELSE 3 END
FROM attention_visible v
JOIN attention_items a ON a.id = v.id
CROSS JOIN push_floor f
WHERE v.owner_id = @owner_id AND a.kind IN ('approval', 'question', 'owner_decision')
  AND a.created_at >= f.started_at
ON CONFLICT (idempotency_key) DO NOTHING;

-- A root task that reached completed, failed or blocked since push was turned on, when the newest
-- notification about it reports a different status. A block on an owner decision is left to its
-- attention item. n counts earlier notifications about the task, so a later change gets a new key.
-- name: EnqueueTaskPush :execrows
INSERT INTO push_notifications (id, idempotency_key, owner_id, kind, source_id, source_state, title, body, href, priority)
SELECT 'push_' || md5(k.key), k.key, t.owner_id, 'task', t.id, t.status,
       t.title,
       CASE t.status WHEN 'completed' THEN 'Task completed' WHEN 'failed' THEN 'Task failed'
            ELSE 'Task blocked: Mainloop could not recover it on its own' END,
       '/tasks/' || t.id,
       CASE t.status WHEN 'completed' THEN 3 ELSE 4 END
FROM tasks t
CROSS JOIN push_floor f
CROSS JOIN LATERAL (
    SELECT 'task:' || t.id || ':' || t.status || ':' || count(*) AS key
    FROM push_notifications p WHERE p.kind = 'task' AND p.source_id = t.id
) k
WHERE t.owner_id = @owner_id AND t.parent_task_id IS NULL
  AND (t.status IN ('completed', 'failed') OR (t.status = 'blocked' AND t.reason IS DISTINCT FROM 'owner_decision'))
  AND t.updated_at >= f.started_at
  AND (SELECT p.source_state FROM push_notifications p
       WHERE p.kind = 'task' AND p.source_id = t.id
       ORDER BY p.created_at DESC, p.id DESC LIMIT 1) IS DISTINCT FROM t.status
ON CONFLICT (idempotency_key) DO NOTHING;

-- name: GetPushNotification :one
SELECT * FROM push_notifications WHERE id = $1;

-- Whether the owner still needs to hear about it: the attention item is still visible, or the task
-- still has the status the notification reports.
-- name: PushStillWanted :one
SELECT CASE p.kind
    WHEN 'attention' THEN EXISTS (SELECT 1 FROM attention_visible v WHERE v.id = p.source_id)
    ELSE EXISTS (SELECT 1 FROM tasks t WHERE t.id = p.source_id AND t.status = p.source_state)
END::boolean
FROM push_notifications p WHERE p.id = $1;

-- Records the outcome of the current claim (fenced on lease_owner and attempts).
-- name: SettlePushNotification :execrows
UPDATE push_notifications
SET state = @state, detail = @detail, updated_at = now(),
    sent_at = CASE WHEN @state = 'sent' THEN now() ELSE sent_at END,
    lease_owner = NULL, lease_until = NULL
WHERE id = @id AND lease_owner = @lease_owner AND attempts = @attempts;
