"""Health attribution outlives workspace rows; project deletion removes it."""

HEALTH_MIGRATION_SQL = """
CREATE TABLE IF NOT EXISTS orchestration_health_events (
 owner_id TEXT NOT NULL, gateway TEXT NOT NULL, event_id TEXT NOT NULL,
 producer_epoch TEXT NOT NULL, sequence BIGINT NOT NULL CHECK(sequence > 0),
 session_id TEXT NOT NULL, runtime_session_id TEXT NOT NULL, a2a_task_id TEXT NOT NULL,
 project_id TEXT REFERENCES projects(id) ON DELETE CASCADE,
 task_id TEXT, attempt_id TEXT, profile_revision TEXT,
 turn_key TEXT NOT NULL, occurred_at TIMESTAMPTZ NOT NULL,
 received_at TIMESTAMPTZ NOT NULL DEFAULT now(), payload JSONB NOT NULL,
 PRIMARY KEY(owner_id,gateway,event_id),
 UNIQUE(owner_id,gateway,producer_epoch,sequence)
);
CREATE INDEX IF NOT EXISTS health_event_turn ON orchestration_health_events
 (owner_id,gateway,session_id,a2a_task_id,turn_key,sequence);
CREATE TABLE IF NOT EXISTS orchestration_health_turns (
 owner_id TEXT NOT NULL, gateway TEXT NOT NULL, session_id TEXT NOT NULL,
 runtime_session_id TEXT NOT NULL, a2a_task_id TEXT NOT NULL, turn_key TEXT NOT NULL,
 project_id TEXT REFERENCES projects(id) ON DELETE CASCADE,
 task_id TEXT, attempt_id TEXT, profile_revision TEXT,
 provider TEXT NOT NULL, occurred_at TIMESTAMPTZ NOT NULL,
 summary JSONB NOT NULL, aggregation_version INTEGER NOT NULL DEFAULT 1,
 frozen BOOLEAN NOT NULL DEFAULT FALSE,
 PRIMARY KEY(owner_id,gateway,session_id,a2a_task_id,turn_key)
);
CREATE INDEX IF NOT EXISTS health_turn_owner_time ON orchestration_health_turns(owner_id,occurred_at);
CREATE TABLE IF NOT EXISTS orchestration_health_collection (
 owner_id TEXT NOT NULL, gateway TEXT NOT NULL, session_id TEXT NOT NULL,
 a2a_task_id TEXT NOT NULL, project_id TEXT REFERENCES projects(id) ON DELETE CASCADE,
 reasons TEXT[] NOT NULL DEFAULT '{}', updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(owner_id,gateway,session_id,a2a_task_id)
);
"""
