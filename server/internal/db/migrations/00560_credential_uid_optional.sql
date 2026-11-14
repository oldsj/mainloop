-- +goose Up
-- A credential can be published without its Secret's UID: Mainloop no longer reads Secrets back,
-- so a create whose reply was lost, found again as a taken name, records the name alone
-- (docs/specs/credentials.md). Cleanup deletes such a Secret by name.
ALTER TABLE credentials DROP CONSTRAINT credentials_check;

-- +goose Down
-- Fails while a published credential has no UID; revoke those first.
ALTER TABLE credentials ADD CONSTRAINT credentials_check CHECK (state <> 'published' OR secret_uid IS NOT NULL);
