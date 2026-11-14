-- +goose Up
-- The owner's default model and reasoning effort for new turns, by role and provider profile
-- (docs/specs/chat.md#session-defaults). A turn uses the session's own /model or /effort override,
-- else the default for its role ('main' for the main thread, 'worker' for every other session)
-- and its binding's provider, else nothing (the provider's own default). Read at send time, so a
-- change reaches running sessions from their next turn. Keyed by provider because model names and
-- effort levels differ between providers.
CREATE TABLE session_defaults (
    user_id text NOT NULL,
    role text NOT NULL CHECK (role IN ('main', 'worker')),
    provider text NOT NULL CHECK (provider ~ '^[a-z][a-z0-9_-]{0,79}$'),
    model text CHECK (model IS NULL OR model ~ '^[A-Za-z0-9._:/-]{1,200}$'),
    effort text CHECK (effort IS NULL OR effort ~ '^[a-z]{1,20}$'),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, role, provider),
    CHECK (model IS NOT NULL OR effort IS NOT NULL)
);

-- +goose Down
DROP TABLE session_defaults;
