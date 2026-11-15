-- +goose Up
-- Issue writes (internal/issues, docs/specs/issue-writes.md): create_issue and comment_issue. Each
-- call writes one row first; a reconcile loop posts at most once and finds a write whose answer was
-- lost by the marker the body carries, so a repeated or cut-off call never files twice.
--
--   issue_writes.state
--     pending      written; nothing has been sent
--     dispatching  the claimer found no write carrying the marker and is about to POST (or did)
--     uncertain    the POST may or may not have landed; resolved by listing, never by resending
--     created      final: the issue or comment exists (number, url, and comment_id for a comment)
--     refused      final: GitHub or Mainloop refused it, or nothing appeared (detail says why)
CREATE TABLE issue_writes (
    id text PRIMARY KEY,
    -- "issue:<owner>:<request id>": a replayed tool call finds its row by this key.
    idempotency_key text NOT NULL UNIQUE,
    owner_id text NOT NULL,
    project_id text NOT NULL,
    -- The Mainloop session that asked.
    session_id text,
    request_id text NOT NULL,
    -- sha256 of the canonical arguments (kind included) without the request id: the same request
    -- id with other arguments is a conflict.
    payload_hash text NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    kind text NOT NULL CHECK (kind IN ('issue', 'comment')),
    repo_id bigint NOT NULL,
    repo_full_name text NOT NULL,
    -- An issue's title and labels; empty for a comment.
    title text NOT NULL DEFAULT '',
    labels text[] NOT NULL DEFAULT '{}',
    -- The text as sent: the agent's body and the marker line.
    body text NOT NULL,
    -- The issue: a comment's target from the start, a new issue's number once created.
    issue_number bigint,
    comment_id bigint,
    url text,
    state text NOT NULL DEFAULT 'pending' CHECK (
        state IN ('pending', 'dispatching', 'uncertain', 'created', 'refused')
    ),
    detail text,
    attempts integer NOT NULL DEFAULT 0,
    -- Lookups made while uncertain, so the recheck delay can grow.
    rechecks integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (kind = 'issue' OR (issue_number IS NOT NULL AND title = '' AND labels = '{}')),
    CHECK (
        (state = 'created') = (
            url IS NOT NULL AND issue_number IS NOT NULL AND (kind = 'issue' OR comment_id IS NOT NULL)
        )
    )
);
CREATE INDEX issue_writes_claim ON issue_writes (next_attempt_at, id)
WHERE state IN ('pending', 'dispatching', 'uncertain');
CREATE INDEX issue_writes_project ON issue_writes (project_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS issue_writes;
