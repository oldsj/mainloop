-- +goose Up
-- Since when the liveness sweep has been failing to read the attempt's kagent session, or null while
-- its reads succeed. A failed read says nothing about the attempt, but a read that keeps failing for
-- longer than the stall timeout leaves the attempt unjudged for as long as the failure lasts; the
-- sweep counts that time here so it can stop treating it as nothing. See internal/tasks/liveness.go.
ALTER TABLE task_attempts
    ADD COLUMN liveness_failing_since timestamptz;

-- +goose Down
ALTER TABLE task_attempts
    DROP COLUMN liveness_failing_since;
