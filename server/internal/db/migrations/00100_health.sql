-- +goose Up
-- Orchestration health (G13): the observations a native agent turn leaves in its kagent task, the
-- per-turn rollups built from them, and the state of collecting them. The Python backend kept the
-- same three things (events, turns, collection gaps) and collected them on a page read. Here the
-- collector is a reconciler sweep and the routes only read.
--
-- Design choices:
--
--   * Attribution is stamped on the row when it is written (project, task, attempt, profile
--     revision) and never re-derived, so the history stays attributed after a task is replaced or
--     a session is deleted. There are no foreign keys to sessions, tasks or attempts for that
--     reason. Only the project is a foreign key: a deleted project takes its health with it.
--   * An event is stored once. Its id is "<producer epoch>:<sequence>", and the same id with the
--     same payload is a no-op while the same id with a different payload is an identity conflict.
--     The event rows are the source of truth; a turn summary is a pure function of them, so a
--     summary can always be rebuilt (aggregation_version records which rules built it).
--   * Collection is durable intent, not a queue. A terminal delivery that carries a kagent task
--     gets one health_collection row (pending). The recovery sweep reads the task from kagent with
--     no transaction open, then writes the events and marks the row collected in one transaction.
--     A failure leaves the row pending with a growing delay and records source_unavailable; a row
--     that keeps failing becomes unavailable, which stays visible as a gap. Nothing in memory is
--     lost by a restart.
--   * Gaps are a property of a receipt (reasons), so the page can say what it could not see
--     instead of showing a clean total over missing data.
--   * Retention freezes a turn once any of its events expires: the summary then stays as it is and
--     the turn takes no more events, so deleting the old events cannot change a total. Events are
--     deleted only after their own window; summaries and collection rows after the longer one.
--
-- State machine (the health package owns the transitions):
--   health_collection.state
--     pending      the receipt is known and has not been read from kagent yet (or the read failed)
--     collected    the task was read and its events stored (final)
--     unavailable  kagent could not give the task after the attempts allowed (final)

CREATE TABLE health_events (
    owner_id text NOT NULL,
    gateway text NOT NULL,
    event_id text NOT NULL,
    producer_epoch text NOT NULL,
    sequence bigint NOT NULL CHECK (sequence BETWEEN 1 AND 1000000),
    session_id text NOT NULL,
    runtime_session_id text NOT NULL,
    a2a_task_id text NOT NULL,
    project_id text REFERENCES projects (id) ON DELETE CASCADE,
    task_id text,
    attempt_id text,
    profile_revision text,
    turn_key text NOT NULL,
    occurred_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    payload jsonb NOT NULL,
    PRIMARY KEY (owner_id, gateway, event_id),
    UNIQUE (owner_id, gateway, producer_epoch, sequence)
);
CREATE INDEX health_events_turn ON health_events (owner_id, gateway, session_id, a2a_task_id, turn_key, sequence);
CREATE INDEX health_events_session ON health_events (owner_id, session_id, occurred_at, sequence);
CREATE INDEX health_events_occurred ON health_events (occurred_at);

CREATE TABLE health_turns (
    owner_id text NOT NULL,
    gateway text NOT NULL,
    session_id text NOT NULL,
    a2a_task_id text NOT NULL,
    turn_key text NOT NULL,
    runtime_session_id text NOT NULL,
    project_id text REFERENCES projects (id) ON DELETE CASCADE,
    task_id text,
    attempt_id text,
    profile_revision text,
    provider text NOT NULL,
    -- When the turn's latest event happened; the page's time window is on this.
    occurred_at timestamptz NOT NULL,
    summary jsonb NOT NULL,
    aggregation_version integer NOT NULL DEFAULT 1,
    frozen boolean NOT NULL DEFAULT false,
    PRIMARY KEY (owner_id, gateway, session_id, a2a_task_id, turn_key)
);
CREATE INDEX health_turns_window ON health_turns (owner_id, occurred_at DESC);
CREATE INDEX health_turns_session ON health_turns (owner_id, session_id, occurred_at DESC);

CREATE TABLE health_collection (
    owner_id text NOT NULL,
    gateway text NOT NULL,
    session_id text NOT NULL,
    a2a_task_id text NOT NULL,
    -- The delivery that produced the task, and when it was created: the time basis of the receipt.
    delivery_id text NOT NULL,
    receipt_at timestamptz NOT NULL,
    project_id text REFERENCES projects (id) ON DELETE CASCADE,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'collected', 'unavailable')),
    -- Why the receipt is not whole: overflow, invalid_event, identity_mismatch, identity_conflict,
    -- invalid_timestamp, source_unavailable.
    reasons text[] NOT NULL DEFAULT '{}',
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    collected_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_id, gateway, session_id, a2a_task_id)
);
-- The recovery sweep's work: only the receipts still to read.
CREATE INDEX health_collection_due ON health_collection (next_attempt_at, receipt_at)
WHERE state = 'pending';
CREATE INDEX health_collection_window ON health_collection (owner_id, receipt_at);

-- +goose Down
DROP TABLE IF EXISTS health_collection;
DROP TABLE IF EXISTS health_turns;
DROP TABLE IF EXISTS health_events;
