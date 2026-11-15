-- +goose Up
-- The agent dotfiles each native Session was created with (docs/specs/agent-dotfiles.md), keyed by
-- the kagent create request id: a retried or replayed create sends the same request, so it gets the
-- same pin, and a new Session of the same Mainloop session (a provider switch) pins anew. A fork is
-- keyed "fork:<fork intent id>" and copies its source's pin. The first pin of a request wins.
CREATE TABLE session_dotfiles (
    request_id text PRIMARY KEY,
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    -- The source that applied: empty when none did.
    repository text NOT NULL DEFAULT '',
    ref text NOT NULL DEFAULT '',
    path text NOT NULL DEFAULT '',
    -- The commit the ref named when the Session was created; NULL when it could not be resolved.
    commit text CHECK (commit ~ '^[0-9a-f]{40}$'),
    digest text REFERENCES dotfiles_bundles (digest),
    status text NOT NULL CHECK (status IN ('ready', 'none', 'unavailable', 'refused')),
    reason text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'ready') = (digest IS NOT NULL)),
    -- A ready or refused pin names the commit it read.
    CHECK (commit IS NOT NULL OR status = 'none' OR status = 'unavailable')
);

CREATE INDEX session_dotfiles_session_idx ON session_dotfiles (session_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS session_dotfiles;
