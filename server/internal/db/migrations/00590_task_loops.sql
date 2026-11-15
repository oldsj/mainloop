-- +goose Up
-- Loops (docs/specs/loops.md): a durable wake-up that main or a supervisor arms instead of polling.
-- A loop belongs to its recipient, the session that armed it, named by the same key report
-- deliveries use ('main' or 'parent-task:<task id>'), so a supervisor's loops follow its task across
-- a retry or reassign. Each firing is one message, delivered through task_event_deliveries in the
-- same batch as child reports.
CREATE TABLE task_loops (
    id text PRIMARY KEY,
    owner_id text NOT NULL,
    -- The arming principal's key and request id: the same request returns the same loop.
    principal_key text NOT NULL,
    request_id text NOT NULL CHECK (length(request_id) BETWEEN 1 AND 100),
    -- Written by the server from the principal, never taken from a request.
    recipient_key text NOT NULL CHECK (recipient_key = 'main' OR recipient_key LIKE 'parent-task:%'),
    -- The task a task trigger watches. A time trigger may name one too, for its messages.
    subject_task_id text,
    trigger text NOT NULL CHECK (trigger IN ('task', 'time')),
    -- A task trigger fires only when the subject reaches one of these statuses; empty is any change.
    statuses text[] NOT NULL DEFAULT '{}',
    -- A time trigger's repeat; NULL fires once.
    every_seconds integer CHECK (every_seconds IS NULL OR every_seconds > 0),
    -- What the recipient asked to be told when the loop fires.
    note text NOT NULL CHECK (length(note) BETWEEN 1 AND 2000),
    max_firings integer NOT NULL CHECK (max_firings >= 1),
    min_interval_seconds integer NOT NULL CHECK (min_interval_seconds >= 0),
    expires_at timestamptz NOT NULL,
    state text NOT NULL DEFAULT 'armed' CHECK (
        state IN ('armed', 'done', 'exhausted', 'expired', 'stopped', 'ended')
    ),
    -- Firings made: each is one message. Occurrences folded into a pending firing do not count.
    firings integer NOT NULL DEFAULT 0,
    -- The subject's status and reason when the loop last looked: a task trigger fires on a change.
    baseline text,
    -- A time trigger's next occurrence.
    fire_at timestamptz,
    -- When the latest firing may be sent: a firing waits min_interval_seconds after the one before.
    last_fired_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- Reconcile intent columns: see internal/reconcile.
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz,
    UNIQUE (principal_key, request_id),
    FOREIGN KEY (owner_id, subject_task_id) REFERENCES tasks (owner_id, id),
    CHECK (trigger <> 'task' OR subject_task_id IS NOT NULL),
    CHECK (trigger <> 'time' OR fire_at IS NOT NULL OR state <> 'armed')
);
CREATE INDEX task_loops_claim ON task_loops (next_attempt_at, id) WHERE state = 'armed';
CREATE INDEX task_loops_recipient ON task_loops (recipient_key) WHERE state = 'armed';
CREATE INDEX task_loops_subject ON task_loops (subject_task_id) WHERE state = 'armed';

-- One firing is one message. Occurrences while it waits (its recipient's open turn, the batch
-- window, the minimum interval) fold into it rather than queue more messages.
CREATE TABLE task_loop_firings (
    id text PRIMARY KEY,
    loop_id text NOT NULL REFERENCES task_loops (id),
    number integer NOT NULL CHECK (number >= 1),
    -- What happened, oldest first: at most 10, the last always the latest.
    facts text[] NOT NULL CHECK (cardinality(facts) BETWEEN 1 AND 10),
    -- Occurrences folded in beyond the facts kept.
    folded integer NOT NULL DEFAULT 0,
    not_before timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (loop_id, number)
);

-- A delivery carries a report or a loop firing.
ALTER TABLE task_event_deliveries
    DROP CONSTRAINT task_event_deliveries_pkey,
    ALTER COLUMN report_id DROP NOT NULL,
    ADD COLUMN loop_firing_id text UNIQUE REFERENCES task_loop_firings (id),
    ADD PRIMARY KEY (id),
    ADD CONSTRAINT task_event_deliveries_report_recipient UNIQUE (report_id, recipient_key),
    ADD CONSTRAINT task_event_deliveries_one_item CHECK (num_nonnulls(report_id, loop_firing_id) = 1);

-- A task trigger looks again as soon as its subject's status or reason changes. The update waits
-- on a loop row an evaluation holds, so an evaluation either sees the change or is followed by
-- another one.
-- +goose StatementBegin
CREATE FUNCTION task_loops_nudge() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE task_loops SET next_attempt_at = now()
    WHERE subject_task_id = NEW.id AND state = 'armed' AND trigger = 'task';
    IF FOUND THEN
        PERFORM pg_notify('mainloop_task_loops', NEW.id);
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE TRIGGER tasks_loops_nudge AFTER UPDATE OF status, reason ON tasks
FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status OR OLD.reason IS DISTINCT FROM NEW.reason)
EXECUTE FUNCTION task_loops_nudge();

-- +goose Down
DROP TRIGGER tasks_loops_nudge ON tasks;
DROP FUNCTION task_loops_nudge();
DELETE FROM task_event_deliveries WHERE loop_firing_id IS NOT NULL;
ALTER TABLE task_event_deliveries
    DROP CONSTRAINT task_event_deliveries_one_item,
    DROP CONSTRAINT task_event_deliveries_report_recipient,
    DROP CONSTRAINT task_event_deliveries_pkey,
    DROP COLUMN loop_firing_id,
    ALTER COLUMN report_id SET NOT NULL,
    ADD PRIMARY KEY (report_id, recipient_key);
DROP TABLE task_loop_firings;
DROP TABLE task_loops;
