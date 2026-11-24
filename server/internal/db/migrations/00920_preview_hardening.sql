-- +goose Up
-- Every preview_screenshot attempt, whether it stored an image or failed, so the per-minute limit
-- counts captures that failed or hung, not only stored rows (issue #249 follow-up). One row per
-- call that passed the limit, with the number of viewports it may capture.
CREATE TABLE preview_screenshot_attempts (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    captures integer NOT NULL CHECK (captures BETWEEN 1 AND 2),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX preview_screenshot_attempts_session_created
    ON preview_screenshot_attempts (session_id, created_at);

-- Retention: a screenshot's image expires (after 7 days, or once 50 newer ones of its workspace
-- hold images) and its row stays, so a report still says what was shown.
ALTER TABLE preview_screenshots ALTER COLUMN data DROP NOT NULL;
ALTER TABLE preview_screenshots ADD COLUMN expired_at timestamptz;
ALTER TABLE preview_screenshots ADD CONSTRAINT preview_screenshots_expired_has_no_data
    CHECK ((data IS NULL) = (expired_at IS NOT NULL));
-- The retention sweep finds the images still held.
CREATE INDEX preview_screenshots_held ON preview_screenshots (created_at) WHERE expired_at IS NULL;

-- A preview grant is good for one exchange: each exchanged grant's nonce is kept until the grant
-- would have expired, and a second exchange of it is refused.
CREATE TABLE preview_grant_uses (
    nonce text PRIMARY KEY CHECK (char_length(nonce) BETWEEN 1 AND 64),
    expires_at timestamptz NOT NULL
);
CREATE INDEX preview_grant_uses_expires ON preview_grant_uses (expires_at);

-- +goose Down
DROP TABLE preview_grant_uses;
DROP INDEX preview_screenshots_held;
ALTER TABLE preview_screenshots DROP CONSTRAINT preview_screenshots_expired_has_no_data;
DELETE FROM preview_screenshots WHERE data IS NULL;
ALTER TABLE preview_screenshots DROP COLUMN expired_at;
ALTER TABLE preview_screenshots ALTER COLUMN data SET NOT NULL;
DROP TABLE preview_screenshot_attempts;
