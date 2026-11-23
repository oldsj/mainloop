-- +goose Up
-- The open-web web_fetch MCP tool (docs/specs/web-fetch.md), opt-in per agent. An agent holds it
-- when its own session's flag is set (the owner's POST /sessions/{id}/web-fetch) or the task it
-- runs was created with it. Both default to off, so no existing agent gains it.
ALTER TABLE agent_bindings ADD COLUMN web_fetch boolean NOT NULL DEFAULT false;
ALTER TABLE tasks ADD COLUMN web_fetch boolean NOT NULL DEFAULT false;

-- One row per fetch, written before the network call in the transaction that re-checks the
-- caller's MCP credential; the per-owner rate limit counts these rows.
CREATE TABLE web_fetches (
    id text PRIMARY KEY,
    owner_id text NOT NULL,
    session_id text REFERENCES sessions (id) ON DELETE SET NULL,
    url text NOT NULL CHECK (length(url) BETWEEN 1 AND 2048),
    final_url text CHECK (final_url IS NULL OR length(final_url) <= 8192),
    status integer CHECK (status IS NULL OR status BETWEEN 100 AND 599),
    bytes integer CHECK (bytes IS NULL OR bytes >= 0),
    error_code text CHECK (error_code IS NULL OR error_code ~ '^[a-z_]{1,40}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);
CREATE INDEX web_fetches_owner_created ON web_fetches (owner_id, created_at);

-- +goose Down
DROP TABLE web_fetches;
ALTER TABLE tasks DROP COLUMN web_fetch;
ALTER TABLE agent_bindings DROP COLUMN web_fetch;
