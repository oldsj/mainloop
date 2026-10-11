-- +goose Up
-- Preview keep-awake leases. A lease row is one open preview stream (a WebSocket, or an HTTP
-- response that outlives a short request). The holder renews it while the stream lives and
-- deletes it on close; the keep-awake sweep treats the owner as active while any unexpired
-- lease exists. A holder that dies simply stops renewing, so its lease lapses on its own.

CREATE TABLE preview_leases (
    id text PRIMARY KEY,
    workspace_id text NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- The process that opened the stream, for diagnosis only.
    holder text NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX preview_leases_workspace ON preview_leases (workspace_id, expires_at);
CREATE INDEX preview_leases_expiry ON preview_leases (expires_at);

-- +goose Down
DROP TABLE IF EXISTS preview_leases;
