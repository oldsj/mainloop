-- Sanitized source migration from mainloop base 6ddd1610bc2034e29b950ac93a14427b6c50ceb4.
-- NULL on preexisting bindings means their original dispatch history is unknown.
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS git_create_dispatched BOOLEAN;
ALTER TABLE native_bindings ALTER COLUMN git_create_dispatched SET DEFAULT FALSE;
CREATE TABLE IF NOT EXISTS git_enrollments (
 issuance_id TEXT PRIMARY KEY,
 binding_id TEXT NOT NULL,
 create_request_id TEXT NOT NULL,
 issuance_version BIGINT NOT NULL CHECK(issuance_version > 0),
 owner_id TEXT NOT NULL, project_id TEXT NOT NULL,
 repository TEXT NOT NULL, branch TEXT NOT NULL,
 plan JSONB NOT NULL, plan_digest TEXT NOT NULL,
 create_dispatched BOOLEAN NOT NULL DEFAULT FALSE,
 association JSONB,
 prepared_revision TEXT,
 reported_composition JSONB,
 warmup_state TEXT NOT NULL DEFAULT 'pending'
   CHECK(warmup_state IN ('pending','suspending','suspended','resuming','complete')),
 read_token_hash TEXT NOT NULL UNIQUE,
 read_state TEXT NOT NULL DEFAULT 'planned'
   CHECK(read_state IN ('planned','confirmed','published','revoked')),
 push_state TEXT NOT NULL CHECK(push_state IN ('absent','planned','published','revoked')),
 read_secret_uid TEXT, push_secret_uid TEXT,
 read_cleanup_pending BOOLEAN NOT NULL DEFAULT FALSE,
 push_cleanup_pending BOOLEAN NOT NULL DEFAULT FALSE,
 revoked_at TIMESTAMPTZ,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 UNIQUE(binding_id,create_request_id)
);
ALTER TABLE git_enrollments ADD COLUMN IF NOT EXISTS prepared_revision TEXT;
ALTER TABLE git_enrollments ADD COLUMN IF NOT EXISTS reported_composition JSONB;
CREATE UNIQUE INDEX IF NOT EXISTS git_enrollment_live_binding
 ON git_enrollments(binding_id) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS git_enrollment_cleanup ON git_enrollments(issuance_id)
 WHERE read_cleanup_pending OR push_cleanup_pending;
CREATE OR REPLACE FUNCTION mainloop_git_plan_immutable() RETURNS trigger AS $$
BEGIN
 IF (NEW.issuance_id,NEW.binding_id,NEW.create_request_id,NEW.issuance_version,
     NEW.owner_id,NEW.project_id,NEW.repository,NEW.branch,NEW.plan,NEW.plan_digest,
     NEW.read_token_hash) IS DISTINCT FROM
    (OLD.issuance_id,OLD.binding_id,OLD.create_request_id,OLD.issuance_version,
     OLD.owner_id,OLD.project_id,OLD.repository,OLD.branch,OLD.plan,OLD.plan_digest,
     OLD.read_token_hash)
    OR (OLD.association IS NOT NULL AND NEW.association IS DISTINCT FROM OLD.association)
    OR (OLD.prepared_revision IS NOT NULL AND NEW.prepared_revision IS DISTINCT FROM OLD.prepared_revision)
    OR (OLD.reported_composition IS NOT NULL AND NEW.reported_composition IS DISTINCT FROM OLD.reported_composition)
    OR (OLD.create_dispatched AND NOT NEW.create_dispatched)
    OR (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS NULL) THEN
   RAISE EXCEPTION 'immutable Git enrollment';
 END IF;
 RETURN NEW;
END $$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS git_plan_immutable ON git_enrollments;
CREATE TRIGGER git_plan_immutable BEFORE UPDATE ON git_enrollments
 FOR EACH ROW EXECUTE FUNCTION mainloop_git_plan_immutable();
ALTER TABLE push_publications ADD COLUMN IF NOT EXISTS owner_id TEXT;
ALTER TABLE push_publications ADD COLUMN IF NOT EXISTS repository TEXT;
ALTER TABLE push_publications ADD COLUMN IF NOT EXISTS branch TEXT;
ALTER TABLE push_publications ADD COLUMN IF NOT EXISTS transport_evidence JSONB;
ALTER TABLE push_publications ADD COLUMN IF NOT EXISTS transport_receipt JSONB;
-- Derive historical scope only from invariant owner and the immutable attempt.
UPDATE push_publications p SET owner_id=g.owner_id,
 repository=lower(p.attempt->>'repository'),
 branch=substring(p.attempt->'update'->>'ref' from '^refs/heads/(.+)$')
 FROM push_grants g WHERE g.id=p.grant_id AND p.owner_id IS NULL;
CREATE INDEX IF NOT EXISTS push_unresolved_branch
 ON push_publications(owner_id,repository,branch)
 WHERE state IN ('dispatching','unknown');
