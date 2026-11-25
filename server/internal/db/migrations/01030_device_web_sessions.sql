-- +goose Up
-- A registered phone's web session (docs/specs/devices.md, "The app's web view"): the iPhone app
-- shows the web app in a web view, which can't sign each request with the device key. The app
-- trades one signed request for a random token that the web view sends as an HttpOnly cookie. The
-- server keeps only the token's SHA-256. A session acts as its device, so revoking the device ends
-- it at once (the verifier reads the device on every request), and it ends at expires_at.
CREATE TABLE device_web_sessions (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    device_id text NOT NULL REFERENCES owner_devices (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    CHECK (expires_at > created_at)
);

CREATE INDEX device_web_sessions_device ON device_web_sessions (device_id);
CREATE INDEX device_web_sessions_expires ON device_web_sessions (expires_at);

-- +goose Down
DROP TABLE device_web_sessions;
