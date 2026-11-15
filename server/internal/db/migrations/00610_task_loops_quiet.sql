-- +goose Up
-- The quiet trigger and the no-progress stop (docs/specs/loops.md). A quiet loop fires when its
-- subject task has not changed for quiet_seconds: no new version (status, report, projection) and
-- no new activity on its current attempt (progress_marker). A loop armed with expect_change is a
-- repair-style loop: after three firings in a row with its subject unchanged, it stops as
-- exhausted with stop_reason no_progress, and the attention sweep asks the owner to decide.
ALTER TABLE task_loops
    DROP CONSTRAINT task_loops_trigger_check,
    ADD CONSTRAINT task_loops_trigger_check CHECK (trigger IN ('task', 'time', 'pull_request', 'quiet')),
    ADD COLUMN quiet_seconds integer CHECK (quiet_seconds IS NULL OR quiet_seconds > 0),
    -- When the quiet loop last saw its subject change, or last fired: it fires quiet_seconds after.
    ADD COLUMN quiet_since timestamptz,
    ADD COLUMN expect_change boolean NOT NULL DEFAULT false,
    -- The subject's version and progress marker at the latest firing, and how many firings in a row
    -- before it found the same.
    ADD COLUMN progress_mark text,
    ADD COLUMN unchanged_firings integer NOT NULL DEFAULT 0,
    ADD COLUMN stop_reason text CHECK (stop_reason IS NULL OR stop_reason = 'no_progress'),
    -- The subject's version when the loop stopped for no progress: its attention item resolves once
    -- the subject moves past it or ends.
    ADD COLUMN stopped_version integer,
    ADD CONSTRAINT task_loops_quiet_shape CHECK (
        trigger <> 'quiet' OR (subject_task_id IS NOT NULL AND quiet_seconds IS NOT NULL)
    ),
    ADD CONSTRAINT task_loops_expect_change_shape CHECK (
        NOT expect_change OR (subject_task_id IS NOT NULL AND (trigger = 'time' OR trigger = 'quiet'))
    ),
    ADD CONSTRAINT task_loops_stop_reason_state CHECK (stop_reason IS NULL OR state = 'exhausted');

-- The firing that stopped its loop says so; a firing's text is fixed once it is sent.
ALTER TABLE task_loop_firings
    ADD COLUMN stops_loop text CHECK (stops_loop IS NULL OR stops_loop = 'no_progress');

-- +goose Down
DELETE FROM attention_items WHERE source_kind = 'loop_no_progress';
DELETE FROM task_event_deliveries WHERE loop_firing_id IN (
    SELECT f.id FROM task_loop_firings f JOIN task_loops l ON l.id = f.loop_id WHERE l.trigger = 'quiet'
);
DELETE FROM task_loop_firings WHERE loop_id IN (SELECT id FROM task_loops WHERE trigger = 'quiet');
DELETE FROM task_loops WHERE trigger = 'quiet';
ALTER TABLE task_loop_firings DROP COLUMN stops_loop;
ALTER TABLE task_loops
    DROP CONSTRAINT task_loops_stop_reason_state,
    DROP CONSTRAINT task_loops_expect_change_shape,
    DROP CONSTRAINT task_loops_quiet_shape,
    DROP COLUMN stopped_version,
    DROP COLUMN stop_reason,
    DROP COLUMN unchanged_firings,
    DROP COLUMN progress_mark,
    DROP COLUMN expect_change,
    DROP COLUMN quiet_since,
    DROP COLUMN quiet_seconds,
    DROP CONSTRAINT task_loops_trigger_check,
    ADD CONSTRAINT task_loops_trigger_check CHECK (trigger IN ('task', 'time', 'pull_request'));
