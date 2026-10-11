-- +goose Up
-- Tasks: delegated work that Mainloop tracks apart from the native sessions that run it.
-- Task lifecycle (tasks), agent activity (task_attempts, the sessions behind them), message
-- delivery (task_event_deliveries), owner attention (owner_decision) and publication are separate
-- concepts, and each has its own columns here.
--
-- Every owner or agent request that changes durable state is a task_operations row: an intent
-- with an explicit state and an idempotency key (owner, principal, request id) whose request
-- digest turns a replay with a different body into a conflict. The reconcile loop drives
-- operations and event deliveries (internal/tasks): both carry the intent-row columns attempts,
-- next_attempt_at, lease_owner and lease_until that internal/reconcile claims rows by.
--
-- Free-form values stay jsonb: the accepted environment (a frozen object read whole), an
-- operation's request payload, and an artifact's content. Everything else is a column.
--
-- The invariants that protect durable state are enforced here and not only in Go:
--   * a task's owner, project, topic, ancestry, creator and mode never change, and its version
--     never decreases;
--   * a task belongs to one root, children have exactly one parent level, and a supervisor
--     attempt belongs to a root task and a child attempt to a child task;
--   * an attempt's routing never changes after it is inserted;
--   * a task has at most one unfenced attempt (creating, active or draining);
--   * the writer generation of a branch only moves forward, by exactly one per transfer.

CREATE TABLE tasks (
    id text PRIMARY KEY,
    owner_id text NOT NULL,
    project_id text REFERENCES projects (id),
    topic_id text REFERENCES topics (id),
    parent_task_id text,
    root_task_id text NOT NULL,
    -- The binding that created the task: null for an owner request. No foreign key, so the task
    -- outlives the session.
    creator_binding_id text,
    title text NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
    brief text NOT NULL CHECK (char_length(brief) BETWEEN 1 AND 16384 AND octet_length(brief) <= 16384),
    mode text NOT NULL CHECK (mode IN ('code', 'coordination')),
    assigned_profile_id text NOT NULL,
    selection_source text NOT NULL CHECK (
        selection_source IN ('explicit', 'project_default', 'installation_default', 'inherited')
    ),
    provider_constraint text,
    -- The environment version frozen when the first attempt was accepted: a WorkspaceEnvironment.
    accepted_environment jsonb CHECK (jsonb_typeof(accepted_environment) = 'object'),
    status text NOT NULL DEFAULT 'queued' CHECK (
        status IN ('queued', 'running', 'waiting', 'blocked', 'completed', 'failed', 'cancelled')
    ),
    reason text CHECK (
        reason IN (
            'owner_decision', 'awaiting_child', 'approval', 'ci', 'publication', 'handoff',
            'reconciliation', 'provisioning_unavailable', 'handoff_unavailable', 'cancel_unavailable'
        )
    ),
    -- The unresolved report request that is waiting for the owner. All three or none.
    decision_attempt_id text,
    decision_report_id text,
    decision_action text CHECK (char_length(decision_action) BETWEEN 1 AND 4000),
    current_attempt_id text,
    version integer NOT NULL DEFAULT 1 CHECK (version >= 1),
    -- The checkout a code task asked for. ref '' is the remote's default branch, depth 0 the
    -- runtime default.
    checkout_branch text CHECK (char_length(checkout_branch) BETWEEN 1 AND 255),
    checkout_ref text CHECK (char_length(checkout_ref) <= 255),
    checkout_depth integer CHECK (checkout_depth BETWEEN 0 AND 1000),
    -- What the GitHub observation last saw of the task's pull request. A cache: nothing here is
    -- evidence of completion. pr_state 'unknown' with a null number means no pull request yet.
    pr_number integer CHECK (pr_number >= 1),
    pr_url text,
    pr_head_sha text,
    pr_state text NOT NULL DEFAULT 'unknown' CHECK (pr_state IN ('open', 'closed', 'merged', 'unknown')),
    ci_state text NOT NULL DEFAULT 'unknown' CHECK (ci_state IN ('pending', 'success', 'failure', 'unknown')),
    ci_head_sha text,
    projected_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner_id, id),
    CONSTRAINT tasks_pr_whole CHECK (pr_number IS NOT NULL OR (pr_url IS NULL AND pr_head_sha IS NULL AND pr_state = 'unknown')),
    CONSTRAINT tasks_no_self_parent CHECK (parent_task_id IS DISTINCT FROM id),
    CONSTRAINT tasks_root CHECK (parent_task_id IS NOT NULL OR root_task_id = id),
    CONSTRAINT tasks_code_has_project CHECK (mode <> 'code' OR project_id IS NOT NULL),
    CONSTRAINT tasks_checkout_whole CHECK (
        (checkout_branch IS NULL AND checkout_ref IS NULL AND checkout_depth IS NULL)
        OR (checkout_branch IS NOT NULL AND checkout_ref IS NOT NULL AND checkout_depth IS NOT NULL)
    ),
    CONSTRAINT tasks_checkout_by_mode CHECK ((mode = 'code') = (checkout_branch IS NOT NULL)),
    CONSTRAINT tasks_decision_whole CHECK (
        (decision_attempt_id IS NULL AND decision_report_id IS NULL AND decision_action IS NULL)
        OR (decision_attempt_id IS NOT NULL AND decision_report_id IS NOT NULL AND decision_action IS NOT NULL)
    ),
    -- Which reasons go with which status. internal/tasks holds the same table and a test checks
    -- every pair against this constraint.
    CONSTRAINT tasks_status_reason CHECK (
        CASE status
            WHEN 'queued' THEN reason IS NULL OR reason = 'provisioning_unavailable'
            WHEN 'running' THEN reason IS NULL
            WHEN 'waiting' THEN reason IS NOT NULL AND (
                reason = 'awaiting_child' OR reason = 'approval' OR reason = 'ci' OR reason = 'publication'
                OR reason = 'handoff' OR reason = 'reconciliation'
            )
            WHEN 'blocked' THEN reason IS NOT NULL AND (reason = 'owner_decision' OR reason = 'reconciliation')
            ELSE reason IS NULL
        END
    ),
    FOREIGN KEY (owner_id, parent_task_id) REFERENCES tasks (owner_id, id),
    FOREIGN KEY (owner_id, root_task_id) REFERENCES tasks (owner_id, id) DEFERRABLE INITIALLY DEFERRED
);
CREATE INDEX tasks_owner_created ON tasks (owner_id, created_at, id);
CREATE INDEX tasks_parent ON tasks (owner_id, parent_task_id);
CREATE INDEX tasks_project ON tasks (owner_id, project_id);

CREATE TABLE task_attempts (
    id text PRIMARY KEY,
    task_id text NOT NULL REFERENCES tasks (id),
    number integer NOT NULL CHECK (number >= 1),
    profile_id text NOT NULL,
    native_provider text NOT NULL CHECK (native_provider IN ('claude', 'codex')),
    configuration_revision text NOT NULL,
    -- The kagent Agent resolved from the profile when the attempt was made.
    agent_namespace text NOT NULL,
    agent_name text NOT NULL,
    role text NOT NULL CHECK (role IN ('supervisor', 'child')),
    depth integer NOT NULL,
    state text NOT NULL DEFAULT 'creating' CHECK (
        state IN ('creating', 'active', 'draining', 'fenced', 'superseded', 'failed', 'cancelled', 'completed')
    ),
    -- Whether the attempt still counts against the task caps. An attempt of unknown state keeps
    -- its capacity until a fence is confirmed.
    capacity_held boolean NOT NULL DEFAULT true,
    -- The native session, binding and workspace are the same id. No foreign key: the attempt is
    -- audit history and outlives the session.
    session_id text UNIQUE,
    binding_id text UNIQUE,
    workspace_id text UNIQUE,
    writer_generation integer CHECK (writer_generation >= 1),
    brief_delivery_id text,
    predecessor_id text REFERENCES task_attempts (id),
    successor_id text REFERENCES task_attempts (id),
    evidence_refs text[] NOT NULL DEFAULT '{}' CHECK (cardinality(evidence_refs) <= 64),
    result_ref text CHECK (char_length(result_ref) BETWEEN 1 AND 2048),
    -- The environment the workspace was created with: a WorkspaceEnvironment.
    environment jsonb CHECK (jsonb_typeof(environment) = 'object'),
    initial_ref text,
    checkpoint_ref text CHECK (char_length(checkpoint_ref) BETWEEN 1 AND 2048),
    manifest_ref text CHECK (char_length(manifest_ref) BETWEEN 1 AND 2048),
    superseded_at timestamptz,
    archived_at timestamptz,
    native_deleted_at timestamptz,
    retention_hold text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (task_id, number),
    UNIQUE (task_id, id),
    CONSTRAINT task_attempts_role_depth CHECK (
        (role = 'supervisor' AND depth = 1) OR (role = 'child' AND depth = 2)
    ),
    CONSTRAINT task_attempts_unfenced_hold_capacity CHECK (
        state NOT IN ('creating', 'active', 'draining') OR capacity_held
    )
);
-- A task has at most one attempt that has not been fenced.
CREATE UNIQUE INDEX task_attempts_one_unfenced ON task_attempts (task_id)
WHERE state IN ('creating', 'active', 'draining');
CREATE INDEX task_attempts_capacity ON task_attempts (task_id) WHERE capacity_held;

-- A task points at its current attempt, and the attempt must be one of the task's own.
ALTER TABLE tasks
ADD CONSTRAINT tasks_current_attempt FOREIGN KEY (id, current_attempt_id)
REFERENCES task_attempts (task_id, id) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE task_operations (
    id text PRIMARY KEY,
    owner_id text NOT NULL,
    -- 'owner' or 'binding:<session id>': who asked.
    principal_key text NOT NULL,
    request_id text NOT NULL CHECK (char_length(request_id) BETWEEN 1 AND 100),
    -- sha256 of the canonical request: a replay with another body is a conflict.
    request_digest text NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    request_payload jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(request_payload) = 'object'),
    kind text NOT NULL CHECK (kind IN ('create', 'retry', 'reassign', 'cancel')),
    task_id text REFERENCES tasks (id),
    attempt_id text REFERENCES task_attempts (id),
    state text NOT NULL DEFAULT 'requested' CHECK (
        state IN (
            'requested', 'draining', 'checkpoint_required', 'checkpoint_verified', 'source_fencing',
            'source_fenced', 'target_creating', 'target_ready', 'completed', 'blocked', 'uncertain'
        )
    ),
    last_confirmed_step text NOT NULL DEFAULT 'requested' CHECK (
        last_confirmed_step IN (
            'requested', 'draining', 'checkpoint_required', 'checkpoint_verified', 'source_fencing',
            'source_fenced', 'target_creating', 'target_ready', 'completed', 'blocked', 'uncertain'
        )
    ),
    source_attempt_id text REFERENCES task_attempts (id),
    target_attempt_id text REFERENCES task_attempts (id),
    checkpoint_ref text CHECK (char_length(checkpoint_ref) BETWEEN 1 AND 2048),
    manifest_ref text CHECK (char_length(manifest_ref) BETWEEN 1 AND 2048),
    reason text CHECK (
        reason IN (
            'owner_decision', 'awaiting_child', 'approval', 'ci', 'publication', 'handoff',
            'reconciliation', 'provisioning_unavailable', 'handoff_unavailable', 'cancel_unavailable'
        )
    ),
    -- Reconcile intent columns: see internal/reconcile.
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner_id, principal_key, request_id)
);
CREATE INDEX task_operations_task ON task_operations (task_id, created_at, id);
CREATE INDEX task_operations_open ON task_operations (created_at, id)
WHERE state NOT IN ('completed', 'blocked');
-- The reconcile loop claims operations in these states. An uncertain one is claimed too: its
-- handler looks at what exists before it does anything.
CREATE INDEX task_operations_claim ON task_operations (next_attempt_at, id)
WHERE state IN ('requested', 'draining', 'source_fencing', 'source_fenced', 'target_creating', 'target_ready');

-- Immutable checkpoint, manifest and summary content. Bounded canonical JSON that Mainloop
-- produced, never a transcript or a credential.
CREATE TABLE task_artifacts (
    id text PRIMARY KEY,
    operation_id text NOT NULL REFERENCES task_operations (id),
    kind text NOT NULL CHECK (
        kind IN ('checkpoint', 'handoff_manifest', 'unverified_provider_summary', 'retention_receipt')
    ),
    content text NOT NULL CHECK (octet_length(content) <= 32768),
    sha256 text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (operation_id, kind),
    CONSTRAINT task_artifacts_summary_size CHECK (
        kind <> 'unverified_provider_summary' OR octet_length(content) <= 8192
    )
);

-- A report is a durable claim by an attempt, apart from the task state it moves and from who
-- is told about it.
CREATE TABLE task_reports (
    id text PRIMARY KEY,
    task_id text NOT NULL,
    attempt_id text NOT NULL,
    request_id text NOT NULL CHECK (char_length(request_id) BETWEEN 1 AND 100),
    request_digest text NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    summary text NOT NULL CHECK (char_length(summary) BETWEEN 1 AND 4000),
    outcome text NOT NULL CHECK (outcome IN ('progress', 'completed', 'failed', 'blocked')),
    owner_action text CHECK (char_length(owner_action) BETWEEN 1 AND 4000),
    evidence_refs text[] NOT NULL DEFAULT '{}' CHECK (cardinality(evidence_refs) <= 64),
    -- The task version this report produced.
    task_version integer NOT NULL CHECK (task_version >= 1),
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (task_id, attempt_id) REFERENCES task_attempts (task_id, id),
    UNIQUE (attempt_id, request_id),
    CONSTRAINT task_reports_owner_action CHECK (owner_action IS NULL OR outcome = 'blocked')
);
CREATE INDEX task_reports_task ON task_reports (task_id, created_at, id);

-- Who still has to be told about a report. A row is written with the report; the dispatcher
-- moves it on and never replays a delivery whose state is uncertain.
CREATE TABLE task_event_deliveries (
    -- The delivery's own id; the message id of its first send derives from it.
    id text NOT NULL UNIQUE,
    report_id text NOT NULL REFERENCES task_reports (id),
    -- 'main' or 'parent-task:<task id>': written by the server, never taken from a request.
    recipient_key text NOT NULL,
    target_attempt_id text REFERENCES task_attempts (id),
    delivery_id text UNIQUE,
    state text NOT NULL DEFAULT 'pending' CHECK (
        state IN ('pending', 'queued', 'delivered', 'uncertain', 'cancelled')
    ),
    -- How many messages have been recorded for this delivery: a send that failed in the ledger is
    -- recorded again under a new message id, up to a bound.
    sends integer NOT NULL DEFAULT 0,
    -- Reconcile intent columns: see internal/reconcile.
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (report_id, recipient_key)
);
CREATE INDEX task_event_deliveries_pending ON task_event_deliveries (updated_at) WHERE state = 'pending';
-- Pending rows are sent; queued rows are watched until the ledger settles them.
CREATE INDEX task_event_deliveries_claim ON task_event_deliveries (next_attempt_at, id)
WHERE state IN ('pending', 'queued');

-- One writer per repository branch. The generation is the fencing token: a transfer to another
-- holder moves it up by exactly one, and nothing moves it down.
CREATE TABLE workspace_writer_claims (
    owner_id text NOT NULL,
    repository text NOT NULL CHECK (repository = lower(repository) AND repository ~ '^[a-z0-9_.-]+/[a-z0-9_.-]+$'),
    branch text NOT NULL,
    generation integer NOT NULL CHECK (generation >= 1),
    attempt_id text UNIQUE REFERENCES task_attempts (id),
    binding_id text UNIQUE,
    held boolean NOT NULL DEFAULT true,
    fence_evidence_ref text,
    fenced_at timestamptz,
    PRIMARY KEY (owner_id, repository, branch),
    CHECK (NOT held OR attempt_id IS NOT NULL OR binding_id IS NOT NULL)
);

-- +goose StatementBegin
CREATE FUNCTION task_identity_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.owner_id, OLD.project_id, OLD.topic_id, OLD.parent_task_id, OLD.root_task_id, OLD.creator_binding_id, OLD.mode)
        IS DISTINCT FROM ROW(NEW.owner_id, NEW.project_id, NEW.topic_id, NEW.parent_task_id, NEW.root_task_id, NEW.creator_binding_id, NEW.mode) THEN
        RAISE EXCEPTION 'task authority and ancestry are immutable';
    END IF;
    IF NEW.version < OLD.version THEN
        RAISE EXCEPTION 'task version cannot decrease';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER task_identity_immutable BEFORE UPDATE ON tasks
FOR EACH ROW EXECUTE FUNCTION task_identity_immutable();

-- A task's topic and project belong to its owner, the root is a root, and a child's parent is a
-- root in the same project. Checked at commit, so a root and its first row can be written in any
-- order inside one transaction.
-- +goose StatementBegin
CREATE FUNCTION validate_task_hierarchy() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    p tasks;
    r tasks;
BEGIN
    IF NEW.topic_id IS NOT NULL
        AND (SELECT user_id FROM topics WHERE id = NEW.topic_id) IS DISTINCT FROM NEW.owner_id THEN
        RAISE EXCEPTION 'task topic owner mismatch';
    END IF;
    IF NEW.project_id IS NOT NULL
        AND (SELECT user_id FROM projects WHERE id = NEW.project_id) IS DISTINCT FROM NEW.owner_id THEN
        RAISE EXCEPTION 'task project owner mismatch';
    END IF;
    SELECT * INTO r FROM tasks WHERE id = NEW.root_task_id;
    IF r.id IS NULL OR r.parent_task_id IS NOT NULL OR r.owner_id <> NEW.owner_id THEN
        RAISE EXCEPTION 'invalid task root';
    END IF;
    IF NEW.parent_task_id IS NOT NULL THEN
        SELECT * INTO p FROM tasks WHERE id = NEW.parent_task_id;
        IF p.id IS NULL OR p.owner_id <> NEW.owner_id OR p.root_task_id <> NEW.root_task_id
            OR p.project_id IS DISTINCT FROM NEW.project_id OR p.parent_task_id IS NOT NULL THEN
            RAISE EXCEPTION 'invalid task ancestry';
        END IF;
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER task_hierarchy AFTER INSERT OR UPDATE ON tasks
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION validate_task_hierarchy();

-- +goose StatementBegin
CREATE FUNCTION validate_attempt_hierarchy() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    t tasks;
BEGIN
    SELECT * INTO t FROM tasks WHERE id = NEW.task_id;
    IF (t.parent_task_id IS NULL AND NEW.role <> 'supervisor')
        OR (t.parent_task_id IS NOT NULL AND NEW.role <> 'child') THEN
        RAISE EXCEPTION 'attempt role does not match task ancestry';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER task_attempt_hierarchy BEFORE INSERT ON task_attempts
FOR EACH ROW EXECUTE FUNCTION validate_attempt_hierarchy();

-- +goose StatementBegin
CREATE FUNCTION task_attempt_routing_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.task_id, OLD.number, OLD.profile_id, OLD.native_provider, OLD.configuration_revision, OLD.agent_namespace, OLD.agent_name, OLD.role, OLD.depth, coalesce(OLD.environment, NEW.environment), OLD.initial_ref)
        IS DISTINCT FROM ROW(NEW.task_id, NEW.number, NEW.profile_id, NEW.native_provider, NEW.configuration_revision, NEW.agent_namespace, NEW.agent_name, NEW.role, NEW.depth, NEW.environment, NEW.initial_ref) THEN
        RAISE EXCEPTION 'attempt routing is immutable';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER task_attempt_routing_immutable BEFORE UPDATE ON task_attempts
FOR EACH ROW EXECUTE FUNCTION task_attempt_routing_immutable();

-- An operation's identity and request never change: the idempotency key and digest are what a
-- replay is compared with.
-- +goose StatementBegin
CREATE FUNCTION task_operation_request_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.owner_id, OLD.principal_key, OLD.request_id, OLD.request_digest, OLD.request_payload, OLD.kind)
        IS DISTINCT FROM ROW(NEW.owner_id, NEW.principal_key, NEW.request_id, NEW.request_digest, NEW.request_payload, NEW.kind) THEN
        RAISE EXCEPTION 'task operation request is immutable';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER task_operation_request_immutable BEFORE UPDATE ON task_operations
FOR EACH ROW EXECUTE FUNCTION task_operation_request_immutable();

-- +goose StatementBegin
CREATE FUNCTION reject_task_artifact_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'task artifacts are immutable';
END $$;
-- +goose StatementEnd
CREATE TRIGGER task_artifact_immutable BEFORE UPDATE OR DELETE ON task_artifacts
FOR EACH ROW EXECUTE FUNCTION reject_task_artifact_mutation();

-- A report is a claim that was made: it is not edited or removed.
-- +goose StatementBegin
CREATE FUNCTION reject_task_report_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'task reports are immutable';
END $$;
-- +goose StatementEnd
CREATE TRIGGER task_report_immutable BEFORE UPDATE OR DELETE ON task_reports
FOR EACH ROW EXECUTE FUNCTION reject_task_report_mutation();

-- +goose StatementBegin
CREATE FUNCTION writer_generation_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'writer claim tombstones must be retained';
    END IF;
    IF NEW.generation < OLD.generation THEN
        RAISE EXCEPTION 'writer generation cannot decrease';
    END IF;
    IF NEW.held AND (NOT OLD.held OR ROW(NEW.attempt_id, NEW.binding_id) IS DISTINCT FROM ROW(OLD.attempt_id, OLD.binding_id)) THEN
        IF NEW.generation <> OLD.generation + 1 THEN
            RAISE EXCEPTION 'writer transfer must increment generation';
        END IF;
    ELSIF NEW.generation <> OLD.generation THEN
        RAISE EXCEPTION 'unexpected writer generation change';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER writer_generation_guard BEFORE UPDATE OR DELETE ON workspace_writer_claims
FOR EACH ROW EXECUTE FUNCTION writer_generation_guard();

-- +goose Down
DROP TABLE IF EXISTS workspace_writer_claims;
DROP TABLE IF EXISTS task_event_deliveries;
DROP TABLE IF EXISTS task_reports;
DROP TABLE IF EXISTS task_artifacts;
DROP TABLE IF EXISTS task_operations;
ALTER TABLE IF EXISTS tasks DROP CONSTRAINT IF EXISTS tasks_current_attempt;
DROP TABLE IF EXISTS task_attempts;
DROP TABLE IF EXISTS tasks;
DROP FUNCTION IF EXISTS writer_generation_guard();
DROP FUNCTION IF EXISTS reject_task_report_mutation();
DROP FUNCTION IF EXISTS reject_task_artifact_mutation();
DROP FUNCTION IF EXISTS task_operation_request_immutable();
DROP FUNCTION IF EXISTS task_attempt_routing_immutable();
DROP FUNCTION IF EXISTS validate_attempt_hierarchy();
DROP FUNCTION IF EXISTS validate_task_hierarchy();
DROP FUNCTION IF EXISTS task_identity_immutable();
