-- +goose Up
-- Session transcripts (docs/specs/transcripts.md, #448): each finished turn of a kagent Session,
-- copied from its A2A task into Mainloop so it outlives the kagent Session and can be searched.
-- Only redacted, bounded text is ever written here: the ingester redacts before the insert, so the
-- search vector, which is generated from the stored text, never indexes a redacted credential.
--
-- States:
--   transcript_sessions.state
--     active       ingestion follows the kagent Session
--     complete     the kagent Session is gone and every turn it had was copied
--     unavailable  the kagent Session became unreadable before every turn was copied; reason says why

-- The ingester's per-kagent-Session cursor, one row per kagent Session a Mainloop session was bound
-- to (a restart or provider switch binds a new one).
CREATE TABLE transcript_sessions (
    kagent_session_id text PRIMARY KEY,
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    owner_id text NOT NULL,
    agent_namespace text NOT NULL,
    agent_name text NOT NULL,
    context_id text NOT NULL,
    -- The page token of the task listing page that holds the first task not yet copied; NULL starts
    -- from the first page. It moves past a page only once every task on it was copied.
    task_cursor text,
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'complete', 'unavailable')),
    reason text NOT NULL DEFAULT '',
    -- Bytes of entry text stored for this kagent Session, against the per-session budget.
    stored_bytes bigint NOT NULL DEFAULT 0 CHECK (stored_bytes >= 0),
    failures integer NOT NULL DEFAULT 0,
    -- When the session is next due; a finished turn sets it to now().
    next_scan_at timestamptz NOT NULL DEFAULT now(),
    scanned_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((state = 'unavailable') = (reason <> ''))
);
CREATE INDEX transcript_sessions_due ON transcript_sessions (next_scan_at) WHERE state = 'active';
CREATE INDEX transcript_sessions_session ON transcript_sessions (session_id);

-- One row per A2A task copied: the receipt that makes a copy happen once. Retention removes a task's
-- entries and keeps this row (pruned_at), so a task kagent still lists is not copied again.
CREATE TABLE transcript_tasks (
    task_id text PRIMARY KEY,
    kagent_session_id text NOT NULL REFERENCES transcript_sessions (kagent_session_id) ON DELETE CASCADE,
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    -- The A2A task state when it was copied: always a final or waiting one.
    task_state text NOT NULL,
    entries integer NOT NULL CHECK (entries >= 0),
    bytes bigint NOT NULL CHECK (bytes >= 0),
    -- When kagent recorded the task's last status; NULL when it did not say.
    task_at timestamptz,
    ingested_at timestamptz NOT NULL DEFAULT now(),
    pruned_at timestamptz
);
CREATE INDEX transcript_tasks_session ON transcript_tasks (session_id, ingested_at);
CREATE INDEX transcript_tasks_retention ON transcript_tasks (ingested_at) WHERE pruned_at IS NULL;

-- One message, text run or tool call or result of a task, in the task's timeline order (pos).
CREATE TABLE transcript_entries (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id text NOT NULL REFERENCES transcript_tasks (task_id) ON DELETE CASCADE,
    session_id text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    pos integer NOT NULL CHECK (pos >= 0),
    kind text NOT NULL CHECK (kind IN ('user', 'assistant', 'thinking', 'tool_call', 'tool_result', 'status', 'ask', 'other')),
    tool_name text NOT NULL DEFAULT '',
    tool_call_id text NOT NULL DEFAULT '',
    is_error boolean NOT NULL DEFAULT false,
    -- Redacted and bounded. Empty for a stub (the session's budget was spent) or withheld output.
    text text NOT NULL DEFAULT '',
    truncated boolean NOT NULL DEFAULT false,
    -- The size of the original text before redaction and cutting.
    orig_bytes integer NOT NULL DEFAULT 0 CHECK (orig_bytes >= 0),
    -- The entry's timeline position as kagent recorded it; NULL when it did not.
    at timestamptz,
    tsv tsvector GENERATED ALWAYS AS (to_tsvector('english', tool_name || ' ' || text)) STORED,
    UNIQUE (task_id, pos)
);
CREATE INDEX transcript_entries_session ON transcript_entries (session_id, id);
CREATE INDEX transcript_entries_tsv ON transcript_entries USING gin (tsv);

-- +goose Down
DROP TABLE IF EXISTS transcript_entries;
DROP TABLE IF EXISTS transcript_tasks;
DROP TABLE IF EXISTS transcript_sessions;
