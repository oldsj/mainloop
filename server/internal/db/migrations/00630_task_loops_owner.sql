-- +goose Up
-- The owner's loop controls (docs/specs/loops.md, "The owner's view and controls"). A paused loop
-- is not evaluated for its trigger, but the runner still claims it to notice its expiry or an ended
-- recipient. fire_request_id is the request id of the owner's latest "fire now", so a retried
-- request does not fire twice.
ALTER TABLE task_loops
    DROP CONSTRAINT task_loops_state_check,
    ADD CONSTRAINT task_loops_state_check CHECK (
        state IN ('armed', 'paused', 'done', 'exhausted', 'expired', 'stopped', 'ended')
    ),
    ADD COLUMN fire_request_id text CHECK (fire_request_id IS NULL OR length(fire_request_id) BETWEEN 1 AND 100);

DROP INDEX task_loops_claim;
CREATE INDEX task_loops_claim ON task_loops (next_attempt_at, id) WHERE state IN ('armed', 'paused');
DROP INDEX task_loops_recipient;
CREATE INDEX task_loops_recipient ON task_loops (recipient_key) WHERE state IN ('armed', 'paused');

-- +goose Down
UPDATE task_loops SET state = 'stopped', updated_at = now() WHERE state = 'paused';
DROP INDEX task_loops_recipient;
CREATE INDEX task_loops_recipient ON task_loops (recipient_key) WHERE state = 'armed';
DROP INDEX task_loops_claim;
CREATE INDEX task_loops_claim ON task_loops (next_attempt_at, id) WHERE state = 'armed';
ALTER TABLE task_loops
    DROP COLUMN fire_request_id,
    DROP CONSTRAINT task_loops_state_check,
    ADD CONSTRAINT task_loops_state_check CHECK (
        state IN ('armed', 'done', 'exhausted', 'expired', 'stopped', 'ended')
    );
