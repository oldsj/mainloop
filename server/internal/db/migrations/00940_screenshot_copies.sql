-- +goose Up
-- A chat image copied from a report's screenshot names that screenshot, so retention can find the
-- copies: they go on the screenshots' schedule (issue #534 follow-up). No foreign key: the copy
-- outlives the workspace its screenshot was stored with.
ALTER TABLE message_images ADD COLUMN screenshot_id text;
-- Copies made before this column: their id is "<message id>:<screenshot id>".
UPDATE message_images SET screenshot_id = substr(id, length(message_id) + 2)
WHERE message_id IS NOT NULL AND starts_with(id, message_id || ':');
CREATE INDEX message_images_screenshot_copies ON message_images (session_id, created_at)
    WHERE screenshot_id IS NOT NULL;

-- +goose Down
DROP INDEX message_images_screenshot_copies;
ALTER TABLE message_images DROP COLUMN screenshot_id;
