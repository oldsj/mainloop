-- +goose Up
-- A hold: the main thread sets a task aside until a time (docs/specs/tasks.md#holds). It is not a
-- status: the task's lifecycle, liveness and runtime bounds go on as before; the hold only keeps
-- it out of the owner's Working and Needs you views until held_until, when a sweep clears it and
-- tells the main thread. A hold never hides a failure: the triggers below clear it the moment the
-- task stops being live (failed, blocked, completed, cancelled) or anything needs the owner about
-- it (an open attention item naming the task or the session of one of its attempts).
ALTER TABLE tasks
    ADD COLUMN held_until timestamptz,
    ADD COLUMN held_reason text CHECK (held_reason IS NULL OR length(held_reason) <= 500);

CREATE INDEX tasks_held ON tasks (held_until) WHERE held_until IS NOT NULL;

-- +goose StatementBegin
CREATE FUNCTION task_hold_release_on_status() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status NOT IN ('queued', 'running', 'waiting') THEN
        NEW.held_until := NULL;
        NEW.held_reason := NULL;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd

CREATE TRIGGER tasks_hold_release BEFORE UPDATE OF status ON tasks
FOR EACH ROW EXECUTE FUNCTION task_hold_release_on_status();

-- This and a hold take the same advisory lock (internal/tasks/hold.go), so they serialize: either
-- the hold commits first and the update below, a new statement, sees and clears it, or this
-- commits first and the hold's own count sees the item. An advisory lock rather than the task row,
-- so an insert that does not otherwise touch the task never waits on, or deadlocks with, its row.
-- +goose StatementBegin
CREATE FUNCTION task_hold_release_on_attention() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    tid text;
BEGIN
    IF NEW.state <> 'open' THEN
        RETURN NULL;
    END IF;
    tid := NEW.task_id;
    IF tid IS NULL AND NEW.session_id IS NOT NULL THEN
        SELECT task_id INTO tid FROM task_attempts WHERE session_id = NEW.session_id;
    END IF;
    IF tid IS NULL THEN
        RETURN NULL;
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('mainloop:task-hold:' || tid, 0));
    UPDATE tasks SET held_until = NULL, held_reason = NULL WHERE id = tid AND held_until IS NOT NULL;
    RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE TRIGGER attention_items_hold_release AFTER INSERT OR UPDATE OF state ON attention_items
FOR EACH ROW EXECUTE FUNCTION task_hold_release_on_attention();

-- +goose Down
DROP TRIGGER attention_items_hold_release ON attention_items;
DROP FUNCTION task_hold_release_on_attention();
DROP TRIGGER tasks_hold_release ON tasks;
DROP FUNCTION task_hold_release_on_status();
DROP INDEX tasks_held;
ALTER TABLE tasks DROP COLUMN held_reason, DROP COLUMN held_until;
