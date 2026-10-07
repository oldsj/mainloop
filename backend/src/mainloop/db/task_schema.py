"""Greenfield task schema. No enrollment of retained sessions or branch writers."""

TASK_MIGRATION_SQL = """
CREATE TABLE IF NOT EXISTS project_provider_preferences (
 project_id TEXT PRIMARY KEY REFERENCES projects(id),
 profile_id TEXT, version BIGINT NOT NULL CHECK(version > 0)
);
CREATE TABLE IF NOT EXISTS tasks (
 id TEXT PRIMARY KEY, owner_id TEXT NOT NULL,
 project_id TEXT REFERENCES projects(id), topic_id TEXT REFERENCES topics(id), parent_task_id TEXT,
 root_task_id TEXT NOT NULL, creator_binding_id TEXT,
 mode TEXT NOT NULL CHECK(mode IN ('code','coordination')),
 status TEXT NOT NULL CHECK(status IN ('queued','running','waiting','blocked','completed','failed','cancelled')),
 current_attempt_id TEXT, version BIGINT NOT NULL CHECK(version > 0),
 snapshot JSONB NOT NULL, projection JSONB NOT NULL DEFAULT '{}',
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 UNIQUE(owner_id,id), UNIQUE(owner_id,project_id,id),
 CHECK(parent_task_id IS DISTINCT FROM id),
 CHECK(parent_task_id IS NOT NULL OR root_task_id=id),
 CHECK(mode <> 'code' OR project_id IS NOT NULL),
 FOREIGN KEY(owner_id,parent_task_id) REFERENCES tasks(owner_id,id),
 FOREIGN KEY(owner_id,root_task_id) REFERENCES tasks(owner_id,id) DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE IF NOT EXISTS task_attempts (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id),
 number BIGINT NOT NULL CHECK(number > 0),
 profile_id TEXT NOT NULL, native_provider TEXT NOT NULL CHECK(native_provider IN ('claude','codex')),
 configuration_revision TEXT NOT NULL, agent_ref JSONB NOT NULL,
 role TEXT NOT NULL, depth INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('creating','active','draining','fenced','superseded','failed','cancelled','completed')),
 capacity_held BOOLEAN NOT NULL DEFAULT TRUE,
 session_id TEXT UNIQUE, binding_id TEXT UNIQUE, workspace_id TEXT UNIQUE,
 writer_generation BIGINT CHECK(writer_generation > 0), snapshot JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 UNIQUE(task_id,number), UNIQUE(task_id,id),
 CHECK((role='supervisor' AND depth=1) OR (role='child' AND depth=2)),
 CHECK(state NOT IN ('creating','active','draining') OR capacity_held)
);
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='task_current_attempt_fk'
               AND conrelid='tasks'::regclass) THEN
 ALTER TABLE tasks ADD CONSTRAINT task_current_attempt_fk
 FOREIGN KEY(id,current_attempt_id) REFERENCES task_attempts(task_id,id) DEFERRABLE INITIALLY DEFERRED;
 END IF;
END $$;
CREATE TABLE IF NOT EXISTS workspace_writer_claims (
 owner_id TEXT NOT NULL, repository TEXT NOT NULL, branch TEXT NOT NULL,
 generation BIGINT NOT NULL CHECK(generation > 0),
 attempt_id TEXT UNIQUE REFERENCES task_attempts(id), binding_id TEXT UNIQUE,
 held BOOLEAN NOT NULL DEFAULT TRUE,
 fence_evidence_ref TEXT, fenced_at TIMESTAMPTZ,
 PRIMARY KEY(owner_id,repository,branch),
 CHECK(repository=lower(repository) AND repository ~ '^[a-z0-9_.-]+/[a-z0-9_.-]+$'),
 CHECK(NOT held OR attempt_id IS NOT NULL OR binding_id IS NOT NULL)
);
CREATE TABLE IF NOT EXISTS task_operations (
 id TEXT PRIMARY KEY, owner_id TEXT NOT NULL, principal_key TEXT NOT NULL,
 request_id TEXT NOT NULL, request_digest TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('create','retry','reassign','cancel')),
 task_id TEXT REFERENCES tasks(id), attempt_id TEXT REFERENCES task_attempts(id),
 state TEXT NOT NULL CHECK(state IN ('requested','draining','checkpoint_required','checkpoint_verified','source_fencing','source_fenced','target_creating','target_ready','completed','blocked','uncertain')),
 snapshot JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 UNIQUE(owner_id,principal_key,request_id)
);
CREATE TABLE IF NOT EXISTS task_events (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id), owner_id TEXT NOT NULL,
 version BIGINT NOT NULL, event_key TEXT NOT NULL, payload JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), published_at TIMESTAMPTZ,
 UNIQUE(task_id,event_key), UNIQUE(task_id,version),
 FOREIGN KEY(owner_id,task_id) REFERENCES tasks(owner_id,id)
);
CREATE INDEX IF NOT EXISTS task_owner_parent ON tasks(owner_id,parent_task_id);
CREATE INDEX IF NOT EXISTS task_event_pending ON task_events(created_at) WHERE published_at IS NULL;
CREATE OR REPLACE FUNCTION task_attempt_routing_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(OLD.task_id,OLD.number,OLD.profile_id,OLD.native_provider,OLD.configuration_revision,OLD.agent_ref,OLD.role,OLD.depth,OLD.snapshot->'environment',OLD.snapshot->'initial_ref')
 IS DISTINCT FROM ROW(NEW.task_id,NEW.number,NEW.profile_id,NEW.native_provider,NEW.configuration_revision,NEW.agent_ref,NEW.role,NEW.depth,NEW.snapshot->'environment',NEW.snapshot->'initial_ref') THEN
 RAISE EXCEPTION 'attempt routing is immutable';
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS task_attempt_routing_immutable ON task_attempts;
CREATE TRIGGER task_attempt_routing_immutable BEFORE UPDATE ON task_attempts
 FOR EACH ROW EXECUTE FUNCTION task_attempt_routing_immutable();
CREATE OR REPLACE FUNCTION validate_task_hierarchy() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE p tasks; r tasks; project_owner TEXT;
BEGIN
 IF NEW.topic_id IS NOT NULL AND (SELECT user_id FROM topics WHERE id=NEW.topic_id) IS DISTINCT FROM NEW.owner_id THEN
 RAISE EXCEPTION 'task topic owner mismatch'; END IF;
 IF NEW.project_id IS NOT NULL THEN
 SELECT user_id INTO project_owner FROM projects WHERE id=NEW.project_id;
 IF project_owner IS DISTINCT FROM NEW.owner_id THEN RAISE EXCEPTION 'task project owner mismatch'; END IF;
 END IF;
 SELECT * INTO r FROM tasks WHERE id=NEW.root_task_id;
 IF r.id IS NULL OR r.parent_task_id IS NOT NULL OR r.owner_id <> NEW.owner_id THEN
 RAISE EXCEPTION 'invalid task root'; END IF;
 IF NEW.parent_task_id IS NOT NULL THEN
 SELECT * INTO p FROM tasks WHERE id=NEW.parent_task_id;
 IF p.id IS NULL OR p.owner_id <> NEW.owner_id OR p.root_task_id <> NEW.root_task_id
    OR p.project_id IS DISTINCT FROM NEW.project_id OR p.parent_task_id IS NOT NULL THEN
 RAISE EXCEPTION 'invalid task ancestry'; END IF;
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS task_hierarchy ON tasks;
CREATE CONSTRAINT TRIGGER task_hierarchy AFTER INSERT OR UPDATE ON tasks
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION validate_task_hierarchy();
"""

TASK_MIGRATION_SQL += """
CREATE OR REPLACE FUNCTION task_identity_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(OLD.owner_id,OLD.project_id,OLD.topic_id,OLD.parent_task_id,OLD.root_task_id,OLD.creator_binding_id,OLD.mode)
 IS DISTINCT FROM ROW(NEW.owner_id,NEW.project_id,NEW.topic_id,NEW.parent_task_id,NEW.root_task_id,NEW.creator_binding_id,NEW.mode) THEN
 RAISE EXCEPTION 'task authority and ancestry are immutable'; END IF;
 IF NEW.version < OLD.version THEN RAISE EXCEPTION 'task version cannot decrease'; END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS task_identity_immutable ON tasks;
CREATE TRIGGER task_identity_immutable BEFORE UPDATE ON tasks FOR EACH ROW EXECUTE FUNCTION task_identity_immutable();
CREATE OR REPLACE FUNCTION validate_attempt_hierarchy() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE p tasks;
BEGIN
 SELECT * INTO p FROM tasks WHERE id=NEW.task_id;
 IF (p.parent_task_id IS NULL AND NEW.role <> 'supervisor') OR
    (p.parent_task_id IS NOT NULL AND NEW.role <> 'child') THEN
 RAISE EXCEPTION 'attempt role does not match task ancestry'; END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS task_attempt_hierarchy ON task_attempts;
CREATE TRIGGER task_attempt_hierarchy BEFORE INSERT ON task_attempts FOR EACH ROW EXECUTE FUNCTION validate_attempt_hierarchy();
"""

TASK_MIGRATION_SQL += """
CREATE UNIQUE INDEX IF NOT EXISTS task_one_unfenced_attempt
 ON task_attempts(task_id) WHERE state IN ('creating','active','draining');
"""

TASK_MIGRATION_SQL += """
-- Immutable checkpoint/manifest/summary references for S3; content is bounded,
-- canonical JSON produced by Mainloop, never runtime transcripts or credentials.
CREATE TABLE IF NOT EXISTS task_artifacts (
 id TEXT PRIMARY KEY, operation_id TEXT NOT NULL REFERENCES task_operations(id),
 kind TEXT NOT NULL CHECK(kind IN ('checkpoint','handoff_manifest','unverified_provider_summary')),
 content TEXT NOT NULL CHECK(octet_length(content) <= 32768),
 sha256 TEXT NOT NULL CHECK(sha256 ~ '^[0-9a-f]{64}$'),
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 UNIQUE(operation_id,kind),
 CHECK(kind <> 'unverified_provider_summary' OR octet_length(content) <= 8192)
);
CREATE OR REPLACE FUNCTION reject_task_artifact_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'task artifacts are immutable'; END $$;
DROP TRIGGER IF EXISTS immutable_task_artifact ON task_artifacts;
CREATE TRIGGER immutable_task_artifact BEFORE UPDATE OR DELETE ON task_artifacts
 FOR EACH ROW EXECUTE FUNCTION reject_task_artifact_mutation();
-- Reports are durable claims, separate from task completion and recipient delivery.
CREATE TABLE IF NOT EXISTS task_reports (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id),
 attempt_id TEXT NOT NULL, request_id TEXT NOT NULL, request_digest TEXT NOT NULL,
 snapshot JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 FOREIGN KEY(task_id,attempt_id) REFERENCES task_attempts(task_id,id),
 UNIQUE(attempt_id,request_id)
);
CREATE TABLE IF NOT EXISTS task_event_deliveries (
 event_id TEXT NOT NULL REFERENCES task_events(id),
 recipient_key TEXT NOT NULL, target_attempt_id TEXT REFERENCES task_attempts(id),
 delivery_id TEXT UNIQUE, state TEXT NOT NULL CHECK(state IN ('pending','queued','delivered','uncertain','cancelled')),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(event_id,recipient_key)
);
"""

TASK_MIGRATION_SQL += """
CREATE OR REPLACE FUNCTION writer_generation_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'writer claim tombstones must be retained'; END IF;
 IF NEW.generation < OLD.generation THEN RAISE EXCEPTION 'writer generation cannot decrease'; END IF;
 IF NEW.held AND (NOT OLD.held OR ROW(NEW.attempt_id,NEW.binding_id) IS DISTINCT FROM ROW(OLD.attempt_id,OLD.binding_id)) THEN
 IF NEW.generation <> OLD.generation+1 THEN RAISE EXCEPTION 'writer transfer must increment generation'; END IF;
 ELSE
 IF NEW.generation <> OLD.generation THEN RAISE EXCEPTION 'unexpected writer generation change'; END IF;
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS writer_generation_guard ON workspace_writer_claims;
CREATE TRIGGER writer_generation_guard BEFORE UPDATE OR DELETE ON workspace_writer_claims
 FOR EACH ROW EXECUTE FUNCTION writer_generation_guard();
"""
