-- +goose Up
-- Owner memory (docs/specs/owner-memory.md): project memory without a project. A few lines of the
-- owner's own standing preferences, loaded into every session the owner has. The main session's
-- entries are active at once; a supervisor's are proposals the owner decides. Active entries are
-- aged on purpose: one not confirmed within the owner's review window is due for review, and one
-- still not kept 7 days later lapses and stops being loaded.
--
-- The owner's row holds the review window and the owner's memory sequence. Every writer locks it
-- (SELECT ... FOR UPDATE), so the sequence and the active byte total move one writer at a time.
CREATE TABLE owner_memory_settings (
    user_id text PRIMARY KEY,
    review_after_days integer NOT NULL DEFAULT 14 CHECK (review_after_days BETWEEN 1 AND 365),
    -- The highest sequence number any change to the active set took; 0 before the first.
    seq bigint NOT NULL DEFAULT 0 CHECK (seq >= 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE owner_memory_entries (
    id text PRIMARY KEY,
    user_id text NOT NULL,
    text text NOT NULL CHECK (octet_length(text) BETWEEN 1 AND 500),
    state text NOT NULL CHECK (state IN (
        'proposed', 'active', 'lapsed', 'rejected', 'withdrawn', 'replaced', 'forgotten', 'moved')),
    author text NOT NULL CHECK (author IN ('main', 'task')),
    -- Where the entry came from, resolved by the server from the recording session. Plain ids, so
    -- the record outlives a pruned session or task.
    source_session_id text,
    source_task_id text,
    source_attempt_id text,
    source_delivery_id text,
    -- The active entry this one replaces when it becomes active.
    replaces_id text REFERENCES owner_memory_entries (id),
    -- Set when the entry becomes active and moved only by the owner's confirmation.
    last_confirmed_at timestamptz,
    lapsed_at timestamptz,
    -- The dotfiles commit whose AGENTS.md holds this text (folded, case aside), or NULL.
    in_dotfiles_commit text,
    -- The project memory entry a move to a project made.
    moved_to_project_id text,
    moved_to_entry_id text,
    created_at timestamptz NOT NULL DEFAULT now(),
    decided_at timestamptz,
    -- Only an entry that was never active has no confirmation. Spelled with OR, not IN, so these
    -- are not enumeration checks (see TestStateConstantsMatchChecks).
    CHECK ((last_confirmed_at IS NULL) = (state = 'proposed' OR state = 'rejected' OR state = 'withdrawn')),
    CHECK (state <> 'lapsed' OR lapsed_at IS NOT NULL),
    CHECK ((moved_to_project_id IS NULL) = (moved_to_entry_id IS NULL))
);
CREATE INDEX owner_memory_entries_open ON owner_memory_entries (user_id, created_at, id)
WHERE state IN ('active', 'lapsed', 'proposed');

-- When each entry was in the active set. An entry is in the set at sequence S when one of its
-- spans has start_seq <= S and had not ended by S, so a brief pinned to S renders the same entries
-- (with the confirmation date each had then) for ever. An entry can have several spans: a lapsed
-- entry the owner keeps starts a new one, and so does every confirmation of an active entry, since
-- the date it renders changes.
CREATE TABLE owner_memory_spans (
    entry_id text NOT NULL REFERENCES owner_memory_entries (id) ON DELETE CASCADE,
    user_id text NOT NULL,
    start_seq bigint NOT NULL CHECK (start_seq > 0),
    end_seq bigint CHECK (end_seq IS NULL OR end_seq > start_seq),
    confirmed_at timestamptz NOT NULL,
    PRIMARY KEY (entry_id, start_seq)
);
CREATE UNIQUE INDEX owner_memory_spans_open ON owner_memory_spans (entry_id) WHERE end_seq IS NULL;
CREATE INDEX owner_memory_spans_seq ON owner_memory_spans (user_id, start_seq);

-- The owner memory sequence an attempt's brief carries, pinned before its first brief is sent.
-- NULL: not pinned yet. 0: no owner memory then.
ALTER TABLE task_attempts ADD COLUMN owner_memory_seq bigint
    CHECK (owner_memory_seq IS NULL OR owner_memory_seq >= 0);

-- What owner memory a delivery gave its session, when it gave any: the whole section or the
-- changes since what the session had.
ALTER TABLE deliveries ADD COLUMN given_owner_memory_seq bigint
    CHECK (given_owner_memory_seq IS NULL OR given_owner_memory_seq >= 0);

-- A project entry the owner moved from owner memory names the owner entry it came from.
ALTER TABLE project_memory_entries ADD COLUMN source_owner_entry_id text;

-- +goose Down
ALTER TABLE project_memory_entries DROP COLUMN IF EXISTS source_owner_entry_id;
ALTER TABLE deliveries DROP COLUMN IF EXISTS given_owner_memory_seq;
ALTER TABLE task_attempts DROP COLUMN IF EXISTS owner_memory_seq;
DROP TABLE IF EXISTS owner_memory_spans;
DROP TABLE IF EXISTS owner_memory_entries;
DROP TABLE IF EXISTS owner_memory_settings;
