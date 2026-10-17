-- +goose Up
-- The liveness sweep bounds a running task: it is blocked, once, when kagent reports its attempt's
-- session gone or terminal (session_lost), when the session has shown no activity for
-- TASK_STALL_TIMEOUT (stalled), or when the attempt has run longer than TASK_MAX_RUNTIME
-- (runtime_limit). See internal/tasks/liveness.go. No attempt column changes: liveness is read
-- fresh from kagent on every sweep pass, never cached.
ALTER TABLE tasks DROP CONSTRAINT tasks_reason_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_reason_check CHECK (
    reason IN (
        'owner_decision', 'awaiting_child', 'approval', 'ci', 'publication', 'handoff',
        'reconciliation', 'provisioning_unavailable', 'handoff_unavailable', 'cancel_unavailable',
        'session_lost', 'stalled', 'runtime_limit'
    )
);
-- task_operations.reason shares the same value set as tasks.reason in the original migration;
-- keep them in lockstep even though no operation uses the three new values yet.
ALTER TABLE task_operations DROP CONSTRAINT task_operations_reason_check;
ALTER TABLE task_operations ADD CONSTRAINT task_operations_reason_check CHECK (
    reason IN (
        'owner_decision', 'awaiting_child', 'approval', 'ci', 'publication', 'handoff',
        'reconciliation', 'provisioning_unavailable', 'handoff_unavailable', 'cancel_unavailable',
        'session_lost', 'stalled', 'runtime_limit'
    )
);
ALTER TABLE tasks DROP CONSTRAINT tasks_status_reason;
ALTER TABLE tasks ADD CONSTRAINT tasks_status_reason CHECK (
    CASE status
        WHEN 'queued' THEN reason IS NULL OR reason = 'provisioning_unavailable'
        WHEN 'running' THEN reason IS NULL
        WHEN 'waiting' THEN reason IS NOT NULL AND (
            reason = 'awaiting_child' OR reason = 'approval' OR reason = 'ci' OR reason = 'publication'
            OR reason = 'handoff' OR reason = 'reconciliation'
        )
        WHEN 'blocked' THEN reason IS NOT NULL AND (
            reason = 'owner_decision' OR reason = 'reconciliation'
            OR reason = 'session_lost' OR reason = 'stalled' OR reason = 'runtime_limit'
        )
        ELSE reason IS NULL
    END
);

ALTER TABLE attention_items DROP CONSTRAINT attention_items_reason_code_check;
ALTER TABLE attention_items ADD CONSTRAINT attention_items_reason_code_check CHECK (
    reason_code IN ('reconciliation', 'runtime_stop_unresolved', 'merge_approval', 'session_lost', 'stalled', 'runtime_limit')
);

-- A liveness reason surfaces the same way a reconciliation block does: one attention item per task,
-- naming which of the three it is, until the owner acts.
CREATE OR REPLACE VIEW task_reconciliation_needs AS
SELECT t.id AS task_id, t.owner_id, t.title,
       CASE
           WHEN EXISTS (
               SELECT 1 FROM task_attempts a WHERE a.task_id = t.id
                 AND 'runtime-stop-unresolved' = ANY(a.evidence_refs) AND a.stop_state <> 'confirmed'
           ) THEN 'runtime_stop_unresolved'
           WHEN t.status = 'blocked' AND t.reason IN ('session_lost', 'stalled', 'runtime_limit') THEN t.reason
           ELSE 'reconciliation'
       END AS reason_code
FROM tasks t
WHERE (t.status = 'blocked' AND t.reason IN ('reconciliation', 'session_lost', 'stalled', 'runtime_limit')) OR EXISTS (
    SELECT 1 FROM task_attempts a WHERE a.task_id = t.id
      AND 'runtime-stop-unresolved' = ANY(a.evidence_refs) AND a.stop_state <> 'confirmed'
);

-- +goose Down
CREATE OR REPLACE VIEW task_reconciliation_needs AS
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
ALTER TABLE attention_items DROP CONSTRAINT attention_items_reason_code_check;
ALTER TABLE attention_items ADD CONSTRAINT attention_items_reason_code_check CHECK (
    reason_code IN ('reconciliation', 'runtime_stop_unresolved', 'merge_approval')
);
ALTER TABLE tasks DROP CONSTRAINT tasks_status_reason;
ALTER TABLE tasks ADD CONSTRAINT tasks_status_reason CHECK (
    CASE status
        WHEN 'queued' THEN reason IS NULL OR reason = 'provisioning_unavailable'
        WHEN 'running' THEN reason IS NULL
        WHEN 'waiting' THEN reason IS NOT NULL AND (
            reason = 'awaiting_child' OR reason = 'approval' OR reason = 'ci' OR reason = 'publication'
            OR reason = 'handoff' OR reason = 'reconciliation'
        )
        WHEN 'blocked' THEN reason IS NOT NULL AND (reason = 'owner_decision' OR reason = 'reconciliation')
        ELSE reason IS NULL
    END
);
ALTER TABLE tasks DROP CONSTRAINT tasks_reason_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_reason_check CHECK (
    reason IN (
        'owner_decision', 'awaiting_child', 'approval', 'ci', 'publication', 'handoff',
        'reconciliation', 'provisioning_unavailable', 'handoff_unavailable', 'cancel_unavailable'
    )
);
ALTER TABLE task_operations DROP CONSTRAINT task_operations_reason_check;
ALTER TABLE task_operations ADD CONSTRAINT task_operations_reason_check CHECK (
    reason IN (
        'owner_decision', 'awaiting_child', 'approval', 'ci', 'publication', 'handoff',
        'reconciliation', 'provisioning_unavailable', 'handoff_unavailable', 'cancel_unavailable'
    )
);
