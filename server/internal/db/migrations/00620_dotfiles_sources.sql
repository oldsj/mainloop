-- +goose Up
-- Agent dotfiles sources (docs/specs/agent-dotfiles.md): the GitHub repository, ref and directory
-- whose AGENTS.md, skills, subagents and commands agent sessions receive as native user-level
-- files. One owner-wide source, and an optional per-project one that replaces it for that
-- project. A NULL repository is "none set": for the owner, no dotfiles; for a project, the
-- owner-wide source applies. Writes are a compare-and-swap on version, as provider preferences.
CREATE TABLE owner_dotfiles_sources (
    owner_id text PRIMARY KEY,
    -- GitHub "owner/name" as the owner wrote it; GitHub compares names case-insensitively.
    repository text CHECK (repository ~ '^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$'),
    -- A branch, tag or full commit SHA; empty is the repository's default branch.
    ref text NOT NULL DEFAULT '' CHECK (length(ref) <= 255),
    -- A relative directory with no "." or ".." segments; empty is the repository root.
    path text NOT NULL DEFAULT '' CHECK (length(path) <= 255),
    version bigint NOT NULL CHECK (version > 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE project_dotfiles_sources (
    project_id text PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
    repository text CHECK (repository ~ '^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$'),
    ref text NOT NULL DEFAULT '' CHECK (length(ref) <= 255),
    path text NOT NULL DEFAULT '' CHECK (length(path) <= 255),
    version bigint NOT NULL CHECK (version > 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS project_dotfiles_sources;
DROP TABLE IF EXISTS owner_dotfiles_sources;
