-- +goose Up
-- The overflow tier of the global child limit (docs/specs/tasks.md#capacity). Admission gives a
-- child a slot in the base tier (TASK_MAX_CHILDREN_GLOBAL) while it has room, else in the overflow
-- tier (TASK_MAX_CHILDREN_OVERFLOW) above it. capacity_tier is the tier of the task's last
-- admission. An overflow task may find no runtime room yet (the spare worker is still waking), so
-- runtime_wait_since marks it as waiting: set with its admission, or with a successor attempt, and
-- cleared by the runtime-wait sweep once kagent has accepted a message for its current attempt, or
-- by the trigger below when the task ends. While it is set, a send kagent refuses is retried
-- instead of failed; the sweep fails a task that waited longer than TASK_OVERFLOW_WAIT_SECONDS.
ALTER TABLE tasks
    ADD COLUMN capacity_tier text NOT NULL DEFAULT 'base' CHECK (capacity_tier IN ('base', 'overflow')),
    ADD COLUMN runtime_wait_since timestamptz,
    ADD CONSTRAINT tasks_runtime_wait_overflow CHECK (runtime_wait_since IS NULL OR capacity_tier = 'overflow');

CREATE INDEX tasks_runtime_wait ON tasks (runtime_wait_since) WHERE runtime_wait_since IS NOT NULL;

-- +goose StatementBegin
CREATE FUNCTION task_runtime_wait_release_on_status() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status IN ('completed', 'failed', 'cancelled') THEN
        NEW.runtime_wait_since := NULL;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd

CREATE TRIGGER tasks_runtime_wait_release BEFORE UPDATE OF status ON tasks
FOR EACH ROW EXECUTE FUNCTION task_runtime_wait_release_on_status();

-- Capacity refusals answered to a caller, by reason, for the mainloop_task_capacity_refusals_total
-- counter. Admission runs in several processes and its transaction rolls back on a refusal, so the
-- count is kept here, written in its own statement after the rollback, and read by the one process
-- that serves metrics. It only grows.
CREATE TABLE task_capacity_refusals (
    reason text PRIMARY KEY CHECK (reason IN ('global_capacity', 'child_capacity')),
    total bigint NOT NULL DEFAULT 0 CHECK (total >= 0)
);

-- +goose Down
DROP TABLE IF EXISTS task_capacity_refusals;
DROP TRIGGER IF EXISTS tasks_runtime_wait_release ON tasks;
DROP FUNCTION IF EXISTS task_runtime_wait_release_on_status();
DROP INDEX IF EXISTS tasks_runtime_wait;
ALTER TABLE tasks
    DROP CONSTRAINT IF EXISTS tasks_runtime_wait_overflow,
    DROP COLUMN IF EXISTS runtime_wait_since,
    DROP COLUMN IF EXISTS capacity_tier;
