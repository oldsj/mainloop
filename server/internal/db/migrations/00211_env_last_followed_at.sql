-- +goose Up
-- When a following environment last accepted a new image from its watched tag. The follow sweep
-- accepts at most one new image per interval, so a tag that moves on every merge does not mint a
-- new version (and a new kagent template) each time. Null means no image has been accepted yet.
ALTER TABLE dev_environments ADD COLUMN last_followed_at timestamptz;

-- +goose Down
ALTER TABLE dev_environments DROP COLUMN last_followed_at;
