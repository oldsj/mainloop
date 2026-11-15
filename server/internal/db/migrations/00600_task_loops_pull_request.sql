-- +goose Up
-- The pull_request trigger (docs/specs/loops.md): a loop on a code task fires when what the task
-- projection saw of its pull request changes, its number, state, or CI result and the commit that
-- result is for. It reads the projection the task projection sweep already keeps fresh.
ALTER TABLE task_loops
    DROP CONSTRAINT task_loops_trigger_check,
    ADD CONSTRAINT task_loops_trigger_check CHECK (trigger IN ('task', 'time', 'pull_request')),
    -- A pull_request trigger fires only when one of these holds after the change; empty is any change.
    ADD COLUMN pr_conditions text[] NOT NULL DEFAULT '{}' CHECK (
        pr_conditions <@ ARRAY['ci_success', 'ci_failure', 'merged', 'closed']::text[]
    ),
    ADD CONSTRAINT task_loops_pull_request_subject CHECK (trigger <> 'pull_request' OR subject_task_id IS NOT NULL);

-- The nudge also follows the projection. Each kind of loop compares only what it watches, so a
-- loop made due by a change it does not watch looks and finds nothing.
DROP TRIGGER tasks_loops_nudge ON tasks;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION task_loops_nudge() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE task_loops SET next_attempt_at = now()
    WHERE subject_task_id = NEW.id AND state = 'armed' AND trigger IN ('task', 'pull_request');
    IF FOUND THEN
        PERFORM pg_notify('mainloop_task_loops', NEW.id);
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE TRIGGER tasks_loops_nudge AFTER UPDATE OF status, reason, pr_number, pr_state, ci_state, ci_head_sha ON tasks
FOR EACH ROW WHEN (
    OLD.status IS DISTINCT FROM NEW.status OR OLD.reason IS DISTINCT FROM NEW.reason
    OR OLD.pr_number IS DISTINCT FROM NEW.pr_number OR OLD.pr_state IS DISTINCT FROM NEW.pr_state
    OR OLD.ci_state IS DISTINCT FROM NEW.ci_state OR OLD.ci_head_sha IS DISTINCT FROM NEW.ci_head_sha
)
EXECUTE FUNCTION task_loops_nudge();

-- +goose Down
DROP TRIGGER tasks_loops_nudge ON tasks;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION task_loops_nudge() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE task_loops SET next_attempt_at = now()
    WHERE subject_task_id = NEW.id AND state = 'armed' AND trigger = 'task';
    IF FOUND THEN
        PERFORM pg_notify('mainloop_task_loops', NEW.id);
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE TRIGGER tasks_loops_nudge AFTER UPDATE OF status, reason ON tasks
FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status OR OLD.reason IS DISTINCT FROM NEW.reason)
EXECUTE FUNCTION task_loops_nudge();

DELETE FROM task_event_deliveries WHERE loop_firing_id IN (
    SELECT f.id FROM task_loop_firings f JOIN task_loops l ON l.id = f.loop_id WHERE l.trigger = 'pull_request'
);
DELETE FROM task_loop_firings WHERE loop_id IN (SELECT id FROM task_loops WHERE trigger = 'pull_request');
DELETE FROM task_loops WHERE trigger = 'pull_request';
ALTER TABLE task_loops
    DROP CONSTRAINT task_loops_pull_request_subject,
    DROP COLUMN pr_conditions,
    DROP CONSTRAINT task_loops_trigger_check,
    ADD CONSTRAINT task_loops_trigger_check CHECK (trigger IN ('task', 'time'));
