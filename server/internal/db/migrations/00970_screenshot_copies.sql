-- +goose Up
-- A chat image copied from a report's screenshot names that screenshot, so retention can find the
-- copies: they go on the screenshots' schedule (issue #534 follow-up). No foreign key: the copy
-- outlives the workspace its screenshot was stored with.
ALTER TABLE message_images ADD COLUMN screenshot_id text;
-- Copies made before this column, claimed or not: their id is "<message id>:<screenshot id>", both
-- UUIDs. A pasted image's id is a bare UUID.
UPDATE message_images SET screenshot_id = split_part(id, ':', 2)
WHERE id ~ '^[0-9a-f-]{36}:[0-9a-f-]{36}$';
CREATE INDEX message_images_screenshot_copies ON message_images (session_id, created_at)
    WHERE screenshot_id IS NOT NULL;

-- +goose Down
DROP INDEX message_images_screenshot_copies;
ALTER TABLE message_images DROP COLUMN screenshot_id;
