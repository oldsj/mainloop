-- +goose Up
-- What project context a delivery to a task session gave it, when that delivery carried a change
-- (docs/specs/project-context.md#running-sessions). A session has what its newest such delivery gave,
-- unless kagent never saw that message (failed or cancelled with no task); with none, it has what
-- its attempt's brief pinned (task_attempts.instructions_version and memory_seq).
ALTER TABLE deliveries
    ADD COLUMN given_instructions_version integer CHECK (given_instructions_version >= 0),
    ADD COLUMN given_memory_seq bigint CHECK (given_memory_seq >= 0),
    ADD CONSTRAINT deliveries_project_context_given
        CHECK ((given_instructions_version IS NULL) = (given_memory_seq IS NULL));

-- +goose Down
ALTER TABLE deliveries
    DROP CONSTRAINT IF EXISTS deliveries_project_context_given,
    DROP COLUMN IF EXISTS given_memory_seq,
    DROP COLUMN IF EXISTS given_instructions_version;
