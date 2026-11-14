-- +goose Up
-- The owner's standing instructions for a project's agents (docs/specs/project-context.md). Every
-- change is a new row; the highest version is current, and no row means none were ever set
-- (version 0). Rows are never updated, so a brief rendered from a version can be read back.
CREATE TABLE project_instruction_versions (
    project_id text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    version integer NOT NULL CHECK (version > 0),
    text text NOT NULL CHECK (octet_length(text) <= 8192),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, version)
);

-- The instructions version an attempt's brief carries, pinned before its first brief is sent so a
-- resend renders the same text. NULL: not pinned yet, or the task has no project. 0: none were set.
ALTER TABLE task_attempts ADD COLUMN instructions_version integer
    CHECK (instructions_version IS NULL OR instructions_version >= 0);

-- +goose Down
ALTER TABLE task_attempts DROP COLUMN IF EXISTS instructions_version;
DROP TABLE IF EXISTS project_instruction_versions;
