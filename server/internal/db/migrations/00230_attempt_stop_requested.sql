-- +goose Up
-- When the attempt's stop was requested (it entered draining). The resolver forces a stop that
-- cancelling did not settle by how long it has waited, not by how often it has looked: looks come
-- faster or slower with the backoff, a wait does not. native_deleted_at, which already exists, is
-- written when the resolver deletes the native session itself.
ALTER TABLE task_attempts
    ADD COLUMN stop_requested_at timestamptz;

-- +goose Down
ALTER TABLE task_attempts
    DROP COLUMN stop_requested_at;
