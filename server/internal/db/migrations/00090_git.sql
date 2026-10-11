-- +goose Up
-- Git transport: the repository each workspace may read through Mainloop, the push gate's grants
-- and publication ledger, and the per-project branch policy. The bearer behind an enrollment is a
-- 'git_read' (or 'git_push') row in credentials; its Secret is published and revoked there. These
-- tables hold what the credential row does not: which repository it is bound to, and the push
-- side's durable state.
--
-- Issuing is a reconcile: the enrollment row is the intent ('planned'), the credential and its
-- Secret are made with no transaction open, and only then is the row marked 'published'. A replay
-- finds the row and resumes. Authentication never trusts this state alone: it also needs the
-- credential to be published and the workspace's session to be live, so a workspace that ends
-- loses its read capability even before the enrollment row is marked revoked.

CREATE TABLE git_enrollments (
    workspace_id text PRIMARY KEY REFERENCES workspaces (id) ON DELETE CASCADE,
    -- "owner/name", lower-cased: GitHub names are case-insensitive.
    repository text NOT NULL CHECK (repository ~ '^[a-z0-9][a-z0-9-]{0,38}/[a-z0-9._-]{1,100}$'),
    state text NOT NULL DEFAULT 'planned' CHECK (state IN ('planned', 'published', 'revoked')),
    credential_id text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    -- Revocable before a credential is linked, as the grants below; only published needs one.
    CHECK (state <> 'published' OR credential_id IS NOT NULL),
    CHECK ((state = 'revoked') = (revoked_at IS NOT NULL))
);
-- The sweep that revokes enrollments looks at the ones still live.
CREATE INDEX git_enrollments_live ON git_enrollments (updated_at) WHERE state <> 'revoked';

-- What a workspace may push: one branch of its enrolled repository, under the project's policy
-- version at the time of the grant. The push gate (not this slice) writes and reads these.
CREATE TABLE git_push_grants (
    workspace_id text PRIMARY KEY REFERENCES git_enrollments (workspace_id) ON DELETE CASCADE,
    branch text NOT NULL CHECK (branch <> ''),
    policy_version bigint NOT NULL CHECK (policy_version > 0),
    credential_id text,
    state text NOT NULL DEFAULT 'planned' CHECK (state IN ('planned', 'published', 'revoked')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    -- A grant may be revoked before its credential is linked (enrollment interrupted); only a
    -- published grant needs the credential.
    CHECK (state <> 'published' OR credential_id IS NOT NULL),
    CHECK ((state = 'revoked') = (revoked_at IS NOT NULL))
);

-- Every push the gate dispatched: an intent row with an explicit state and the workspace's
-- request id as its idempotency key (UNIQUE with the workspace). The gate claims rows through
-- reconcile.Table, so the row carries the claim columns (plan §4.3). workspace_id is plain text,
-- not a foreign key: this is the audit ledger and outlives the workspace, so deleting a session
-- must not be blocked by (or erase) its publications.
--
-- 'dispatching' and 'unknown' are unresolved: the remote may or may not have taken the
-- update, so the gate reads GitHub before it acts again and never replays the body.
CREATE TABLE git_publications (
    id text PRIMARY KEY,
    workspace_id text NOT NULL,
    request_id text NOT NULL,
    repository text NOT NULL,
    branch text NOT NULL,
    -- The validated update, frozen when the row is made.
    attempt jsonb NOT NULL,
    state text NOT NULL DEFAULT 'pending' CHECK (
        state IN ('pending', 'dispatching', 'confirmed', 'rejected', 'unknown')
    ),
    transport_evidence jsonb,
    transport_receipt jsonb,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, request_id)
);
CREATE INDEX git_publications_unresolved ON git_publications (repository, branch)
WHERE state IN ('dispatching', 'unknown');
-- The claim index: only the states a loop works on (the same list as the reconcile.Table).
CREATE INDEX git_publications_claim ON git_publications (next_attempt_at, id)
WHERE state IN ('pending', 'dispatching', 'unknown');

-- The branch rules of a project, versioned: a grant records the version it was made under.
CREATE TABLE git_branch_policies (
    project_id text PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    policy jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS git_branch_policies;
DROP TABLE IF EXISTS git_publications;
DROP TABLE IF EXISTS git_push_grants;
DROP TABLE IF EXISTS git_enrollments;
