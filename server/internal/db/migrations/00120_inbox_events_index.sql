-- +goose Up
-- The attention sweep reads the newest inbox:updated event of the owner every pass. The general
-- (user_id, id) index walks back over every other event type to reach it; this one is a single
-- probe.
CREATE INDEX events_user_inbox ON events (user_id, id) WHERE type = 'inbox:updated';

-- +goose Down
DROP INDEX IF EXISTS events_user_inbox;
