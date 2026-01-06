"""PostgreSQL client for durable workflow persistence."""

import json
from contextlib import asynccontextmanager
from datetime import datetime, timezone
from typing import Any

import asyncpg
from mainloop.config import settings

from models import (
    Conversation,
    MainThread,
    Message,
    Project,
    QueueItem,
    QueueItemPriority,
    QueueItemType,
    TaskStatus,
    WorkerTask,
)


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

-- Worker tasks
CREATE TABLE IF NOT EXISTS worker_tasks (
    id TEXT PRIMARY KEY,
    main_thread_id TEXT NOT NULL REFERENCES main_threads(id),
    user_id TEXT NOT NULL,
    task_type TEXT NOT NULL,
    description TEXT NOT NULL,
    prompt TEXT NOT NULL,
    model TEXT,
    repo_url TEXT,
    branch_name TEXT,
    base_branch TEXT DEFAULT 'main',
    status TEXT NOT NULL DEFAULT 'pending',
    workflow_run_id TEXT,
    worker_pod_name TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    result JSONB,
    error TEXT,
    pr_url TEXT,
    pr_number INTEGER,
    commit_sha TEXT,
    -- Conversation linking for routing
    conversation_id TEXT,
    message_id TEXT,
    keywords TEXT[] DEFAULT '{}',
    skip_plan BOOLEAN DEFAULT FALSE,
    -- Interactive planning state
    pending_questions JSONB,
    plan_text TEXT
);
CREATE INDEX IF NOT EXISTS idx_worker_tasks_main_thread ON worker_tasks(main_thread_id);
CREATE INDEX IF NOT EXISTS idx_worker_tasks_user_id ON worker_tasks(user_id);
CREATE INDEX IF NOT EXISTS idx_worker_tasks_status ON worker_tasks(status);

-- Queue items (human-in-the-loop / inbox)
CREATE TABLE IF NOT EXISTS queue_items (
    id TEXT PRIMARY KEY,
    main_thread_id TEXT NOT NULL REFERENCES main_threads(id),
    task_id TEXT REFERENCES worker_tasks(id),
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
    open_issue_count INTEGER DEFAULT 0,
    UNIQUE(user_id, full_name)
);
CREATE INDEX IF NOT EXISTS idx_projects_user_id ON projects(user_id);
CREATE INDEX IF NOT EXISTS idx_projects_last_used ON projects(last_used_at DESC);
"""

# Migration SQL for adding new columns to existing tables
MIGRATION_SQL = """
-- Add new columns to worker_tasks if they don't exist
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='worker_tasks' AND column_name='model') THEN
        ALTER TABLE worker_tasks ADD COLUMN model TEXT;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='worker_tasks' AND column_name='pr_number') THEN
        ALTER TABLE worker_tasks ADD COLUMN pr_number INTEGER;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='worker_tasks' AND column_name='conversation_id') THEN
        ALTER TABLE worker_tasks ADD COLUMN conversation_id TEXT;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='worker_tasks' AND column_name='message_id') THEN
        ALTER TABLE worker_tasks ADD COLUMN message_id TEXT;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='worker_tasks' AND column_name='keywords') THEN
        ALTER TABLE worker_tasks ADD COLUMN keywords TEXT[] DEFAULT '{}';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='worker_tasks' AND column_name='skip_plan') THEN
        ALTER TABLE worker_tasks ADD COLUMN skip_plan BOOLEAN DEFAULT FALSE;
    END IF;
    -- Issue tracking columns (plan phase)
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='worker_tasks' AND column_name='issue_url') THEN
        ALTER TABLE worker_tasks ADD COLUMN issue_url TEXT;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='worker_tasks' AND column_name='issue_number') THEN
        ALTER TABLE worker_tasks ADD COLUMN issue_number INTEGER;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='worker_tasks' AND column_name='issue_etag') THEN
        ALTER TABLE worker_tasks ADD COLUMN issue_etag TEXT;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='worker_tasks' AND column_name='issue_last_modified') THEN
        ALTER TABLE worker_tasks ADD COLUMN issue_last_modified TIMESTAMPTZ;
    END IF;
    -- ETag columns for PR polling
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='worker_tasks' AND column_name='pr_etag') THEN
        ALTER TABLE worker_tasks ADD COLUMN pr_etag TEXT;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='worker_tasks' AND column_name='pr_last_modified') THEN
        ALTER TABLE worker_tasks ADD COLUMN pr_last_modified TIMESTAMPTZ;
    END IF;
    -- Interactive planning columns
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='worker_tasks' AND column_name='pending_questions') THEN
        ALTER TABLE worker_tasks ADD COLUMN pending_questions JSONB;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='worker_tasks' AND column_name='plan_text') THEN
        ALTER TABLE worker_tasks ADD COLUMN plan_text TEXT;
    END IF;
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
    -- Add project_id to worker_tasks
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='worker_tasks' AND column_name='project_id') THEN
        ALTER TABLE worker_tasks ADD COLUMN project_id TEXT REFERENCES projects(id);
    END IF;
END $$;

-- Migrate existing repo URLs to projects and link tasks
DO $$
DECLARE
    task_record RECORD;
    v_project_id TEXT;
    repo_owner TEXT;
    repo_name TEXT;
    v_full_name TEXT;
BEGIN
    -- Only run if projects table is empty (first migration)
    IF NOT EXISTS (SELECT 1 FROM projects LIMIT 1) THEN
        FOR task_record IN
            SELECT DISTINCT user_id, repo_url
            FROM worker_tasks
            WHERE repo_url IS NOT NULL
        LOOP
            -- Parse owner/name from URL (handles both https://github.com/owner/repo and https://github.com/owner/repo.git)
            repo_owner := split_part(
                replace(replace(task_record.repo_url, 'https://github.com/', ''), '.git', ''),
                '/', 1
            );
            repo_name := split_part(
                replace(replace(task_record.repo_url, 'https://github.com/', ''), '.git', ''),
                '/', 2
            );
            v_full_name := repo_owner || '/' || repo_name;

            -- Check if project already exists for this user
            SELECT id INTO v_project_id
            FROM projects
            WHERE projects.user_id = task_record.user_id AND projects.full_name = v_full_name;

            IF v_project_id IS NULL THEN
                v_project_id := gen_random_uuid()::TEXT;
                INSERT INTO projects (id, user_id, owner, name, full_name, html_url, created_at, last_used_at)
                VALUES (
                    v_project_id,
                    task_record.user_id,
                    repo_owner,
                    repo_name,
                    v_full_name,
                    task_record.repo_url,
                    NOW(),
                    NOW()
                );
            END IF;

            -- Update tasks to reference the project
            UPDATE worker_tasks
            SET project_id = v_project_id
            WHERE worker_tasks.repo_url = task_record.repo_url AND worker_tasks.user_id = task_record.user_id;
        END LOOP;
    END IF;
END $$;

-- Create indexes if they don't exist
CREATE INDEX IF NOT EXISTS idx_worker_tasks_keywords ON worker_tasks USING GIN(keywords);
CREATE INDEX IF NOT EXISTS idx_worker_tasks_project ON worker_tasks(project_id);
CREATE INDEX IF NOT EXISTS idx_queue_items_read_at ON queue_items(read_at);
"""


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

    # ============= Worker Task Operations =============

    async def create_worker_task(self, task: WorkerTask) -> WorkerTask:
        """Create a new worker task."""
        if not self._pool:
            return task
        async with self.connection() as conn:
            # Serialize pending_questions to JSON for storage
            import json
            pending_questions_json = (
                json.dumps([q.model_dump() for q in task.pending_questions])
                if task.pending_questions
                else None
            )

            await conn.execute(
                """
                INSERT INTO worker_tasks
                (id, main_thread_id, user_id, task_type, description, prompt, model,
                 repo_url, branch_name, base_branch, status, created_at,
                 conversation_id, message_id, keywords, skip_plan, plan_text, pending_questions)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
                """,
                task.id,
                task.main_thread_id,
                task.user_id,
                task.task_type,
                task.description,
                task.prompt,
                task.model,
                task.repo_url,
                task.branch_name,
                task.base_branch,
                task.status.value,
                task.created_at,
                task.conversation_id,
                task.message_id,
                task.keywords,
                task.skip_plan,
                task.plan_text,
                pending_questions_json,
            )
        return task

    async def get_worker_task(self, task_id: str) -> WorkerTask | None:
        """Get a worker task by ID."""
        if not self._pool:
            return None
        async with self.connection() as conn:
            row = await conn.fetchrow(
                "SELECT * FROM worker_tasks WHERE id = $1", task_id
            )
        if not row:
            return None
        return self._row_to_worker_task(row)

    async def list_worker_tasks(
        self,
        user_id: str,
        status: str | None = None,
        project_id: str | None = None,
        limit: int = 50,
    ) -> list[WorkerTask]:
        """List worker tasks for a user."""
        if not self._pool:
            return []

        query = "SELECT * FROM worker_tasks WHERE user_id = $1"
        params: list[Any] = [user_id]

        if status:
            query += f" AND status = ${len(params) + 1}"
            params.append(status)

        if project_id:
            query += f" AND project_id = ${len(params) + 1}"
            params.append(project_id)

        query += f" ORDER BY created_at DESC LIMIT ${len(params) + 1}"
        params.append(limit)

        async with self.connection() as conn:
            rows = await conn.fetch(query, *params)
        return [self._row_to_worker_task(row) for row in rows]

    async def update_worker_task(
        self,
        task_id: str,
        status: TaskStatus | None = None,
        workflow_run_id: str | None = None,
        worker_pod_name: str | None = None,
        started_at: datetime | None = None,
        completed_at: datetime | None = None,
        result: dict | None = None,
        error: str | None = None,
        project_id: str | None = None,
        # Issue fields (plan phase)
        issue_url: str | None = None,
        issue_number: int | None = None,
        issue_etag: str | None = None,
        issue_last_modified: datetime | None = None,
        # PR fields (implementation phase)
        pr_url: str | None = None,
        pr_number: int | None = None,
        pr_etag: str | None = None,
        pr_last_modified: datetime | None = None,
        commit_sha: str | None = None,
        branch_name: str | None = None,
        # Interactive planning fields
        pending_questions: list[dict] | None = None,
        plan_text: str | None = None,
    ):
        """Update worker task fields."""
        if not self._pool:
            return
        updates = []
        params = []
        param_idx = 1

        if status is not None:
            updates.append(f"status = ${param_idx}")
            params.append(status.value if isinstance(status, TaskStatus) else status)
            param_idx += 1
        if workflow_run_id is not None:
            updates.append(f"workflow_run_id = ${param_idx}")
            params.append(workflow_run_id)
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
        if result is not None:
            updates.append(f"result = ${param_idx}")
            params.append(json.dumps(result))
            param_idx += 1
        if error is not None:
            updates.append(f"error = ${param_idx}")
            params.append(error)
            param_idx += 1
        if project_id is not None:
            updates.append(f"project_id = ${param_idx}")
            params.append(project_id)
            param_idx += 1
        if pr_url is not None:
            updates.append(f"pr_url = ${param_idx}")
            params.append(pr_url)
            param_idx += 1
        if pr_number is not None:
            updates.append(f"pr_number = ${param_idx}")
            params.append(pr_number)
            param_idx += 1
        if commit_sha is not None:
            updates.append(f"commit_sha = ${param_idx}")
            params.append(commit_sha)
            param_idx += 1
        if branch_name is not None:
            updates.append(f"branch_name = ${param_idx}")
            params.append(branch_name)
            param_idx += 1
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
        if pr_etag is not None:
            updates.append(f"pr_etag = ${param_idx}")
            params.append(pr_etag)
            param_idx += 1
        if pr_last_modified is not None:
            updates.append(f"pr_last_modified = ${param_idx}")
            params.append(pr_last_modified)
            param_idx += 1
        # pending_questions: empty list [] means clear, list with items means set
        # None means don't update (standard pattern)
        if pending_questions is not None:
            updates.append(f"pending_questions = ${param_idx}")
            # Empty list clears (stores null), non-empty stores as JSON string
            # asyncpg requires explicit JSON serialization for JSONB columns
            params.append(json.dumps(pending_questions) if pending_questions else None)
            param_idx += 1
        if plan_text is not None:
            updates.append(f"plan_text = ${param_idx}")
            params.append(plan_text)
            param_idx += 1

        params.append(task_id)

        if updates:
            async with self.connection() as conn:
                await conn.execute(
                    f"UPDATE worker_tasks SET {', '.join(updates)} WHERE id = ${param_idx}",
                    *params,
                )

    def _row_to_worker_task(self, row: asyncpg.Record) -> WorkerTask:
        return WorkerTask(
            id=row["id"],
            main_thread_id=row["main_thread_id"],
            user_id=row["user_id"],
            task_type=row["task_type"],
            description=row["description"],
            prompt=row["prompt"],
            model=row.get("model"),
            repo_url=row["repo_url"],
            project_id=row.get("project_id"),
            branch_name=row["branch_name"],
            base_branch=row["base_branch"],
            status=TaskStatus(row["status"]),
            workflow_run_id=row["workflow_run_id"],
            worker_pod_name=row["worker_pod_name"],
            created_at=row["created_at"],
            started_at=row["started_at"],
            completed_at=row["completed_at"],
            result=(
                row["result"]
                if isinstance(row["result"], dict)
                else (json.loads(row["result"]) if row["result"] else None)
            ),
            error=row["error"],
            # Issue fields (plan phase)
            issue_url=row.get("issue_url"),
            issue_number=row.get("issue_number"),
            issue_etag=row.get("issue_etag"),
            issue_last_modified=row.get("issue_last_modified"),
            # PR fields (implementation phase)
            pr_url=row["pr_url"],
            pr_number=row.get("pr_number"),
            pr_etag=row.get("pr_etag"),
            pr_last_modified=row.get("pr_last_modified"),
            commit_sha=row["commit_sha"],
            conversation_id=row.get("conversation_id"),
            message_id=row.get("message_id"),
            keywords=list(row["keywords"]) if row.get("keywords") else [],
            skip_plan=row.get("skip_plan", False),
            # Interactive planning state
            # Handle both JSONB (returns list) and legacy string data
            pending_questions=_parse_json_field(row.get("pending_questions")),
            plan_text=row.get("plan_text"),
        )

    # ============= Project Operations =============

    async def create_project(self, project: Project) -> Project:
        """Create a new project."""
        if not self._pool:
            return project
        async with self.connection() as conn:
            await conn.execute(
                """
                INSERT INTO projects
                (id, user_id, owner, name, full_name, description, default_branch,
                 avatar_url, html_url, created_at, last_used_at, metadata_updated_at,
                 open_pr_count, open_issue_count)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
                """,
                project.id,
                project.user_id,
                project.owner,
                project.name,
                project.full_name,
                project.description,
                project.default_branch,
                project.avatar_url,
                project.html_url,
                project.created_at,
                project.last_used_at,
                project.metadata_updated_at,
                project.open_pr_count,
                project.open_issue_count,
            )
        return project

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
        return Project(
            id=row["id"],
            user_id=row["user_id"],
            owner=row["owner"],
            name=row["name"],
            full_name=row["full_name"],
            description=row.get("description"),
            default_branch=row.get("default_branch", "main"),
            avatar_url=row.get("avatar_url"),
            html_url=row["html_url"],
            created_at=row["created_at"],
            last_used_at=row["last_used_at"],
            metadata_updated_at=row.get("metadata_updated_at"),
            open_pr_count=row.get("open_pr_count", 0),
            open_issue_count=row.get("open_issue_count", 0),
        )

    async def get_project_by_repo(self, user_id: str, full_name: str) -> Project | None:
        """Get a project by GitHub full_name (owner/repo)."""
        if not self._pool:
            return None
        async with self.connection() as conn:
            row = await conn.fetchrow(
                "SELECT * FROM projects WHERE user_id = $1 AND full_name = $2",
                user_id,
                full_name,
            )
        if not row:
            return None
        return Project(
            id=row["id"],
            user_id=row["user_id"],
            owner=row["owner"],
            name=row["name"],
            full_name=row["full_name"],
            description=row.get("description"),
            default_branch=row.get("default_branch", "main"),
            avatar_url=row.get("avatar_url"),
            html_url=row["html_url"],
            created_at=row["created_at"],
            last_used_at=row["last_used_at"],
            metadata_updated_at=row.get("metadata_updated_at"),
            open_pr_count=row.get("open_pr_count", 0),
            open_issue_count=row.get("open_issue_count", 0),
        )

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
        return [
            Project(
                id=row["id"],
                user_id=row["user_id"],
                owner=row["owner"],
                name=row["name"],
                full_name=row["full_name"],
                description=row.get("description"),
                default_branch=row.get("default_branch", "main"),
                avatar_url=row.get("avatar_url"),
                html_url=row["html_url"],
                created_at=row["created_at"],
                last_used_at=row["last_used_at"],
                metadata_updated_at=row.get("metadata_updated_at"),
                open_pr_count=row.get("open_pr_count", 0),
                open_issue_count=row.get("open_issue_count", 0),
            )
            for row in rows
        ]

    async def update_project_metadata(
        self,
        project_id: str,
        description: str | None = None,
        avatar_url: str | None = None,
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

    async def get_or_create_project_from_url(
        self, user_id: str, repo_url: str
    ) -> Project:
        """Get or create a project from a GitHub URL."""
        # Parse owner/repo from URL
        repo_url_clean = repo_url.replace("https://github.com/", "").replace(".git", "")
        parts = repo_url_clean.split("/")
        if len(parts) < 2:
            raise ValueError(f"Invalid GitHub URL: {repo_url}")

        owner = parts[0]
        name = parts[1]
        full_name = f"{owner}/{name}"

        # Try to get existing project
        project = await self.get_project_by_repo(user_id, full_name)
        if project:
            # Touch to update last_used_at
            await self.touch_project(project.id)
            return project

        # Create new project
        project = Project(
            user_id=user_id,
            owner=owner,
            name=name,
            full_name=full_name,
            html_url=repo_url,
        )
        return await self.create_project(project)

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
            WHERE {' AND '.join(conditions)}
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
        """List conversations for a user."""
        if not self._pool:
            return []
        async with self.connection() as conn:
            rows = await conn.fetch(
                """
                SELECT * FROM conversations
                WHERE user_id = $1
                ORDER BY updated_at DESC
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
        self, conversation_id: str, role: str, content: str
    ) -> Message:
        """Create a new message in a conversation."""
        import uuid

        message = Message(
            id=str(uuid.uuid4()),
            conversation_id=conversation_id,
            role=role,  # type: ignore
            content=content,
            created_at=datetime.now(timezone.utc),
        )
        if not self._pool:
            return message
        async with self.connection() as conn:
            await conn.execute(
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
            await conn.execute(
                "UPDATE conversations SET updated_at = $1 WHERE id = $2",
                datetime.now(timezone.utc),
                conversation_id,
            )
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


# Global database instance
db = Database()
