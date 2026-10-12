-- +goose Up
-- Scheduled runs (G14). kagent owns schedules and executions; Mainloop keeps only what it needs to
-- turn an execution into a Mainloop session and an inbox item, exactly once.
--
-- scheduled_run_schedules is both the create intent and the registry the adoption sweep walks. One
-- kagent principal serves every Mainloop instance, so a schedule is ours only if this table says
-- so: a row is written in state 'intent' (request id, Agent, the config sent) before the create is
-- sent, and moves to 'created' with the schedule id once kagent answered. A create whose reply was
-- lost is finished by replaying it from the row (kagent dedupes on the request id), and only on an
-- Agent this deployment still owns. Nothing else ever enters the registry, so another instance's schedules are never listed, controlled or adopted.
--
-- cursor_execution_id is the newest execution id up to which every execution has been fully handled
-- (and was old enough to be settled); the sweep reads kagent's executions newest first and stops
-- there. A deleted schedule is read one last time (its executions stay readable) and then settled.
CREATE TABLE scheduled_run_schedules (
    request_id text PRIMARY KEY,
    owner_id text NOT NULL,
    agent_namespace text NOT NULL,
    agent_name text NOT NULL,
    -- protojson of the ScheduledRunConfig the create sent, for the replay.
    create_config jsonb NOT NULL,
    state text NOT NULL DEFAULT 'intent' CHECK (state IN ('intent', 'created')),
    schedule_id text UNIQUE,
    cursor_execution_id text NOT NULL DEFAULT '',
    deleted_at timestamptz,
    settled_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((state = 'created') = (schedule_id IS NOT NULL)),
    CHECK (settled_at IS NULL OR deleted_at IS NOT NULL)
);
CREATE INDEX scheduled_run_schedules_open ON scheduled_run_schedules (owner_id) WHERE settled_at IS NULL;

-- A create attempt (a route call, a client retry or a recovery replay) holds a lease on its intent
-- until expires_at. The attempt's call to kagent runs under a deadline derived from the lease, so no
-- attempt outlives it. While any lease is unexpired another attempt may still make the schedule, so
-- an attempt's refusal proves nothing; only the sweep's recovery, when no other lease is live, takes
-- a refusal as final and deletes the intent. Expiry is compared with the database clock.
CREATE TABLE scheduled_run_create_attempts (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id text NOT NULL REFERENCES scheduled_run_schedules (request_id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL
);
CREATE INDEX scheduled_run_create_attempts_request ON scheduled_run_create_attempts (request_id, expires_at);

-- One row per execution Mainloop has taken up: the idempotency record of the adoption. The id of
-- the session it made is derived from the execution id, so a replay after a crash finds the same
-- session. session_id is NULL for an execution that has not reserved a kagent session yet (an
-- execution that ended without one still needs its inbox item). It is not a foreign key: the id
-- stays as history after the session is deleted, and a set id is never made again.
-- attention_opened_at is set in the transaction that opens the item.
CREATE TABLE scheduled_run_links (
    execution_id text PRIMARY KEY,
    schedule_id text NOT NULL REFERENCES scheduled_run_schedules (schedule_id) ON DELETE CASCADE,
    kagent_session_id text,
    session_id text,
    state text NOT NULL,
    attention_opened_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX scheduled_run_links_kagent_session ON scheduled_run_links (kagent_session_id)
WHERE kagent_session_id IS NOT NULL;
CREATE INDEX scheduled_run_links_schedule ON scheduled_run_links (schedule_id, execution_id DESC);

-- +goose Down
DROP TABLE IF EXISTS scheduled_run_links;
DROP TABLE IF EXISTS scheduled_run_create_attempts;
DROP TABLE IF EXISTS scheduled_run_schedules;
