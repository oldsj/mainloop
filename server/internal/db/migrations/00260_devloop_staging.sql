-- +goose Up
-- The shared instance the devloop targets is now mainloop-staging (it was mainloop-dev). The lease
-- and the sync lock are keyed by that name. Move the existing rows to the new id and keep the
-- holder and queue, so a rename mid-lease does not hand the lease to someone else.
INSERT INTO devloop_lease (id, holder_session_id, acquired_at, last_used_at, idle_timeout_seconds, created_at, updated_at)
SELECT 'mainloop-staging', holder_session_id, acquired_at, last_used_at, idle_timeout_seconds, created_at, updated_at
FROM devloop_lease WHERE id = 'mainloop-dev'
ON CONFLICT (id) DO NOTHING;
UPDATE devloop_lease_queue SET lease_id = 'mainloop-staging' WHERE lease_id = 'mainloop-dev';
DELETE FROM devloop_lease WHERE id = 'mainloop-dev';

UPDATE devloop_sync_lock SET id = 'mainloop-staging' WHERE id = 'mainloop-dev';

-- +goose Down
INSERT INTO devloop_lease (id, holder_session_id, acquired_at, last_used_at, idle_timeout_seconds, created_at, updated_at)
SELECT 'mainloop-dev', holder_session_id, acquired_at, last_used_at, idle_timeout_seconds, created_at, updated_at
FROM devloop_lease WHERE id = 'mainloop-staging'
ON CONFLICT (id) DO NOTHING;
UPDATE devloop_lease_queue SET lease_id = 'mainloop-dev' WHERE lease_id = 'mainloop-staging';
DELETE FROM devloop_lease WHERE id = 'mainloop-staging';

UPDATE devloop_sync_lock SET id = 'mainloop-dev' WHERE id = 'mainloop-staging';
