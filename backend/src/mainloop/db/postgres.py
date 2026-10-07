"""PostgreSQL client for durable workflow persistence."""

import json
import uuid
from contextlib import asynccontextmanager
from datetime import datetime, timezone
from typing import Any

import asyncpg
from mainloop.config import settings
from mainloop.db.environment_schema import ENVIRONMENT_MIGRATION_SQL
from mainloop.db.hitl_schema import HITL_MIGRATION_SQL
from mainloop.db.merge_schema import MERGE_MIGRATION_SQL
from mainloop.services.github_repo import GithubRepo

from models import (
    Conversation,
    MainThread,
    Message,
    Project,
    QueueItem,
    QueueItemPriority,
    QueueItemType,
    Session,
    SessionNotification,
    SessionStatus,
)
from models.merge_policy import MergePolicyUpdate


class PRCreationConflict(Exception):
    """A request ID or repo/head/base intent already has a different payload."""


def _parse_json_field(value: Any) -> list | dict | None:
    """Parse a JSON field that might be a string or already parsed."""
    if value is None:
        return None
    if isinstance(value, (list, dict)):
        return value
    if isinstance(value, str):
        try:
            return json.loads(value)
        except json.JSONDecodeError:
            return None
    return None


# SQL schema for workflow tables
SCHEMA_SQL = """
-- Main threads (eternal per-user workflows)
CREATE TABLE IF NOT EXISTS main_threads (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    workflow_run_id TEXT,
    status TEXT NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_activity_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    active_tasks TEXT[] DEFAULT '{}',
    context JSONB DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_main_threads_user_id ON main_threads(user_id);

-- Queue items (human-in-the-loop / inbox)
CREATE TABLE IF NOT EXISTS queue_items (
    id TEXT PRIMARY KEY,
    main_thread_id TEXT NOT NULL REFERENCES main_threads(id),
    task_id TEXT,
    user_id TEXT NOT NULL,
    item_type TEXT NOT NULL,
    priority TEXT NOT NULL DEFAULT 'normal',
    title TEXT NOT NULL,
    content TEXT NOT NULL,
    context JSONB DEFAULT '{}',
    options TEXT[],
    status TEXT NOT NULL DEFAULT 'pending',
    response TEXT,
    responded_at TIMESTAMPTZ,
    read_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_queue_items_user_id ON queue_items(user_id);
CREATE INDEX IF NOT EXISTS idx_queue_items_status ON queue_items(status);
CREATE INDEX IF NOT EXISTS idx_queue_items_main_thread ON queue_items(main_thread_id);

-- Conversations
CREATE TABLE IF NOT EXISTS conversations (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    title TEXT,
    summary TEXT,
    summarized_through_id TEXT,
    message_count INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_conversations_user_id ON conversations(user_id);

-- Messages
CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversations(id),
    role TEXT NOT NULL,
    content TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id);

-- Projects
CREATE TABLE IF NOT EXISTS projects (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    owner TEXT NOT NULL,
    name TEXT NOT NULL,
    full_name TEXT NOT NULL,
    description TEXT,
    default_branch TEXT DEFAULT 'main',
    avatar_url TEXT,
    html_url TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    metadata_updated_at TIMESTAMPTZ,
    open_pr_count INTEGER DEFAULT 0,
    open_issue_count INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_projects_user_id ON projects(user_id);
CREATE INDEX IF NOT EXISTS idx_projects_last_used ON projects(last_used_at DESC);

-- Sessions (unified: both simple conversations and code work)
CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    main_thread_id TEXT NOT NULL REFERENCES main_threads(id),
    title TEXT NOT NULL,
    description TEXT NOT NULL,
    prompt TEXT NOT NULL,
    conversation_id TEXT NOT NULL REFERENCES conversations(id),
    status TEXT NOT NULL DEFAULT 'pending',
    worker_pod_name TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    summary TEXT,
    error TEXT,
    -- Code work fields (optional - only used when repo_url is set)
    repo_url TEXT,
    project_id TEXT REFERENCES projects(id),
    branch_name TEXT,
    base_branch TEXT DEFAULT 'main',
    model TEXT,
    -- GitHub integration - Plan phase (issue)
    issue_url TEXT,
    issue_number INTEGER,
    issue_etag TEXT,
    issue_last_modified TIMESTAMPTZ,
    -- GitHub integration - Implementation phase (PR)
    pr_url TEXT,
    pr_number INTEGER,
    pr_etag TEXT,
    pr_last_modified TIMESTAMPTZ,
    commit_sha TEXT,
    -- Inline thread anchoring
    anchor_message_id TEXT REFERENCES messages(id),
    color VARCHAR(20),
    result JSONB
);
CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_main_thread ON sessions(main_thread_id);
CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);
CREATE INDEX IF NOT EXISTS idx_sessions_repo_url ON sessions(repo_url);
CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project_id);
CREATE INDEX IF NOT EXISTS idx_sessions_anchor ON sessions(anchor_message_id);

-- Native agent bindings: one per session bound to a kagent Session (Claude or Codex Agent)
CREATE TABLE IF NOT EXISTS native_bindings (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id),
    kind TEXT NOT NULL,
    kagent_session_id TEXT,      -- kagent Session id, equal to the A2A contextId; NULL until created
    kagent_request_id TEXT,      -- CreateSession request id of a replacement Session; NULL = derived
    model TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    credential_cleanup_pending BOOLEAN NOT NULL DEFAULT FALSE,
    child_start_failure TEXT,
    kagent_deleted_at TIMESTAMPTZ,  -- set once kagent confirmed DeleteSession for an archived session
    queue_held BOOLEAN NOT NULL DEFAULT FALSE,  -- the owner stopped a turn: queued messages wait for their next message
    mcp_grant_kind TEXT NOT NULL DEFAULT 'none',
    credential_ref JSONB,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- Cleanup survives deletion of the workspace/session binding that owned a credential.
CREATE TABLE IF NOT EXISTS agent_credential_cleanup (
    session_id TEXT PRIMARY KEY,
    credential_ref JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- Delivery ledger: one row per message; the A2A task is the receipt
CREATE TABLE IF NOT EXISTS native_deliveries (
    message_id TEXT PRIMARY KEY REFERENCES messages(id),
    session_id TEXT NOT NULL REFERENCES sessions(id),
    state TEXT NOT NULL,
    task_id TEXT,
    evidence_ref TEXT,
    detail TEXT,
    partial_text TEXT,          -- last observed streamed reply; retained if cancellation omits artifacts
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_native_deliveries_session ON native_deliveries(session_id);

-- Context model (main thread window, session tree, topics). Additive to the r6 tables.
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'agent';
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS parent_session_id TEXT;
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS topic_id TEXT;
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS token_hash TEXT;
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS standing_hash TEXT;
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS turns INTEGER NOT NULL DEFAULT 0;
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS reported_at TIMESTAMPTZ;
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS kagent_session_id TEXT;
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS kagent_request_id TEXT;
ALTER TABLE native_deliveries ADD COLUMN IF NOT EXISTS task_id TEXT;
ALTER TABLE native_deliveries ADD COLUMN IF NOT EXISTS partial_text TEXT;
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS kagent_deleted_at TIMESTAMPTZ;
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS queue_held BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS mcp_grant_kind TEXT NOT NULL DEFAULT 'none';
ALTER TABLE native_bindings ADD COLUMN IF NOT EXISTS credential_ref JSONB;
CREATE TABLE IF NOT EXISTS agent_credential_cleanup (
    session_id TEXT PRIMARY KEY,
    credential_ref JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- Existing authenticated main/child bindings remain coordination grants. Ordinary agent
-- bindings (including retained workspace Sessions) are never enrolled by migration.
UPDATE native_bindings SET mcp_grant_kind='coordination'
 WHERE role IN ('main','child') AND token_hash IS NOT NULL AND mcp_grant_kind='none';
-- Freeze the already-supported reference for existing coordination bindings with a live hash.
UPDATE native_bindings SET credential_ref=jsonb_build_object(
    'origin','http://mainloop-mcp.mainloop.svc.cluster.local',
    'header','Authorization','secret_name','mainloop-agent-tokens','secret_key',session_id)
 WHERE (mcp_grant_kind='coordination' AND token_hash IS NOT NULL
        OR credential_cleanup_pending=TRUE) AND credential_ref IS NULL;
-- Carry forward any cleanup that was pending before cleanup records became deletion-safe.
INSERT INTO agent_credential_cleanup(session_id,credential_ref)
 SELECT session_id,credential_ref FROM native_bindings
 WHERE credential_cleanup_pending=TRUE AND credential_ref IS NOT NULL
 ON CONFLICT(session_id) DO NOTHING;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='native_bindings_mcp_grant_kind_check') THEN
        ALTER TABLE native_bindings ADD CONSTRAINT native_bindings_mcp_grant_kind_check
          CHECK (mcp_grant_kind IN ('none','coordination','workspace')) NOT VALID;
    END IF;
END $$;
ALTER TABLE native_bindings VALIDATE CONSTRAINT native_bindings_mcp_grant_kind_check;
-- The Substrate journal transport is gone: its cursors, lineage and events have no meaning on kagent.
ALTER TABLE native_bindings DROP COLUMN IF EXISTS agent_name;
ALTER TABLE native_bindings DROP COLUMN IF EXISTS native_session_id;
ALTER TABLE native_bindings DROP COLUMN IF EXISTS approval_policy;
ALTER TABLE native_bindings DROP COLUMN IF EXISTS generation;
ALTER TABLE native_bindings DROP COLUMN IF EXISTS journal_cursor;
ALTER TABLE native_bindings DROP COLUMN IF EXISTS journal_ref;
ALTER TABLE native_bindings DROP COLUMN IF EXISTS lineage_seq;
ALTER TABLE native_bindings DROP COLUMN IF EXISTS context_tokens;
ALTER TABLE native_bindings DROP COLUMN IF EXISTS baseline_tokens;
ALTER TABLE native_bindings DROP COLUMN IF EXISTS turns_in_lineage;
ALTER TABLE native_bindings DROP COLUMN IF EXISTS continuations;
-- A delivery still open at the cutover has no kagent task and its binding has no kagent Session
-- yet, so nothing could resolve it and it would block the session for good. Settle it as unknown
-- (never replayed). The legacy cursor column marks the one run that sees Substrate-era rows.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_name='native_deliveries' AND column_name='cursor_before') THEN
        UPDATE native_deliveries
           SET state='uncertain',
               detail='open at the kagent cutover, outcome unknown; not replayed',
               updated_at=NOW()
         WHERE state IN ('recorded','sending','delivered');
    END IF;
END $$;
ALTER TABLE native_deliveries DROP COLUMN IF EXISTS cursor_before;
ALTER TABLE native_deliveries DROP COLUMN IF EXISTS generation;
DROP TABLE IF EXISTS native_lineage;
DROP TABLE IF EXISTS native_events;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;
-- Cancelling used to record status failed + this error text (and agent sync could then revive
-- it). Cancelled is its own status now; correct the old rows. Idempotent.
UPDATE sessions SET status = 'cancelled', error = NULL
 WHERE error = 'Cancelled by user' AND status <> 'cancelled';
-- A child that has reported is done, not waiting on the user. New reports set this directly;
-- this corrects children that reported before that, which no sync would revisit. Idempotent.
UPDATE sessions SET status = 'completed'
 WHERE status = 'waiting_on_user'
   AND EXISTS (SELECT 1 FROM native_bindings b
               WHERE b.session_id = sessions.id AND b.role = 'child' AND b.reported_at IS NOT NULL);
ALTER TABLE native_deliveries ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'user';
CREATE UNIQUE INDEX IF NOT EXISTS idx_native_bindings_token ON native_bindings(token_hash) WHERE token_hash IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_native_bindings_parent ON native_bindings(parent_session_id);

-- Branch workspaces: the repository kagent clones into the harness when it creates the Session.
-- repo/ref/branch/depth are the CreateSession `workspace` and never change, so a replacement
-- Session resends them unchanged. last_active_at is the last preview request or user resume and
-- feeds the idle debounce; idle_suspended_at is when the idle check last suspended the Session
-- (the workspace is a candidate again only after newer activity). ports are the dev server ports
-- the preview URL may reach.
CREATE TABLE IF NOT EXISTS workspaces (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    repo TEXT NOT NULL,
    ref TEXT NOT NULL DEFAULT '',
    branch TEXT NOT NULL DEFAULT '',
    depth INTEGER NOT NULL DEFAULT 0,
    ports JSONB NOT NULL DEFAULT '[]'::jsonb,
    idle_timeout_minutes INTEGER NOT NULL DEFAULT 30,
    last_active_at TIMESTAMPTZ,
    idle_suspended_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS development_environment JSONB;
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS reported_development_environment JSONB;
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS runtime_composition JSONB;

-- Topics are durable records (not sessions). Supervisors (next slice) attach to a topic.
CREATE TABLE IF NOT EXISTS topics (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    name TEXT NOT NULL,
    status_line TEXT NOT NULL DEFAULT '',
    checkpoint TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, name)
);
-- Notes, decisions, pending intent and child reports for a topic (source-linked).
CREATE TABLE IF NOT EXISTS topic_records (
    id TEXT PRIMARY KEY,
    topic_id TEXT NOT NULL REFERENCES topics(id),
    kind TEXT NOT NULL,          -- note | decision | pending | report
    text TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'open',   -- pending: open | done
    session_id TEXT,             -- the session that wrote it
    evidence_ref TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_topic_records_topic ON topic_records(topic_id, created_at);

-- Session notifications (ephemeral)
CREATE TABLE IF NOT EXISTS session_notifications (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL,
    title TEXT NOT NULL,
    preview TEXT NOT NULL,
    read BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_session_notifications_user ON session_notifications(user_id);
CREATE INDEX IF NOT EXISTS idx_session_notifications_unread ON session_notifications(user_id, read) WHERE NOT read;
"""

# Migration SQL for adding new columns to existing tables
MIGRATION_SQL = """
-- Add queue_items and conversation migrations
DO $$
BEGIN
    -- Add read_at to queue_items
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='queue_items' AND column_name='read_at') THEN
        ALTER TABLE queue_items ADD COLUMN read_at TIMESTAMPTZ;
    END IF;
    -- Add compaction fields to conversations
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='conversations' AND column_name='summary') THEN
        ALTER TABLE conversations ADD COLUMN summary TEXT;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='conversations' AND column_name='summarized_through_id') THEN
        ALTER TABLE conversations ADD COLUMN summarized_through_id TEXT;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='conversations' AND column_name='message_count') THEN
        ALTER TABLE conversations ADD COLUMN message_count INTEGER NOT NULL DEFAULT 0;
    END IF;
    -- Drop deprecated claude_session_id if it exists
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='conversations' AND column_name='claude_session_id') THEN
        ALTER TABLE conversations DROP COLUMN claude_session_id;
    END IF;
END $$;

-- Create indexes if they don't exist
CREATE INDEX IF NOT EXISTS idx_queue_items_read_at ON queue_items(read_at);

-- One project per repository per user, whatever the letter case: GitHub names are
-- case-insensitive. get_or_create_project relies on this index for ON CONFLICT. Creating it
-- fails if a user already has two projects that differ only in case.
DROP INDEX IF EXISTS idx_projects_user_full_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_user_lower_full_name
    ON projects(user_id, lower(full_name));
-- The older exact-case UNIQUE is redundant, and it is not an ON CONFLICT arbiter: two
-- concurrent first inserts of one repository could still fail on it. Drop it only after
-- the lower-case index exists, so uniqueness is never absent.
ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_user_id_full_name_key;

-- Add new columns to sessions for unified model
DO $$
BEGIN
    -- Code work fields
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='sessions' AND column_name='repo_url') THEN
        ALTER TABLE sessions ADD COLUMN repo_url TEXT;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='sessions' AND column_name='project_id') THEN
        ALTER TABLE sessions ADD COLUMN project_id TEXT REFERENCES projects(id);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='sessions' AND column_name='branch_name') THEN
        ALTER TABLE sessions ADD COLUMN branch_name TEXT;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='sessions' AND column_name='base_branch') THEN
        ALTER TABLE sessions ADD COLUMN base_branch TEXT DEFAULT 'main';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='sessions' AND column_name='model') THEN
        ALTER TABLE sessions ADD COLUMN model TEXT;
    END IF;
    -- GitHub issue fields
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='sessions' AND column_name='issue_url') THEN
        ALTER TABLE sessions ADD COLUMN issue_url TEXT;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='sessions' AND column_name='issue_number') THEN
        ALTER TABLE sessions ADD COLUMN issue_number INTEGER;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='sessions' AND column_name='issue_etag') THEN
        ALTER TABLE sessions ADD COLUMN issue_etag TEXT;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='sessions' AND column_name='issue_last_modified') THEN
        ALTER TABLE sessions ADD COLUMN issue_last_modified TIMESTAMPTZ;
    END IF;
    -- GitHub PR fields
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='sessions' AND column_name='pr_url') THEN
        ALTER TABLE sessions ADD COLUMN pr_url TEXT;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='sessions' AND column_name='pr_number') THEN
        ALTER TABLE sessions ADD COLUMN pr_number INTEGER;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='sessions' AND column_name='pr_etag') THEN
        ALTER TABLE sessions ADD COLUMN pr_etag TEXT;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='sessions' AND column_name='pr_last_modified') THEN
        ALTER TABLE sessions ADD COLUMN pr_last_modified TIMESTAMPTZ;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='sessions' AND column_name='commit_sha') THEN
        ALTER TABLE sessions ADD COLUMN commit_sha TEXT;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='sessions' AND column_name='result') THEN
        ALTER TABLE sessions ADD COLUMN result JSONB;
    END IF;
    -- Inline thread anchoring fields
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='sessions' AND column_name='anchor_message_id') THEN
        ALTER TABLE sessions ADD COLUMN anchor_message_id TEXT REFERENCES messages(id);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='sessions' AND column_name='color') THEN
        ALTER TABLE sessions ADD COLUMN color VARCHAR(20);
    END IF;
END $$;

-- Create session indexes
CREATE INDEX IF NOT EXISTS idx_sessions_repo_url ON sessions(repo_url);
CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project_id);
CREATE INDEX IF NOT EXISTS idx_sessions_anchor ON sessions(anchor_message_id);

-- PR creation intent survives process/transport failure. Only the inserting caller may POST;
-- duplicates reconcile by listing, never by taking over an expired lease.
CREATE TABLE IF NOT EXISTS pr_creations (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    project_id TEXT NOT NULL REFERENCES projects(id),
    repo_id BIGINT NOT NULL,
    head TEXT NOT NULL,
    base TEXT NOT NULL,
    expected_sha TEXT NOT NULL,
    payload_hash TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'uncertain' CHECK (state IN ('uncertain', 'created')),
    result JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, repo_id, head, base)
);
CREATE TABLE IF NOT EXISTS pr_creation_requests (
    user_id TEXT NOT NULL,
    request_id TEXT NOT NULL,
    payload_hash TEXT NOT NULL,
    creation_id TEXT NOT NULL REFERENCES pr_creations(id),
    PRIMARY KEY (user_id, request_id)
);
"""


MIGRATION_SQL += HITL_MIGRATION_SQL
MIGRATION_SQL += MERGE_MIGRATION_SQL
MIGRATION_SQL += ENVIRONMENT_MIGRATION_SQL


class Database:
    """PostgreSQL database client for workflow persistence."""

    def __init__(self):
        self._pool: asyncpg.Pool | None = None

    async def connect(self):
        """Create connection pool."""
        if not settings.database_url:
            return
        self._pool = await asyncpg.create_pool(
            settings.database_url,
            min_size=2,
            max_size=10,
        )

    async def disconnect(self):
        """Close connection pool."""
        if self._pool:
            await self._pool.close()
            self._pool = None

    @asynccontextmanager
    async def connection(self):
        """Get a connection from the pool."""
        if not self._pool:
            raise RuntimeError("Database not connected")
        async with self._pool.acquire() as conn:
            yield conn

    async def ensure_tables_exist(self):
        """Create tables if they don't exist and run migrations."""
        if not self._pool:
            return
        async with self.connection() as conn:
            await conn.execute(SCHEMA_SQL)
            await conn.execute(MIGRATION_SQL)

    async def pr_project_authority(
        self, binding: dict, project_id: str, branch: str | None = None
    ) -> dict | None:
        """Resolve PR authority through the shared role/grant/workspace scope check."""
        from mainloop.services.workspace_authority import (
            ScopeUnavailable,
            resolve_project_authority,
        )

        async with self.connection() as conn:
            try:
                resolved = await resolve_project_authority(
                    conn, binding, project_id, branch=branch
                )
            except ScopeUnavailable:
                return None
        return resolved[0] if resolved else None

    async def claim_pr_creation(
        self,
        *,
        user_id: str,
        project_id: str,
        request_id: str,
        payload_hash: str,
        repo_id: int,
        head: str,
        base: str,
        expected_sha: str,
    ) -> tuple[dict, bool]:
        """Serialize request IDs and repo/head/base. A conflict rolls back both inserts."""
        async with self.connection() as conn:
            async with conn.transaction():
                await conn.execute(
                    "SELECT pg_advisory_xact_lock(hashtext($1))",
                    f"pr-request:{user_id}:{request_id}",
                )
                request = await conn.fetchrow(
                    "SELECT * FROM pr_creation_requests WHERE user_id=$1 AND request_id=$2",
                    user_id,
                    request_id,
                )
                if request:
                    if request["payload_hash"] != payload_hash:
                        raise PRCreationConflict
                    row = await conn.fetchrow(
                        "SELECT * FROM pr_creations WHERE id=$1", request["creation_id"]
                    )
                    if row["repo_id"] != repo_id:
                        raise PRCreationConflict
                    return dict(row), False
                creation_id = str(uuid.uuid4())
                inserted = await conn.fetchrow(
                    """INSERT INTO pr_creations
                       (id,user_id,project_id,repo_id,head,base,expected_sha,payload_hash)
                       VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
                       ON CONFLICT (user_id,repo_id,head,base) DO NOTHING RETURNING *""",
                    creation_id,
                    user_id,
                    project_id,
                    repo_id,
                    head,
                    base,
                    expected_sha,
                    payload_hash,
                )
                row = inserted or await conn.fetchrow(
                    """SELECT * FROM pr_creations
                       WHERE user_id=$1 AND repo_id=$2 AND head=$3 AND base=$4""",
                    user_id,
                    repo_id,
                    head,
                    base,
                )
                if row["payload_hash"] != payload_hash:
                    raise PRCreationConflict
                await conn.execute(
                    "INSERT INTO pr_creation_requests VALUES ($1,$2,$3,$4)",
                    user_id,
                    request_id,
                    payload_hash,
                    row["id"],
                )
                return dict(row), inserted is not None

    async def get_pr_creation_request(
        self, user_id: str, request_id: str, payload_hash: str
    ) -> dict | None:
        async with self.connection() as conn:
            row = await conn.fetchrow(
                """SELECT c.*, r.payload_hash AS request_hash
                   FROM pr_creation_requests r JOIN pr_creations c ON c.id=r.creation_id
                   WHERE r.user_id=$1 AND r.request_id=$2""",
                user_id,
                request_id,
            )
        if row and row["request_hash"] != payload_hash:
            raise PRCreationConflict
        return dict(row) if row else None

    async def finish_pr_creation(self, creation_id: str, result: dict) -> None:
        async with self.connection() as conn:
            await conn.execute(
                """UPDATE pr_creations SET state='created', result=$2::jsonb
                   WHERE id=$1 AND state='uncertain'""",
                creation_id,
                json.dumps(result),
            )

    # ============= Main Thread Operations =============

    async def create_main_thread(self, thread: MainThread) -> MainThread:
        """Create a new main thread."""
        if not self._pool:
            return thread
        async with self.connection() as conn:
            await conn.execute(
                """
                INSERT INTO main_threads (id, user_id, workflow_run_id, status, created_at, last_activity_at, active_tasks, context)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
                """,
                thread.id,
                thread.user_id,
                thread.workflow_run_id,
                thread.status,
                thread.created_at,
                thread.last_activity_at,
                thread.active_tasks,
                json.dumps(thread.context) if thread.context else "{}",
            )
        return thread

    async def get_main_thread(self, thread_id: str) -> MainThread | None:
        """Get a main thread by ID."""
        if not self._pool:
            return None
        async with self.connection() as conn:
            row = await conn.fetchrow(
                "SELECT * FROM main_threads WHERE id = $1", thread_id
            )
        if not row:
            return None
        return self._row_to_main_thread(row)

    async def get_main_thread_by_user(self, user_id: str) -> MainThread | None:
        """Get the main thread for a user."""
        if not self._pool:
            return None
        async with self.connection() as conn:
            row = await conn.fetchrow(
                "SELECT * FROM main_threads WHERE user_id = $1 LIMIT 1", user_id
            )
        if not row:
            return None
        return self._row_to_main_thread(row)

    async def update_main_thread(
        self,
        thread_id: str,
        workflow_run_id: str | None = None,
        status: str | None = None,
        context: dict | None = None,
    ):
        """Update main thread fields."""
        if not self._pool:
            return
        updates = []
        params = []
        param_idx = 1

        if workflow_run_id is not None:
            updates.append(f"workflow_run_id = ${param_idx}")
            params.append(workflow_run_id)
            param_idx += 1
        if status is not None:
            updates.append(f"status = ${param_idx}")
            params.append(status)
            param_idx += 1
        if context is not None:
            updates.append(f"context = ${param_idx}")
            params.append(context)
            param_idx += 1

        updates.append(f"last_activity_at = ${param_idx}")
        params.append(datetime.now(timezone.utc))
        param_idx += 1

        params.append(thread_id)

        if updates:
            async with self.connection() as conn:
                await conn.execute(
                    f"UPDATE main_threads SET {', '.join(updates)} WHERE id = ${param_idx}",
                    *params,
                )

    async def add_recent_repo(self, thread_id: str, repo_url: str, max_repos: int = 5):
        """Add a repo to the recent repos list in main thread context.

        Keeps the list at max_repos, removing oldest entries.
        """
        if not self._pool:
            return

        async with self.connection() as conn:
            # Get current context
            row = await conn.fetchrow(
                "SELECT context FROM main_threads WHERE id = $1", thread_id
            )
            if not row:
                return

            # Parse context - handle both dict and string
            raw_context = row["context"]
            if isinstance(raw_context, dict):
                context = raw_context
            elif raw_context:
                context = json.loads(raw_context)
            else:
                context = {}

            recent_repos = context.get("recent_repos", [])

            # Remove if already exists (to move to front)
            recent_repos = [r for r in recent_repos if r != repo_url]

            # Add to front
            recent_repos.insert(0, repo_url)

            # Trim to max
            recent_repos = recent_repos[:max_repos]

            context["recent_repos"] = recent_repos

            await conn.execute(
                "UPDATE main_threads SET context = $1, last_activity_at = $2 WHERE id = $3",
                json.dumps(context),
                datetime.now(timezone.utc),
                thread_id,
            )

    async def get_recent_repos(self, thread_id: str) -> list[str]:
        """Get recent repos from main thread context."""
        if not self._pool:
            return []

        async with self.connection() as conn:
            row = await conn.fetchrow(
                "SELECT context FROM main_threads WHERE id = $1", thread_id
            )
            if not row:
                return []

            # Parse context - handle both dict and string
            raw_context = row["context"]
            if isinstance(raw_context, dict):
                context = raw_context
            elif raw_context:
                context = json.loads(raw_context)
            else:
                context = {}

            return context.get("recent_repos", [])

    async def add_active_task(self, thread_id: str, task_id: str):
        """Add a task to the active tasks list."""
        if not self._pool:
            return
        async with self.connection() as conn:
            await conn.execute(
                """
                UPDATE main_threads
                SET active_tasks = array_append(active_tasks, $1),
                    last_activity_at = NOW()
                WHERE id = $2
                """,
                task_id,
                thread_id,
            )

    async def remove_active_task(self, thread_id: str, task_id: str):
        """Remove a task from the active tasks list."""
        if not self._pool:
            return
        async with self.connection() as conn:
            await conn.execute(
                """
                UPDATE main_threads
                SET active_tasks = array_remove(active_tasks, $1),
                    last_activity_at = NOW()
                WHERE id = $2
                """,
                task_id,
                thread_id,
            )

    def _row_to_main_thread(self, row: asyncpg.Record) -> MainThread:
        return MainThread(
            id=row["id"],
            user_id=row["user_id"],
            workflow_run_id=row["workflow_run_id"],
            status=row["status"],
            created_at=row["created_at"],
            last_activity_at=row["last_activity_at"],
            active_tasks=list(row["active_tasks"]) if row["active_tasks"] else [],
            context=(
                row["context"]
                if isinstance(row["context"], dict)
                else (json.loads(row["context"]) if row["context"] else {})
            ),
        )

    # ============= Project Operations =============

    def _row_to_project(self, row: asyncpg.Record) -> Project:
        return Project(
            id=row["id"],
            user_id=row["user_id"],
            owner=row["owner"],
            name=row["name"],
            full_name=row["full_name"],
            description=row.get("description"),
            default_branch=row.get("default_branch") or "",
            avatar_url=row.get("avatar_url"),
            html_url=row["html_url"],
            created_at=row["created_at"],
            last_used_at=row["last_used_at"],
            metadata_updated_at=row.get("metadata_updated_at"),
            merge_policy=row["merge_policy"],
            merge_policy_version=row["merge_policy_version"],
            open_pr_count=row.get("open_pr_count") or 0,
            open_issue_count=row.get("open_issue_count") or 0,
        )

    async def update_merge_policy(
        self, project_id: str, owner_id: str, update: MergePolicyUpdate
    ) -> Project | None:
        """Serialize owner changes on the project row; merge claims must use this lock too."""
        async with self.connection() as conn, conn.transaction():
            row = await conn.fetchrow(
                "SELECT * FROM projects WHERE id=$1 AND user_id=$2 FOR UPDATE",
                project_id,
                owner_id,
            )
            if row is None:
                return None
            if row["merge_policy_version"] != update.expected_version:
                raise ValueError("Merge policy version changed")
            if row["merge_policy"] == update.merge_policy:
                return self._row_to_project(row)
            changed = await conn.fetchrow(
                """UPDATE projects SET merge_policy=$2,merge_policy_version=merge_policy_version+1
                   WHERE id=$1 RETURNING *""",
                project_id,
                update.merge_policy.value,
            )
            await conn.execute(
                """INSERT INTO project_merge_policy_audit(project_id,owner_id,old_policy,new_policy,version)
                   VALUES($1,$2,$3,$4,$5)""",
                project_id,
                owner_id,
                row["merge_policy"],
                update.merge_policy.value,
                changed["merge_policy_version"],
            )
            return self._row_to_project(changed)

    async def get_project(self, project_id: str) -> Project | None:
        """Get a project by ID."""
        if not self._pool:
            return None
        async with self.connection() as conn:
            row = await conn.fetchrow(
                "SELECT * FROM projects WHERE id = $1", project_id
            )
        if not row:
            return None
        return self._row_to_project(row)

    async def get_project_by_repo(self, user_id: str, full_name: str) -> Project | None:
        """Get a project by GitHub full_name (owner/repo)."""
        if not self._pool:
            return None
        async with self.connection() as conn:
            row = await conn.fetchrow(
                "SELECT * FROM projects WHERE user_id = $1 AND lower(full_name) = lower($2)",
                user_id,
                full_name,
            )
        if not row:
            return None
        return self._row_to_project(row)

    async def list_projects(self, user_id: str, limit: int = 20) -> list[Project]:
        """List user's projects ordered by last_used_at."""
        if not self._pool:
            return []
        async with self.connection() as conn:
            rows = await conn.fetch(
                "SELECT * FROM projects WHERE user_id = $1 ORDER BY last_used_at DESC LIMIT $2",
                user_id,
                limit,
            )
        return [self._row_to_project(row) for row in rows]

    async def update_project_metadata(
        self,
        project_id: str,
        description: str | None = None,
        avatar_url: str | None = None,
        default_branch: str | None = None,
        open_pr_count: int | None = None,
        open_issue_count: int | None = None,
    ):
        """Update cached GitHub metadata for a project."""
        if not self._pool:
            return
        updates = []
        params = []
        param_idx = 1

        if description is not None:
            updates.append(f"description = ${param_idx}")
            params.append(description)
            param_idx += 1
        if avatar_url is not None:
            updates.append(f"avatar_url = ${param_idx}")
            params.append(avatar_url)
            param_idx += 1
        if default_branch:
            updates.append(f"default_branch = ${param_idx}")
            params.append(default_branch)
            param_idx += 1
        if open_pr_count is not None:
            updates.append(f"open_pr_count = ${param_idx}")
            params.append(open_pr_count)
            param_idx += 1
        if open_issue_count is not None:
            updates.append(f"open_issue_count = ${param_idx}")
            params.append(open_issue_count)
            param_idx += 1

        if updates:
            updates.append(f"metadata_updated_at = ${param_idx}")
            params.append(datetime.now(timezone.utc))
            param_idx += 1

            params.append(project_id)
            async with self.connection() as conn:
                await conn.execute(
                    f"UPDATE projects SET {', '.join(updates)} WHERE id = ${param_idx}",
                    *params,
                )

    async def touch_project(self, project_id: str):
        """Update last_used_at timestamp."""
        if not self._pool:
            return
        async with self.connection() as conn:
            await conn.execute(
                "UPDATE projects SET last_used_at = $1 WHERE id = $2",
                datetime.now(timezone.utc),
                project_id,
            )

    async def get_or_create_project(self, user_id: str, repo: GithubRepo) -> Project:
        """Find the user's project for a GitHub repository, creating it if absent.

        One ``INSERT ... ON CONFLICT`` on ``(user_id, lower(full_name))``, so concurrent callers get
        the same row and ``Foo/Bar`` and ``foo/bar`` are one project. An existing project keeps its stored URL and metadata and is touched. A
        new one stores the canonical URL and an empty ``default_branch``: nothing here asks
        GitHub, so the repository default is unknown until a refresh and clones use the remote's.
        """
        project = Project(
            user_id=user_id,
            owner=repo.owner,
            name=repo.name,
            full_name=repo.full_name,
            default_branch="",
            html_url=repo.html_url,
        )
        if not self._pool:
            return project
        async with self.connection() as conn:
            row = await conn.fetchrow(
                """
                INSERT INTO projects
                (id, user_id, owner, name, full_name, default_branch, html_url,
                 created_at, last_used_at)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
                ON CONFLICT (user_id, lower(full_name))
                DO UPDATE SET last_used_at = EXCLUDED.last_used_at
                RETURNING *
                """,
                project.id,
                project.user_id,
                project.owner,
                project.name,
                project.full_name,
                project.default_branch,
                project.html_url,
                datetime.now(timezone.utc),
            )
        return self._row_to_project(row)

    # ============= Queue Item Operations =============

    async def create_queue_item(self, item: QueueItem) -> QueueItem:
        """Create a new queue item."""
        if not self._pool:
            return item
        async with self.connection() as conn:
            await conn.execute(
                """
                INSERT INTO queue_items
                (id, main_thread_id, task_id, user_id, item_type, priority,
                 title, content, context, options, status, created_at, expires_at)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
                """,
                item.id,
                item.main_thread_id,
                item.task_id,
                item.user_id,
                item.item_type.value,
                item.priority.value,
                item.title,
                item.content,
                json.dumps(item.context) if item.context else "{}",
                item.options,
                item.status,
                item.created_at,
                item.expires_at,
            )
        return item

    async def get_queue_item(self, item_id: str) -> QueueItem | None:
        """Get a queue item by ID."""
        if not self._pool:
            return None
        async with self.connection() as conn:
            row = await conn.fetchrow(
                "SELECT * FROM queue_items WHERE id = $1", item_id
            )
        if not row:
            return None
        return self._row_to_queue_item(row)

    async def list_queue_items(
        self,
        user_id: str,
        status: str = "pending",
        limit: int = 50,
        unread_only: bool = False,
        task_id: str | None = None,
    ) -> list[QueueItem]:
        """List queue items for a user.

        Args:
            user_id: The user ID
            status: Filter by status (default: "pending")
            limit: Max items to return
            unread_only: Only return unread items
            task_id: Filter by task ID

        """
        if not self._pool:
            return []

        # Build query dynamically based on filters
        conditions = ["user_id = $1", "status = $2"]
        params: list[Any] = [user_id, status]
        param_idx = 3

        if unread_only:
            conditions.append("read_at IS NULL")

        if task_id:
            conditions.append(f"task_id = ${param_idx}")
            params.append(task_id)
            param_idx += 1

        params.append(limit)

        query = f"""
            SELECT * FROM queue_items
            WHERE {" AND ".join(conditions)}
            ORDER BY
                CASE priority
                    WHEN 'urgent' THEN 1
                    WHEN 'high' THEN 2
                    WHEN 'normal' THEN 3
                    ELSE 4
                END,
                created_at DESC
            LIMIT ${param_idx}
        """

        async with self.connection() as conn:
            rows = await conn.fetch(query, *params)
        return [self._row_to_queue_item(row) for row in rows]

    async def update_queue_item(
        self,
        item_id: str,
        status: str | None = None,
        response: str | None = None,
    ):
        """Update queue item fields."""
        if not self._pool:
            return
        async with self.connection() as conn:
            if await conn.fetchval(
                "SELECT 1 FROM queue_items WHERE id=$1 AND item_type='hitl_request'",
                item_id,
            ):
                raise ValueError(
                    "HITL cards can only be settled by their structured decision"
                )
        updates = []
        params = []
        param_idx = 1

        if status is not None:
            updates.append(f"status = ${param_idx}")
            params.append(status)
            param_idx += 1
        if response is not None:
            updates.append(f"response = ${param_idx}")
            params.append(response)
            param_idx += 1
            updates.append(f"responded_at = ${param_idx}")
            params.append(datetime.now(timezone.utc))
            param_idx += 1

        params.append(item_id)

        if updates:
            async with self.connection() as conn:
                await conn.execute(
                    f"UPDATE queue_items SET {', '.join(updates)} WHERE id = ${param_idx}",
                    *params,
                )

    async def count_unread_queue_items(self, user_id: str) -> int:
        """Count unread queue items for a user."""
        if not self._pool:
            return 0
        async with self.connection() as conn:
            row = await conn.fetchrow(
                """
                SELECT COUNT(*) as count FROM queue_items
                WHERE user_id = $1 AND read_at IS NULL AND status = 'pending'
                """,
                user_id,
            )
        return row["count"] if row else 0

    async def mark_queue_item_read(self, item_id: str) -> None:
        """Mark a queue item as read."""
        if not self._pool:
            return
        async with self.connection() as conn:
            await conn.execute(
                "UPDATE queue_items SET read_at = NOW() WHERE id = $1",
                item_id,
            )

    async def mark_all_queue_items_read(self, user_id: str) -> int:
        """Mark all pending queue items as read for a user."""
        if not self._pool:
            return 0
        async with self.connection() as conn:
            result = await conn.execute(
                """
                UPDATE queue_items SET read_at = NOW()
                WHERE user_id = $1 AND read_at IS NULL AND status = 'pending'
                """,
                user_id,
            )
        # Parse the result string "UPDATE N" to get count
        try:
            return int(result.split()[-1])
        except (ValueError, IndexError):
            return 0

    def _row_to_queue_item(self, row: asyncpg.Record) -> QueueItem:
        return QueueItem(
            id=row["id"],
            main_thread_id=row["main_thread_id"],
            task_id=row["task_id"],
            user_id=row["user_id"],
            item_type=QueueItemType(row["item_type"]),
            priority=QueueItemPriority(row["priority"]),
            title=row["title"],
            content=row["content"],
            context=(
                row["context"]
                if isinstance(row["context"], dict)
                else (json.loads(row["context"]) if row["context"] else {})
            ),
            options=list(row["options"]) if row["options"] else None,
            status=row["status"],
            response=row["response"],
            hitl_request_id=row.get("hitl_request_id"),
            responded_at=row["responded_at"],
            read_at=row.get("read_at"),
            created_at=row["created_at"],
            expires_at=row["expires_at"],
        )

    # ============= Conversation Operations =============

    async def create_conversation(
        self, user_id: str, title: str | None = None
    ) -> Conversation:
        """Create a new conversation."""
        import uuid

        conversation = Conversation(
            id=str(uuid.uuid4()),
            user_id=user_id,
            title=title or "New Conversation",
            message_count=0,
            created_at=datetime.now(timezone.utc),
            updated_at=datetime.now(timezone.utc),
        )
        if not self._pool:
            return conversation
        async with self.connection() as conn:
            await conn.execute(
                """
                INSERT INTO conversations (id, user_id, title, message_count, created_at, updated_at)
                VALUES ($1, $2, $3, $4, $5, $6)
                """,
                conversation.id,
                conversation.user_id,
                conversation.title,
                conversation.message_count,
                conversation.created_at,
                conversation.updated_at,
            )
        return conversation

    async def get_conversation(self, conversation_id: str) -> Conversation | None:
        """Get a conversation by ID."""
        if not self._pool:
            return None
        async with self.connection() as conn:
            row = await conn.fetchrow(
                "SELECT * FROM conversations WHERE id = $1", conversation_id
            )
        if not row:
            return None
        return self._row_to_conversation(row)

    async def list_conversations(
        self, user_id: str, limit: int = 50
    ) -> list[Conversation]:
        """List main thread conversations for a user (excludes session conversations)."""
        if not self._pool:
            return []
        async with self.connection() as conn:
            rows = await conn.fetch(
                """
                SELECT c.* FROM conversations c
                WHERE c.user_id = $1
                AND NOT EXISTS (
                    SELECT 1 FROM sessions s WHERE s.conversation_id = c.id
                    AND NOT EXISTS (SELECT 1 FROM native_bindings b WHERE b.session_id = s.id AND b.role = 'main')
                )
                ORDER BY c.updated_at DESC
                LIMIT $2
                """,
                user_id,
                limit,
            )
        return [self._row_to_conversation(row) for row in rows]

    async def update_conversation_summary(
        self,
        conversation_id: str,
        summary: str,
        summarized_through_id: str,
    ) -> None:
        """Update the compaction summary for a conversation."""
        if not self._pool:
            return
        async with self.connection() as conn:
            await conn.execute(
                """
                UPDATE conversations
                SET summary = $1, summarized_through_id = $2, updated_at = $3
                WHERE id = $4
                """,
                summary,
                summarized_through_id,
                datetime.now(timezone.utc),
                conversation_id,
            )

    async def increment_message_count(self, conversation_id: str) -> int:
        """Increment message count and return new value."""
        if not self._pool:
            return 0
        async with self.connection() as conn:
            row = await conn.fetchrow(
                """
                UPDATE conversations
                SET message_count = message_count + 1, updated_at = $1
                WHERE id = $2
                RETURNING message_count
                """,
                datetime.now(timezone.utc),
                conversation_id,
            )
        return row["message_count"] if row else 0

    def _row_to_conversation(self, row: asyncpg.Record) -> Conversation:
        return Conversation(
            id=row["id"],
            user_id=row["user_id"],
            title=row["title"],
            summary=row.get("summary"),
            summarized_through_id=row.get("summarized_through_id"),
            message_count=row.get("message_count", 0),
            created_at=row["created_at"],
            updated_at=row["updated_at"],
        )

    async def create_message(
        self, conversation_id: str, role: str, content: str, *, conn: Any | None = None
    ) -> Message:
        """Create a new message, optionally inside a caller-owned transaction."""
        import uuid

        message = Message(
            id=str(uuid.uuid4()),
            conversation_id=conversation_id,
            role=role,  # type: ignore
            content=content,
            created_at=datetime.now(timezone.utc),
        )
        if not self._pool and conn is None:
            return message

        async def insert(connection) -> None:
            await connection.execute(
                """
                INSERT INTO messages (id, conversation_id, role, content, created_at)
                VALUES ($1, $2, $3, $4, $5)
                """,
                message.id,
                message.conversation_id,
                message.role,
                message.content,
                message.created_at,
            )
            # Update conversation's updated_at
            await connection.execute(
                "UPDATE conversations SET updated_at = $1 WHERE id = $2",
                datetime.now(timezone.utc),
                conversation_id,
            )

        if conn is not None:
            await insert(conn)
        else:
            async with self.connection() as connection:
                await insert(connection)
        return message

    async def get_messages(self, conversation_id: str) -> list[Message]:
        """Get all messages for a conversation."""
        if not self._pool:
            return []
        async with self.connection() as conn:
            rows = await conn.fetch(
                """
                SELECT * FROM messages
                WHERE conversation_id = $1
                ORDER BY created_at ASC
                """,
                conversation_id,
            )
        return [
            Message(
                id=row["id"],
                conversation_id=row["conversation_id"],
                role=row["role"],  # type: ignore
                content=row["content"],
                created_at=row["created_at"],
            )
            for row in rows
        ]

    async def get_message(self, message_id: str) -> Message | None:
        """Get a single message by ID."""
        if not self._pool:
            return None
        async with self.connection() as conn:
            row = await conn.fetchrow(
                "SELECT * FROM messages WHERE id = $1",
                message_id,
            )
        if not row:
            return None
        return Message(
            id=row["id"],
            conversation_id=row["conversation_id"],
            role=row["role"],  # type: ignore
            content=row["content"],
            created_at=row["created_at"],
        )

    async def list_messages(
        self, conversation_id: str, limit: int = 20
    ) -> list[Message]:
        """Get recent messages for a conversation (for context window)."""
        if not self._pool:
            return []
        async with self.connection() as conn:
            rows = await conn.fetch(
                """
                SELECT * FROM messages
                WHERE conversation_id = $1
                ORDER BY created_at DESC
                LIMIT $2
                """,
                conversation_id,
                limit,
            )
        # Reverse to get chronological order
        rows = list(reversed(rows))
        return [self._row_to_message(row) for row in rows]

    async def get_messages_after(
        self, conversation_id: str, after_message_id: str | None, limit: int = 20
    ) -> list[Message]:
        """Get messages after a specific message ID (for unsummarized messages)."""
        if not self._pool:
            return []
        async with self.connection() as conn:
            if after_message_id:
                # Get the timestamp of the after_message_id
                ref_row = await conn.fetchrow(
                    "SELECT created_at FROM messages WHERE id = $1",
                    after_message_id,
                )
                if ref_row:
                    rows = await conn.fetch(
                        """
                        SELECT * FROM messages
                        WHERE conversation_id = $1 AND created_at > $2
                        ORDER BY created_at DESC
                        LIMIT $3
                        """,
                        conversation_id,
                        ref_row["created_at"],
                        limit,
                    )
                else:
                    # Reference message not found, get recent
                    rows = await conn.fetch(
                        """
                        SELECT * FROM messages
                        WHERE conversation_id = $1
                        ORDER BY created_at DESC
                        LIMIT $2
                        """,
                        conversation_id,
                        limit,
                    )
            else:
                # No reference, get recent messages
                rows = await conn.fetch(
                    """
                    SELECT * FROM messages
                    WHERE conversation_id = $1
                    ORDER BY created_at DESC
                    LIMIT $2
                    """,
                    conversation_id,
                    limit,
                )
        # Reverse to get chronological order
        rows = list(reversed(rows))
        return [self._row_to_message(row) for row in rows]

    async def get_messages_for_compaction(
        self, conversation_id: str, up_to_count: int
    ) -> list[Message]:
        """Get oldest messages for compaction (up to a count)."""
        if not self._pool:
            return []
        async with self.connection() as conn:
            rows = await conn.fetch(
                """
                SELECT * FROM messages
                WHERE conversation_id = $1
                ORDER BY created_at ASC
                LIMIT $2
                """,
                conversation_id,
                up_to_count,
            )
        return [self._row_to_message(row) for row in rows]

    def _row_to_message(self, row: asyncpg.Record) -> Message:
        return Message(
            id=row["id"],
            conversation_id=row["conversation_id"],
            role=row["role"],  # type: ignore
            content=row["content"],
            created_at=row["created_at"],
        )

    # ============= Session Operations =============

    async def create_session(
        self, session: Session, *, conn: Any | None = None
    ) -> Session:
        """Create a new session, optionally inside a caller-owned transaction."""
        if not self._pool and conn is None:
            return session
        if conn is None:
            async with self.connection() as connection:
                return await self.create_session(session, conn=connection)
        await conn.execute(
            """
            INSERT INTO sessions
            (id, user_id, main_thread_id, title, description, prompt,
             conversation_id, status, worker_pod_name, created_at,
             started_at, completed_at, summary, error,
             repo_url, project_id, branch_name, base_branch, model,
             issue_url, issue_number, issue_etag, issue_last_modified,
             pr_url, pr_number, pr_etag, pr_last_modified, commit_sha,
             anchor_message_id, color, result)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
                    $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28,
                    $29, $30, $31)
            """,
            session.id,
            session.user_id,
            session.main_thread_id,
            session.title,
            session.description,
            session.prompt,
            session.conversation_id,
            session.status.value,
            session.worker_pod_name,
            session.created_at,
            session.started_at,
            session.completed_at,
            session.summary,
            session.error,
            # Code work fields
            session.repo_url,
            session.project_id,
            session.branch_name,
            session.base_branch,
            session.model,
            # GitHub issue fields
            session.issue_url,
            session.issue_number,
            session.issue_etag,
            session.issue_last_modified,
            # GitHub PR fields
            session.pr_url,
            session.pr_number,
            session.pr_etag,
            session.pr_last_modified,
            session.commit_sha,
            # Inline thread anchoring
            session.anchor_message_id,
            session.color,
            json.dumps(session.result) if session.result else None,
        )
        return session

    async def get_session(self, session_id: str) -> Session | None:
        """Get a session by ID."""
        if not self._pool:
            return None
        async with self.connection() as conn:
            row = await conn.fetchrow(
                """SELECT sessions.*,
                          (SELECT b.kind FROM native_bindings b WHERE b.session_id = sessions.id) AS agent_kind
                   FROM sessions WHERE id = $1""",
                session_id,
            )
        if not row:
            return None
        return self._row_to_session(row)

    async def list_sessions(
        self,
        user_id: str,
        status: SessionStatus | None = None,
        limit: int = 50,
        include_archived: bool = False,
        project_id: str | None = None,
    ) -> list[Session]:
        """List a user's sessions, optionally by project; hide cleared sessions by default."""
        if not self._pool:
            return []

        # The native main thread's session row is the conversation itself, not a listed session.
        query = (
            "SELECT sessions.*, "
            "(SELECT b.kind FROM native_bindings b WHERE b.session_id = sessions.id) AS agent_kind "
            "FROM sessions WHERE user_id = $1 AND NOT EXISTS "
            "(SELECT 1 FROM native_bindings b WHERE b.session_id = sessions.id AND b.role = 'main')"
        )
        params: list[Any] = [user_id]

        if not include_archived:
            query += " AND archived_at IS NULL"

        if status:
            query += f" AND status = ${len(params) + 1}"
            params.append(status.value)

        if project_id is not None:
            query += f" AND project_id = ${len(params) + 1}"
            params.append(project_id)

        query += f" ORDER BY created_at DESC LIMIT ${len(params) + 1}"
        params.append(limit)

        async with self.connection() as conn:
            rows = await conn.fetch(query, *params)
        return [self._row_to_session(row) for row in rows]

    async def archive_sessions(
        self,
        user_id: str,
        session_ids: list[str] | None = None,
        parent_session_id: str | None = None,
    ) -> list[str]:
        """Clear finished sessions from the list; returns the ids that were archived.

        Only finished sessions (completed, failed, cancelled) qualify, and never the main
        thread. Rows are kept for audit. ``session_ids`` narrows to those sessions and
        ``parent_session_id`` to the direct children of that session; both omitted means every
        finished session of the user.
        """
        if not self._pool:
            return []
        async with self.connection() as conn:
            rows = await conn.fetch(
                """UPDATE sessions SET archived_at = NOW()
                   WHERE user_id = $1 AND archived_at IS NULL
                     AND status IN ('completed', 'failed', 'cancelled')
                     AND NOT EXISTS (SELECT 1 FROM native_bindings m
                                     WHERE m.session_id = sessions.id AND m.role = 'main')
                     AND ($2::text[] IS NULL OR id = ANY($2))
                     AND ($3::text IS NULL OR EXISTS (
                            SELECT 1 FROM native_bindings c
                            WHERE c.session_id = sessions.id AND c.parent_session_id = $3))
                   RETURNING id""",
                user_id,
                session_ids,
                parent_session_id,
            )
        from mainloop.runtime.agent_credentials import revoke
        from mainloop.runtime.native_sessions import delete_kagent_session

        for row in rows:
            await revoke(row["id"])
            await delete_kagent_session(row["id"])
        return [r["id"] for r in rows]

    async def update_session(
        self,
        session_id: str,
        status: SessionStatus | None = None,
        worker_pod_name: str | None = None,
        started_at: datetime | None = None,
        completed_at: datetime | None = None,
        summary: str | None = None,
        error: str | None = None,
        # Code work fields
        repo_url: str | None = None,
        project_id: str | None = None,
        branch_name: str | None = None,
        # GitHub issue fields
        issue_url: str | None = None,
        issue_number: int | None = None,
        issue_etag: str | None = None,
        issue_last_modified: datetime | None = None,
        # GitHub PR fields
        pr_url: str | None = None,
        pr_number: int | None = None,
        pr_etag: str | None = None,
        pr_last_modified: datetime | None = None,
        commit_sha: str | None = None,
        # Inline thread anchoring
        anchor_message_id: str | None = None,
        color: str | None = None,
        result: dict | None = None,
    ):
        """Update session fields."""
        if not self._pool:
            return
        updates = []
        params = []
        param_idx = 1

        if status is not None:
            updates.append(f"status = ${param_idx}")
            params.append(status.value)
            param_idx += 1
        if worker_pod_name is not None:
            updates.append(f"worker_pod_name = ${param_idx}")
            params.append(worker_pod_name)
            param_idx += 1
        if started_at is not None:
            updates.append(f"started_at = ${param_idx}")
            params.append(started_at)
            param_idx += 1
        if completed_at is not None:
            updates.append(f"completed_at = ${param_idx}")
            params.append(completed_at)
            param_idx += 1
        if summary is not None:
            updates.append(f"summary = ${param_idx}")
            params.append(summary)
            param_idx += 1
        if error is not None:
            updates.append(f"error = ${param_idx}")
            params.append(error)
            param_idx += 1
        # Code work fields
        if repo_url is not None:
            updates.append(f"repo_url = ${param_idx}")
            params.append(repo_url)
            param_idx += 1
        if project_id is not None:
            updates.append(f"project_id = ${param_idx}")
            params.append(project_id)
            param_idx += 1
        if branch_name is not None:
            updates.append(f"branch_name = ${param_idx}")
            params.append(branch_name)
            param_idx += 1
        # GitHub issue fields
        if issue_url is not None:
            updates.append(f"issue_url = ${param_idx}")
            params.append(issue_url)
            param_idx += 1
        if issue_number is not None:
            updates.append(f"issue_number = ${param_idx}")
            params.append(issue_number)
            param_idx += 1
        if issue_etag is not None:
            updates.append(f"issue_etag = ${param_idx}")
            params.append(issue_etag)
            param_idx += 1
        if issue_last_modified is not None:
            updates.append(f"issue_last_modified = ${param_idx}")
            params.append(issue_last_modified)
            param_idx += 1
        # GitHub PR fields
        if pr_url is not None:
            updates.append(f"pr_url = ${param_idx}")
            params.append(pr_url)
            param_idx += 1
        if pr_number is not None:
            updates.append(f"pr_number = ${param_idx}")
            params.append(pr_number)
            param_idx += 1
        if pr_etag is not None:
            updates.append(f"pr_etag = ${param_idx}")
            params.append(pr_etag)
            param_idx += 1
        if pr_last_modified is not None:
            updates.append(f"pr_last_modified = ${param_idx}")
            params.append(pr_last_modified)
            param_idx += 1
        if commit_sha is not None:
            updates.append(f"commit_sha = ${param_idx}")
            params.append(commit_sha)
            param_idx += 1
        # Inline thread anchoring
        if anchor_message_id is not None:
            updates.append(f"anchor_message_id = ${param_idx}")
            params.append(anchor_message_id)
            param_idx += 1
        if color is not None:
            updates.append(f"color = ${param_idx}")
            params.append(color)
            param_idx += 1
        if result is not None:
            updates.append(f"result = ${param_idx}")
            params.append(json.dumps(result))
            param_idx += 1

        params.append(session_id)

        if updates:
            async with self.connection() as conn:
                await conn.execute(
                    f"UPDATE sessions SET {', '.join(updates)} WHERE id = ${param_idx}",
                    *params,
                )
            if status in (
                SessionStatus.COMPLETED,
                SessionStatus.FAILED,
                SessionStatus.CANCELLED,
            ):
                from mainloop.runtime.agent_credentials import revoke

                await revoke(session_id)

    def _row_to_session(self, row: asyncpg.Record) -> Session:
        result = _parse_json_field(row.get("result"))

        return Session(
            id=row["id"],
            user_id=row["user_id"],
            main_thread_id=row["main_thread_id"],
            title=row["title"],
            description=row["description"],
            prompt=row["prompt"],
            conversation_id=row["conversation_id"],
            agent_kind=row.get("agent_kind"),
            status=SessionStatus(row["status"]),
            worker_pod_name=row.get("worker_pod_name"),
            created_at=row["created_at"],
            started_at=row.get("started_at"),
            completed_at=row.get("completed_at"),
            archived_at=row.get("archived_at"),
            summary=row.get("summary"),
            error=row.get("error"),
            # Code work fields
            repo_url=row.get("repo_url"),
            project_id=row.get("project_id"),
            branch_name=row.get("branch_name"),
            # Legacy workspace rows stored NULL for the remote's default ref.
            base_branch=row.get("base_branch") or "",
            model=row.get("model"),
            # GitHub issue fields
            issue_url=row.get("issue_url"),
            issue_number=row.get("issue_number"),
            issue_etag=row.get("issue_etag"),
            issue_last_modified=row.get("issue_last_modified"),
            # GitHub PR fields
            pr_url=row.get("pr_url"),
            pr_number=row.get("pr_number"),
            pr_etag=row.get("pr_etag"),
            pr_last_modified=row.get("pr_last_modified"),
            commit_sha=row.get("commit_sha"),
            # Inline thread anchoring
            anchor_message_id=row.get("anchor_message_id"),
            color=row.get("color"),
            result=result,
        )

    # ============= Session Notification Operations =============

    async def create_session_notification(
        self, notification: SessionNotification
    ) -> SessionNotification:
        """Create a new session notification."""
        if not self._pool:
            return notification
        async with self.connection() as conn:
            await conn.execute(
                """
                INSERT INTO session_notifications
                (id, session_id, user_id, title, preview, read, created_at)
                VALUES ($1, $2, $3, $4, $5, $6, $7)
                """,
                notification.id,
                notification.session_id,
                notification.user_id,
                notification.title,
                notification.preview,
                notification.read,
                notification.created_at,
            )
        return notification

    async def list_session_notifications(
        self,
        user_id: str,
        unread_only: bool = True,
        limit: int = 50,
    ) -> list[SessionNotification]:
        """List session notifications for a user."""
        if not self._pool:
            return []

        if unread_only:
            query = """
                SELECT * FROM session_notifications
                WHERE user_id = $1 AND read = FALSE
                ORDER BY created_at DESC
                LIMIT $2
            """
        else:
            query = """
                SELECT * FROM session_notifications
                WHERE user_id = $1
                ORDER BY created_at DESC
                LIMIT $2
            """

        async with self.connection() as conn:
            rows = await conn.fetch(query, user_id, limit)
        return [self._row_to_session_notification(row) for row in rows]

    async def mark_session_notification_read(self, notification_id: str) -> None:
        """Mark a session notification as read."""
        if not self._pool:
            return
        async with self.connection() as conn:
            await conn.execute(
                "UPDATE session_notifications SET read = TRUE WHERE id = $1",
                notification_id,
            )

    async def dismiss_session_notification(
        self, notification_id: str, user_id: str
    ) -> bool:
        """Delete one of the user's session notifications; False if it is not theirs or is gone."""
        if not self._pool:
            return False
        async with self.connection() as conn:
            result = await conn.execute(
                "DELETE FROM session_notifications WHERE id = $1 AND user_id = $2",
                notification_id,
                user_id,
            )
        return result.endswith(" 1")

    def _row_to_session_notification(self, row: asyncpg.Record) -> SessionNotification:
        return SessionNotification(
            id=row["id"],
            session_id=row["session_id"],
            user_id=row["user_id"],
            title=row["title"],
            preview=row["preview"],
            read=row["read"],
            created_at=row["created_at"],
        )


# Global database instance
db = Database()
