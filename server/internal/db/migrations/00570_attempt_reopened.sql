-- +goose Up
-- A follow-up to a completed task reopens its attempt on the same native session (see
-- docs/specs/tasks.md#follow-up). reopened_at is when it last did: the runtime bound counts from it,
-- not from the attempt's creation, and the attempt's history keeps created_at.
ALTER TABLE task_attempts
    ADD COLUMN reopened_at timestamptz;

-- +goose Down
ALTER TABLE task_attempts
    DROP COLUMN reopened_at;
