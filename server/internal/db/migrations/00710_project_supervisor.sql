-- +goose Up
-- A project's standing supervisor (docs/specs/project-supervisor.md): a root coordination task
-- marked standing, at most one per project for the life of the project. It never completes: a
-- completed report puts it at rest, waiting with reason idle, and its next message wakes it.
ALTER TABLE tasks
    ADD COLUMN standing boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT tasks_standing_shape CHECK (
        NOT standing OR (mode = 'coordination' AND parent_task_id IS NULL AND project_id IS NOT NULL)
    );

CREATE UNIQUE INDEX tasks_standing_project ON tasks (project_id) WHERE standing;

ALTER TABLE tasks DROP CONSTRAINT tasks_reason_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_reason_check CHECK (
    reason IN (
        'owner_decision', 'awaiting_child', 'approval', 'ci', 'publication', 'handoff',
        'reconciliation', 'provisioning_unavailable', 'handoff_unavailable', 'cancel_unavailable',
        'session_lost', 'stalled', 'runtime_limit', 'idle'
    )
);
-- Operations share the task reason set.
ALTER TABLE task_operations DROP CONSTRAINT task_operations_reason_check;
ALTER TABLE task_operations ADD CONSTRAINT task_operations_reason_check CHECK (
    reason IN (
        'owner_decision', 'awaiting_child', 'approval', 'ci', 'publication', 'handoff',
        'reconciliation', 'provisioning_unavailable', 'handoff_unavailable', 'cancel_unavailable',
        'session_lost', 'stalled', 'runtime_limit', 'idle'
    )
);
ALTER TABLE tasks DROP CONSTRAINT tasks_status_reason;
ALTER TABLE tasks ADD CONSTRAINT tasks_status_reason CHECK (
    CASE status
        WHEN 'queued' THEN reason IS NULL OR reason = 'provisioning_unavailable'
        WHEN 'running' THEN reason IS NULL
        WHEN 'waiting' THEN reason IS NOT NULL AND (
            reason = 'awaiting_child' OR reason = 'approval' OR reason = 'ci' OR reason = 'publication'
            OR reason = 'handoff' OR reason = 'reconciliation' OR (reason = 'idle' AND standing)
        )
        WHEN 'blocked' THEN reason IS NOT NULL AND (
            reason = 'owner_decision' OR reason = 'reconciliation'
            OR reason = 'session_lost' OR reason = 'stalled' OR reason = 'runtime_limit'
        )
        ELSE reason IS NULL
    END
);

-- +goose Down
UPDATE tasks SET status = 'running', reason = NULL WHERE reason = 'idle';
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
ALTER TABLE tasks DROP CONSTRAINT tasks_reason_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_reason_check CHECK (
    reason IN (
        'owner_decision', 'awaiting_child', 'approval', 'ci', 'publication', 'handoff',
        'reconciliation', 'provisioning_unavailable', 'handoff_unavailable', 'cancel_unavailable',
        'session_lost', 'stalled', 'runtime_limit'
    )
);
DROP INDEX IF EXISTS tasks_standing_project;
ALTER TABLE tasks
    DROP CONSTRAINT IF EXISTS tasks_standing_shape,
    DROP COLUMN IF EXISTS standing;
