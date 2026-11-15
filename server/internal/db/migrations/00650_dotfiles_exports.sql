-- +goose Up
-- Agent dotfiles exports (docs/specs/agent-dotfiles.md): what Mainloop read from a source's
-- directory at one commit, server-side. A ready export names its bundle, the deterministic tar of
-- the allowed files that sessions are given; a refused one names why (and, for a secret, which
-- files and rules matched, never the text). A commit's directory always exports the same way, so
-- both outcomes are kept; an unavailable read is not stored.
CREATE TABLE dotfiles_bundles (
    -- "sha256:<hex>" of content.
    digest text PRIMARY KEY CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    content bytea NOT NULL CHECK (octet_length(content) <= 32 * 1024 * 1024),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE dotfiles_exports (
    -- "owner/name", lower-cased: GitHub names are case-insensitive.
    repository text NOT NULL,
    commit text NOT NULL CHECK (commit ~ '^[0-9a-f]{40}$'),
    path text NOT NULL,
    status text NOT NULL CHECK (status IN ('ready', 'refused')),
    reason text NOT NULL DEFAULT '',
    digest text REFERENCES dotfiles_bundles (digest),
    -- [{path, rule}] for a secret_found refusal.
    findings jsonb NOT NULL DEFAULT '[]',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repository, commit, path),
    CHECK ((status = 'ready') = (digest IS NOT NULL)),
    CHECK ((status = 'refused') = (reason <> ''))
);

-- +goose Down
DROP TABLE IF EXISTS dotfiles_exports;
DROP TABLE IF EXISTS dotfiles_bundles;
