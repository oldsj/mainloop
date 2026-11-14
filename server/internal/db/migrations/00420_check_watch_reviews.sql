-- +goose Up
-- A watch can also wake its agent on review feedback (docs/specs/ci.md#review-feedback): with
-- reviews set, the watch settles as 'review' as soon as a trusted reviewer says something on the
-- pull request after reviews_since. reviews_until is the newest feedback the settled message
-- carried; the next review watch of the same session and pull request starts there, so nothing is
-- delivered twice and nothing said while the agent worked is missed.
ALTER TABLE check_watches
    ADD COLUMN reviews boolean NOT NULL DEFAULT false,
    ADD COLUMN reviews_since timestamptz,
    ADD COLUMN reviews_until timestamptz,
    ADD CONSTRAINT check_watches_reviews_pr CHECK (NOT reviews OR (pr_number IS NOT NULL AND reviews_since IS NOT NULL)),
    DROP CONSTRAINT check_watches_outcome_check,
    ADD CONSTRAINT check_watches_outcome_check CHECK (
        outcome IN ('green', 'failed', 'pending_timeout', 'no_checks', 'not_found', 'review')
    );
CREATE INDEX check_watches_review_cursor ON check_watches (session_id, pr_number)
WHERE reviews_until IS NOT NULL;

-- +goose Down
DROP INDEX check_watches_review_cursor;
DELETE FROM check_watches WHERE outcome = 'review';
ALTER TABLE check_watches
    DROP CONSTRAINT check_watches_outcome_check,
    ADD CONSTRAINT check_watches_outcome_check CHECK (
        outcome IN ('green', 'failed', 'pending_timeout', 'no_checks', 'not_found')
    ),
    DROP CONSTRAINT check_watches_reviews_pr,
    DROP COLUMN reviews_until,
    DROP COLUMN reviews_since,
    DROP COLUMN reviews;
