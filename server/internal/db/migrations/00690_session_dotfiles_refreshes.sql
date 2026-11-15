-- +goose Up
-- The owner's refresh of a running native Session's agent dotfiles to the source's current commit
-- (docs/specs/agent-dotfiles.md). The row is the durable intent, written before any kagent call: id is
-- the caller's idempotency key and kagent_session_id the Session it updates. Its pin is the
-- session_dotfiles row keyed "refresh:<id>", which the session shows once the refresh is applied.
-- state is pending until kagent holds the pinned dotfiles (applied) or the refresh cannot apply
-- (failed, with a reason). A pending row is continued by repeating the request or by the resume
-- sweep; each attempt reads the Session from kagent first, and never targets another Session. At
-- most one refresh per session is pending at a time.
CREATE TABLE session_dotfiles_refreshes (
    id text PRIMARY KEY,
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    kagent_session_id text NOT NULL,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'applied', 'failed')),
    reason text NOT NULL DEFAULT '' CHECK ((state = 'failed') = (reason <> '')),
    -- The last error of an attempt kagent did not answer, for the log and the next attempt.
    detail text,
    attempts integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX session_dotfiles_refreshes_one_pending ON session_dotfiles_refreshes (session_id)
WHERE state = 'pending';
CREATE INDEX session_dotfiles_refreshes_session_idx ON session_dotfiles_refreshes (session_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS session_dotfiles_refreshes;
