-- +goose Up
-- The event outbox. A domain change inserts its event row in the same transaction (see
-- internal/events), so a client never sees an event for a change that rolled back, and a change
-- never commits without its event. Ids are commit-ordered: the writer takes an advisory
-- transaction lock before it inserts, so a reader that has seen id N has seen every earlier id.

CREATE TABLE events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id text NOT NULL,
    -- One of the persisted SSE event names; internal/events checks it. `connected` and
    -- `heartbeat` are per-stream frames and never stored.
    type text NOT NULL,
    payload jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
-- Replay reads one user's events after an id.
CREATE INDEX events_user_id ON events (user_id, id);
CREATE INDEX events_created_at ON events (created_at);

-- The newest id the retention sweep has deleted. A client that resumes from an id at or below it
-- has missed events the table can no longer replay.
CREATE TABLE event_floor (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    pruned_through bigint NOT NULL DEFAULT 0
);
INSERT INTO event_floor DEFAULT VALUES;
