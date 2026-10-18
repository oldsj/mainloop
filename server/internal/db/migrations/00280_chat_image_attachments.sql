-- +goose Up
-- Images pasted into the main chat composer. Uploaded ahead of the message that carries them
-- (POST /chat/images), then claimed by the message it is sent with (SendWithImages), in the same
-- transaction as the session_messages insert. An upload nothing ever claims stays an orphan row;
-- given the size cap, that is rare and small enough to not need a reaping sweep yet.
CREATE TABLE message_images (
    id text PRIMARY KEY,
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    message_id text REFERENCES session_messages (id) ON DELETE CASCADE,
    media_type text NOT NULL,
    data bytea NOT NULL,
    size_bytes integer NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX message_images_message ON message_images (message_id);
-- The claim query's search: an upload not yet attached to any message.
CREATE INDEX message_images_unclaimed ON message_images (session_id, created_at) WHERE message_id IS NULL;

-- +goose Down
DROP TABLE IF EXISTS message_images;
