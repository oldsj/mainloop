-- +goose Up
-- Project memory (docs/specs/project-context.md): short entries agents record for later agents on
-- a project. The main session's entries are active at once; a task's or workspace agent's are
-- proposals only the owner accepts. Active entries go into every new task brief after the
-- instructions.
--
-- Each change to a project's active set takes the next memory sequence number, under the project
-- row lock: an entry is in the set at sequence S when activated_seq <= S and it had not ended by
-- S. The project's current sequence is the highest number any entry carries, so a brief pinned to
-- a sequence renders the same entries for ever.
CREATE TABLE project_memory_entries (
    id text PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    text text NOT NULL CHECK (octet_length(text) BETWEEN 1 AND 500),
    state text NOT NULL CHECK (state IN ('proposed', 'active', 'rejected', 'withdrawn', 'forgotten', 'replaced')),
    author text NOT NULL CHECK (author IN ('main', 'task', 'agent')),
    -- Where the entry came from, resolved by the server from the recording session. Kept as
    -- plain ids, so the record outlives a pruned session or task.
    source_session_id text,
    source_task_id text,
    source_attempt_id text,
    source_delivery_id text,
    -- The active entry this one replaces when it becomes active.
    replaces_id text REFERENCES project_memory_entries (id),
    activated_seq bigint CHECK (activated_seq > 0),
    ended_seq bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    decided_at timestamptz,
    CHECK ((state = 'active') = (activated_seq IS NOT NULL AND ended_seq IS NULL)),
    CHECK (ended_seq IS NULL OR (activated_seq IS NOT NULL AND ended_seq > activated_seq)),
    CHECK ((state IN ('forgotten', 'replaced')) = (ended_seq IS NOT NULL))
);
CREATE INDEX project_memory_entries_open ON project_memory_entries (project_id, created_at, id)
WHERE state IN ('active', 'proposed');
CREATE INDEX project_memory_entries_seq ON project_memory_entries (project_id, activated_seq)
WHERE activated_seq IS NOT NULL;

-- The memory sequence an attempt's brief carries, pinned with instructions_version before its
-- first brief is sent. NULL: not pinned yet, or the task has no project. 0: no memory then.
ALTER TABLE task_attempts ADD COLUMN memory_seq bigint
    CHECK (memory_seq IS NULL OR memory_seq >= 0);

-- +goose Down
ALTER TABLE task_attempts DROP COLUMN IF EXISTS memory_seq;
DROP TABLE IF EXISTS project_memory_entries;
