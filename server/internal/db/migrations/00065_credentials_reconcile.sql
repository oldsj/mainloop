-- +goose Up
-- Credential cleanup is driven by the reconcile runner: a revoked credential owes a Secret delete,
-- and the loop claims it like any other intent row. 'revoked' is the one claimable state; a
-- 'planned' row that never got published is revoked by a sweep first, so its Secret (if the create
-- landed) is looked up by name and deleted too.

ALTER TABLE credentials
ADD COLUMN attempts integer NOT NULL DEFAULT 0,
ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT now(),
ADD COLUMN lease_owner text,
ADD COLUMN lease_until timestamptz;

DROP INDEX credentials_cleanup;
CREATE INDEX credentials_claim ON credentials (next_attempt_at, id) WHERE state IN ('revoked');
-- A planned row that was never published is found by age.
CREATE INDEX credentials_stale_planned ON credentials (updated_at) WHERE state = 'planned';

-- +goose Down
DROP INDEX IF EXISTS credentials_stale_planned;
DROP INDEX IF EXISTS credentials_claim;
CREATE INDEX credentials_cleanup ON credentials (updated_at) WHERE state = 'revoked';
ALTER TABLE credentials
DROP COLUMN IF EXISTS lease_until,
DROP COLUMN IF EXISTS lease_owner,
DROP COLUMN IF EXISTS next_attempt_at,
DROP COLUMN IF EXISTS attempts;
