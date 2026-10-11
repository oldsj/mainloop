-- +goose Up
-- Scheduled runs (G14). kagent owns schedules and executions; Mainloop keeps only what it needs to
-- turn an execution into a Mainloop session and an inbox item, exactly once.
--
-- scheduled_run_schedules is the registry the adoption sweep walks: the schedules Mainloop created
-- or found in kagent's list. request_id is the create's idempotency key (NULL for a schedule the
-- sweep found); adopt_from is kagent's created_at for the schedule, so executions that predate it
-- are never adopted. cursor_execution_id is the newest execution id up to which every execution
-- has been fully handled; the sweep reads kagent's executions newest first and stops there.
-- A deleted schedule is read one last time (its executions stay readable) and then settled.
CREATE TABLE scheduled_run_schedules (
    schedule_id text PRIMARY KEY,
    owner_id text NOT NULL,
    agent_namespace text NOT NULL,
    agent_name text NOT NULL,
    request_id text,
    adopt_from timestamptz NOT NULL,
    cursor_execution_id text NOT NULL DEFAULT '',
    deleted_at timestamptz,
    settled_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (settled_at IS NULL OR deleted_at IS NOT NULL)
);
CREATE UNIQUE INDEX scheduled_run_schedules_request ON scheduled_run_schedules (owner_id, request_id)
WHERE request_id IS NOT NULL;
CREATE INDEX scheduled_run_schedules_open ON scheduled_run_schedules (owner_id) WHERE settled_at IS NULL;

-- One row per execution Mainloop has taken up: the idempotency record of the adoption. The id of
-- the session it made is derived from the execution id, so a replay after a crash finds the same
-- session. session_id is NULL for an execution that ended without ever reserving a kagent session
-- (it still needs its inbox item). attention_opened_at is set in the transaction that opens the item.
CREATE TABLE scheduled_run_links (
    execution_id text PRIMARY KEY,
    schedule_id text NOT NULL REFERENCES scheduled_run_schedules (schedule_id) ON DELETE CASCADE,
    kagent_session_id text,
    session_id text REFERENCES sessions (id) ON DELETE SET NULL,
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
DROP TABLE IF EXISTS scheduled_run_schedules;
