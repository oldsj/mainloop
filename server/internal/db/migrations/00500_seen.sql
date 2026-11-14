-- +goose Up
-- The owner's seen state, shared by every device (docs/specs/seen.md): which tasks they last had
-- open and when, and their last two app visits. It replaces per-browser local storage, so a task
-- opened on the phone is no longer unseen on the desktop.
CREATE TABLE owner_seen (
    user_id text PRIMARY KEY,
    -- When the record started: a task that last changed before it counts as seen.
    since timestamptz NOT NULL DEFAULT now(),
    last_visit timestamptz,
    previous_visit timestamptz
);

CREATE TABLE task_seen (
    user_id text NOT NULL,
    task_id text NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    seen_at timestamptz NOT NULL,
    PRIMARY KEY (user_id, task_id)
);
CREATE INDEX task_seen_age ON task_seen (user_id, seen_at);

-- +goose Down
DROP TABLE task_seen;
DROP TABLE owner_seen;
