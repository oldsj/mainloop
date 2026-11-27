-- +goose Up
-- The export format joins the key of a stored export (docs/specs/agent-dotfiles.md). A commit's
-- directory exports the same way only under one format: when what an export keeps changes (format
-- 2 keeps claude/settings.json), an export stored under an older format must be read again, not
-- reused. Rows stored so far are format 1. Bundles are untouched: pins and running Sessions keep
-- downloading the digests they hold.
ALTER TABLE dotfiles_exports
    ADD COLUMN format integer NOT NULL DEFAULT 1 CHECK (format >= 1),
    DROP CONSTRAINT dotfiles_exports_pkey,
    ADD PRIMARY KEY (repository, commit, path, format);

ALTER TABLE dotfiles_exports ALTER COLUMN format DROP DEFAULT;

-- +goose Down
-- Keeps the format 1 rows; newer ones are read again by the older code.
DELETE FROM dotfiles_exports WHERE format <> 1;

ALTER TABLE dotfiles_exports
    DROP CONSTRAINT dotfiles_exports_pkey,
    DROP COLUMN format,
    ADD PRIMARY KEY (repository, commit, path);
