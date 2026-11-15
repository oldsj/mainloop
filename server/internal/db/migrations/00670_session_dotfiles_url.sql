-- +goose Up
-- The url a native Session's create sent its dotfiles bundle with, so a retried or replayed create
-- sends the same request. NULL when the create sent none: no ready bundle, no MCP credential to
-- authorize the download, or a pin recorded before delivery existed.
ALTER TABLE session_dotfiles ADD COLUMN url text;
ALTER TABLE session_dotfiles ADD CONSTRAINT session_dotfiles_url_ready CHECK (url IS NULL OR status = 'ready');

-- +goose Down
ALTER TABLE session_dotfiles DROP COLUMN IF EXISTS url;
