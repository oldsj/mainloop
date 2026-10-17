-- +goose Up
-- An environment may opt in to following its watched tag: a sweep resolves the tag, appends a new
-- version when the image changed and accepts it as the default (internal/environments/follow.go).
-- Off by default, so a watched tag stays metadata for every other environment.
ALTER TABLE dev_environments
ADD COLUMN follow_watched_tag boolean NOT NULL DEFAULT false,
ADD CONSTRAINT dev_environments_follow_needs_tag CHECK (NOT follow_watched_tag OR watched_tag IS NOT NULL);

-- +goose Down
ALTER TABLE dev_environments
DROP CONSTRAINT dev_environments_follow_needs_tag,
DROP COLUMN follow_watched_tag;
