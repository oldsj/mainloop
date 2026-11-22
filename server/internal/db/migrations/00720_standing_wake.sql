-- +goose Up
-- A standing supervisor's runtime bound (TASK_MAX_RUNTIME) counts from its last wake, not from its
-- attempt's start: it lives for weeks, resting between batches (docs/specs/project-supervisor.md).
-- Whatever wakes it (a follow-up, a report, the liveness sweep seeing a turn) moves it from
-- waiting/idle to running; this trigger restarts the bound on its current attempt in the same
-- statement's transaction, so no path can forget to.
-- +goose StatementBegin
CREATE FUNCTION task_standing_wake() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.standing AND OLD.status = 'waiting' AND OLD.reason = 'idle' AND NEW.status = 'running'
       AND NEW.current_attempt_id IS NOT NULL THEN
        UPDATE task_attempts SET reopened_at = now(), updated_at = now() WHERE id = NEW.current_attempt_id;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd

CREATE TRIGGER tasks_standing_wake AFTER UPDATE OF status ON tasks
FOR EACH ROW EXECUTE FUNCTION task_standing_wake();

-- +goose Down
DROP TRIGGER IF EXISTS tasks_standing_wake ON tasks;
DROP FUNCTION IF EXISTS task_standing_wake();
