-- +goose Up
-- Stalled-turn detection (issue #265). The delivery loop records when it last saw a live event of a
-- delivered turn's task (activity_at) and what that event left the turn doing (activity_kind):
-- event (thinking or writing), tool (a tool call started with no result yet, which may run long
-- without a word) or parked (waiting for the owner, which is not silence). The writes are throttled
-- and never touch updated_at, which the uncertain hold and idle suspend read. A sweep sets
-- stalled_at once a delivered turn has been quiet longer than its allowance; the next event clears
-- it. Stalled is an observation, not an outcome: it never changes state, never replays a prompt and
-- does not bump outcome_version.
ALTER TABLE deliveries
    ADD COLUMN activity_at timestamptz,
    ADD COLUMN activity_kind text CHECK (activity_kind IN ('event', 'tool', 'parked')),
    ADD COLUMN stalled_at timestamptz;

CREATE INDEX deliveries_delivered_quiet ON deliveries (coalesce(activity_at, updated_at))
WHERE state = 'delivered' AND stalled_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS deliveries_delivered_quiet;
ALTER TABLE deliveries
    DROP COLUMN stalled_at,
    DROP COLUMN activity_kind,
    DROP COLUMN activity_at;
