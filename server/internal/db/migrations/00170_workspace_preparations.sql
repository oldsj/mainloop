-- +goose Up
-- A workspace created with a development environment must have its workspace prepared by kagent
-- before the runtime admits any message. Preparation is an intent keyed by the workspace's create
-- request: planned (nothing asked yet), requested (the profile is frozen and kagent was or may be
-- asked), confirmed or failed. A workspace with no environment has no row and needs none.
CREATE TABLE workspace_preparations (
    id text PRIMARY KEY REFERENCES workspaces (id) ON DELETE CASCADE,
    idempotency_key text NOT NULL UNIQUE,
    action_id text NOT NULL,
    state text NOT NULL DEFAULT 'planned' CHECK (state IN ('planned', 'requested', 'confirmed', 'failed')),
    setup_profile text NOT NULL DEFAULT '',
    setup_digest text NOT NULL DEFAULT '',
    detail text NOT NULL DEFAULT '',
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX workspace_preparations_claim ON workspace_preparations (next_attempt_at, id)
WHERE state IN ('planned', 'requested');

-- +goose Down
DROP TABLE workspace_preparations;
