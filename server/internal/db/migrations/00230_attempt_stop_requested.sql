-- +goose Up
-- When the attempt's stop was requested (it entered draining). The resolver forces a stop that
-- cancelling did not settle by how long it has waited, not by how often it has looked: looks come
-- faster or slower with the backoff, a wait does not. native_deleted_at, which already exists, is
-- written when the resolver deletes the native session itself.
ALTER TABLE task_attempts
    ADD COLUMN stop_requested_at timestamptz;

-- An attempt already draining has no recorded request. Without one the resolver ages its stop from
-- updated_at, which every look bumps, so the stop would restart its grace at every look and never be
-- forced. updated_at is the closest it has: never earlier than the request, so a stop is at worst
-- forced a little late, and it stops moving once the stop is aged from this column.
UPDATE task_attempts SET stop_requested_at = updated_at WHERE state = 'draining';

-- +goose Down
ALTER TABLE task_attempts
    DROP COLUMN stop_requested_at;
