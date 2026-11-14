-- +goose Up
-- Where a project's heavy checks run (docs/specs/ci.md). verification_mode is the owner's choice;
-- 'auto' follows ci_detection, a cached read of the CI that reported on the default branch's tip.
-- ci_detection is JSON so later reads (workflow side effects, Actions minutes) can add fields
-- without a migration.
ALTER TABLE projects
    ADD COLUMN verification_mode text NOT NULL DEFAULT 'auto'
        CHECK (verification_mode IN ('auto', 'ci', 'hybrid', 'local')),
    ADD COLUMN ci_detection jsonb,
    ADD COLUMN ci_detected_at timestamptz;

-- +goose Down
ALTER TABLE projects
    DROP COLUMN ci_detected_at,
    DROP COLUMN ci_detection,
    DROP COLUMN verification_mode;
