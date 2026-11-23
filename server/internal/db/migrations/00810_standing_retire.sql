-- +goose Up
-- The owner retires a project's standing supervisor (docs/specs/project-supervisor.md): it is
-- cancelled, and the project's next first use starts a new one. A project has at most one standing
-- supervisor that has not ended; the ones it retired stay as its history.
DROP INDEX tasks_standing_project;
CREATE UNIQUE INDEX tasks_standing_project ON tasks (project_id)
    WHERE standing AND status NOT IN ('completed', 'cancelled', 'failed');

-- +goose Down
-- A retired standing supervisor becomes an ordinary cancelled coordination task, so at most one
-- standing task per project remains.
UPDATE tasks SET standing = false WHERE standing AND status IN ('completed', 'cancelled', 'failed');
DROP INDEX tasks_standing_project;
CREATE UNIQUE INDEX tasks_standing_project ON tasks (project_id) WHERE standing;
