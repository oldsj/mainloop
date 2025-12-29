"""PostgreSQL client for durable workflow persistence."""

import json
import asyncpg
from datetime import datetime, timezone
from typing import Any
from contextlib import asynccontextmanager

from models import (
    Conversation,
    Message,
    MainThread,
    WorkerTask,
    TaskStatus,
    QueueItem,
    QueueItemType,
    QueueItemPriority,
)
from mainloop.config import settings


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
    commit_sha TEXT
);
CREATE INDEX IF NOT EXISTS idx_worker_tasks_main_thread ON worker_tasks(main_thread_id);
CREATE INDEX IF NOT EXISTS idx_worker_tasks_user_id ON worker_tasks(user_id);
CREATE INDEX IF NOT EXISTS idx_worker_tasks_status ON worker_tasks(status);

-- Queue items (human-in-the-loop)
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
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_queue_items_user_id ON queue_items(user_id);
CREATE INDEX IF NOT EXISTS idx_queue_items_status ON queue_items(status);
CREATE INDEX IF NOT EXISTS idx_queue_items_main_thread ON queue_items(main_thread_id);

-- Conversations (migrated from BigQuery)
CREATE TABLE IF NOT EXISTS conversations (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    title TEXT,
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
        """Create tables if they don't exist."""
        if not self._pool:
            return
        async with self.connection() as conn:
            await conn.execute(SCHEMA_SQL)

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
            context=row["context"] if isinstance(row["context"], dict) else (json.loads(row["context"]) if row["context"] else {}),
        )

    # ============= Worker Task Operations =============

    async def create_worker_task(self, task: WorkerTask) -> WorkerTask:
        """Create a new worker task."""
        if not self._pool:
            return task
        async with self.connection() as conn:
            await conn.execute(
                """
                INSERT INTO worker_tasks
                (id, main_thread_id, user_id, task_type, description, prompt,
                 repo_url, branch_name, base_branch, status, created_at)
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
                """,
                task.id,
                task.main_thread_id,
                task.user_id,
                task.task_type,
                task.description,
                task.prompt,
                task.repo_url,
                task.branch_name,
                task.base_branch,
                task.status.value,
                task.created_at,
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
        limit: int = 50,
    ) -> list[WorkerTask]:
        """List worker tasks for a user."""
        if not self._pool:
            return []

        query = "SELECT * FROM worker_tasks WHERE user_id = $1"
        params: list[Any] = [user_id]

        if status:
            query += " AND status = $2"
            params.append(status)

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
        pr_url: str | None = None,
        pr_number: int | None = None,
        commit_sha: str | None = None,
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
            repo_url=row["repo_url"],
            branch_name=row["branch_name"],
            base_branch=row["base_branch"],
            status=TaskStatus(row["status"]),
            workflow_run_id=row["workflow_run_id"],
            worker_pod_name=row["worker_pod_name"],
            created_at=row["created_at"],
            started_at=row["started_at"],
            completed_at=row["completed_at"],
            result=row["result"] if isinstance(row["result"], dict) else (json.loads(row["result"]) if row["result"] else None),
            error=row["error"],
            pr_url=row["pr_url"],
            commit_sha=row["commit_sha"],
        )

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
    ) -> list[QueueItem]:
        """List queue items for a user."""
        if not self._pool:
            return []
        async with self.connection() as conn:
            rows = await conn.fetch(
                """
                SELECT * FROM queue_items
                WHERE user_id = $1 AND status = $2
                ORDER BY
                    CASE priority
                        WHEN 'urgent' THEN 1
                        WHEN 'high' THEN 2
                        WHEN 'normal' THEN 3
                        ELSE 4
                    END,
                    created_at DESC
                LIMIT $3
                """,
                user_id,
                status,
                limit,
            )
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
            context=row["context"] if isinstance(row["context"], dict) else (json.loads(row["context"]) if row["context"] else {}),
            options=list(row["options"]) if row["options"] else None,
            status=row["status"],
            response=row["response"],
            responded_at=row["responded_at"],
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
            created_at=datetime.now(timezone.utc),
            updated_at=datetime.now(timezone.utc),
        )
        if not self._pool:
            return conversation
        async with self.connection() as conn:
            await conn.execute(
                """
                INSERT INTO conversations (id, user_id, title, created_at, updated_at)
                VALUES ($1, $2, $3, $4, $5)
                """,
                conversation.id,
                conversation.user_id,
                conversation.title,
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
        return Conversation(
            id=row["id"],
            user_id=row["user_id"],
            title=row["title"],
            created_at=row["created_at"],
            updated_at=row["updated_at"],
        )

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
        return [
            Conversation(
                id=row["id"],
                user_id=row["user_id"],
                title=row["title"],
                created_at=row["created_at"],
                updated_at=row["updated_at"],
            )
            for row in rows
        ]

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


# Global database instance
db = Database()
