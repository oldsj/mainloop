-- +goose Up
-- The liveness sweep judges a working turn by whether kagent's record of it still changes, not by
-- whether kagent still calls it working: a turn whose native process wedged stays "working" in
-- kagent forever. progress_marker is the last fingerprint of the attempt's kagent task the sweep
-- saw (its state, status time and newest history message), and progress_at is when the sweep
-- first saw that fingerprint. See internal/tasks/liveness.go.
ALTER TABLE task_attempts
    ADD COLUMN progress_marker text,
    ADD COLUMN progress_at timestamptz;

-- +goose Down
ALTER TABLE task_attempts
    DROP COLUMN progress_at,
    DROP COLUMN progress_marker;
