-- +goose Up
-- The text a delivery sent to kagent when it differs from the owner's message: the first message
-- to a new main Session carries a short preamble of the earlier conversation. It is frozen with
-- the move to sending, so one A2A messageId always carries one payload, and it never reaches the
-- mirrored conversation, which keeps only what the owner wrote. NULL until the send is decided.
ALTER TABLE deliveries ADD COLUMN sent_text text;

-- +goose Down
ALTER TABLE deliveries DROP COLUMN IF EXISTS sent_text;
