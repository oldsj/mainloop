-- +goose Up
-- A project's own limit on its worker tasks (child tasks) holding a slot at once; NULL uses the
-- configured default (TASK_MAX_WORKERS_PER_PROJECT). See docs/specs/tasks.md#capacity.
ALTER TABLE projects ADD COLUMN max_workers integer
    CONSTRAINT projects_max_workers CHECK (max_workers BETWEEN 1 AND 100);

-- One row per change of a project's worker limit, by the owner (API) or the main session (MCP
-- project_settings). The row and the change commit together; rows are never updated or deleted.
CREATE TABLE project_worker_limit_audit (
    id bigserial PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    owner_id text NOT NULL,
    changed_by text NOT NULL CHECK (changed_by IN ('owner', 'main')),
    session_id text,
    old_max_workers integer,
    new_max_workers integer,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((changed_by = 'main') = (session_id IS NOT NULL))
);

CREATE INDEX project_worker_limit_audit_project ON project_worker_limit_audit (project_id, id);

-- +goose StatementBegin
CREATE FUNCTION project_worker_limit_audit_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'Worker limit audit rows are immutable';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER project_worker_limit_audit_immutable BEFORE UPDATE ON project_worker_limit_audit
FOR EACH ROW EXECUTE FUNCTION project_worker_limit_audit_immutable();

-- Capacity refusals now also count project_capacity.
ALTER TABLE task_capacity_refusals DROP CONSTRAINT task_capacity_refusals_reason_check;
ALTER TABLE task_capacity_refusals ADD CONSTRAINT task_capacity_refusals_reason_check
    CHECK (reason IN ('global_capacity', 'child_capacity', 'project_capacity'));

-- +goose Down
DELETE FROM task_capacity_refusals WHERE reason = 'project_capacity';
ALTER TABLE task_capacity_refusals DROP CONSTRAINT task_capacity_refusals_reason_check;
ALTER TABLE task_capacity_refusals ADD CONSTRAINT task_capacity_refusals_reason_check
    CHECK (reason IN ('global_capacity', 'child_capacity'));
DROP TABLE IF EXISTS project_worker_limit_audit;
DROP FUNCTION IF EXISTS project_worker_limit_audit_immutable();
ALTER TABLE projects DROP COLUMN IF EXISTS max_workers;
