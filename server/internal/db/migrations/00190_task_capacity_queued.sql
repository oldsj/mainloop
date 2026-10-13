-- +goose Up
-- TASK_MAX_ACTIVE_GLOBAL admits a task over the cap as queued rather than refusing it: it waits
-- with reason capacity_queued, holding no slot, until the FIFO-next queued task is promoted to
-- provisioning_unavailable as a slot frees. internal/tasks holds the same reason and status table.
ALTER TABLE tasks DROP CONSTRAINT tasks_reason_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_reason_check CHECK (
    reason IN (
        'owner_decision', 'awaiting_child', 'approval', 'ci', 'publication', 'handoff',
        'reconciliation', 'provisioning_unavailable', 'capacity_queued', 'handoff_unavailable', 'cancel_unavailable'
    )
);

ALTER TABLE tasks DROP CONSTRAINT tasks_status_reason;
ALTER TABLE tasks ADD CONSTRAINT tasks_status_reason CHECK (
    CASE status
        WHEN 'queued' THEN reason IS NULL OR reason = 'provisioning_unavailable' OR reason = 'capacity_queued'
        WHEN 'running' THEN reason IS NULL
        WHEN 'waiting' THEN reason IS NOT NULL AND (
            reason = 'awaiting_child' OR reason = 'approval' OR reason = 'ci' OR reason = 'publication'
            OR reason = 'handoff' OR reason = 'reconciliation'
        )
        WHEN 'blocked' THEN reason IS NOT NULL AND (reason = 'owner_decision' OR reason = 'reconciliation')
        ELSE reason IS NULL
    END
);

-- +goose Down
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
