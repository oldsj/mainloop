"""DBOS transaction functions for database operations in workflows.

These functions use @DBOS.transaction() which provides:
- Automatic connection management via DBOS's pool
- ACID transaction semantics
- Checkpointing for workflow recovery

Note: These are synchronous functions (not async) per DBOS requirements.
"""

import json
import uuid
from datetime import datetime, timezone

from dbos import DBOS
from sqlalchemy import text

from models import (
    MainThread,
    QueueItem,
    Session,
    SessionStatus,
)


def _parse_json_field(value) -> dict | list | None:
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


# ============= Main Thread Transactions =============


@DBOS.transaction()
def save_main_thread(thread: MainThread) -> MainThread:
    """Save main thread to database."""
    DBOS.sql_session.execute(
        text(
            """
            INSERT INTO main_threads (id, user_id, workflow_run_id, status, created_at, last_activity_at, active_tasks, context)
            VALUES (:id, :user_id, :workflow_run_id, :status, :created_at, :last_activity_at, :active_tasks, :context)
        """
        ),
        {
            "id": thread.id,
            "user_id": thread.user_id,
            "workflow_run_id": thread.workflow_run_id,
            "status": thread.status,
            "created_at": thread.created_at,
            "last_activity_at": thread.last_activity_at,
            "active_tasks": thread.active_tasks,
            "context": json.dumps(thread.context) if thread.context else "{}",
        },
    )
    return thread


@DBOS.transaction()
def get_main_thread_by_user(user_id: str) -> MainThread | None:
    """Get main thread for user."""
    result = DBOS.sql_session.execute(
        text("SELECT * FROM main_threads WHERE user_id = :user_id LIMIT 1"),
        {"user_id": user_id},
    )
    row = result.mappings().first()
    if not row:
        return None
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


# ============= Queue Item Transactions =============


@DBOS.transaction()
def save_queue_item(item: QueueItem) -> QueueItem:
    """Save queue item to database."""
    DBOS.sql_session.execute(
        text(
            """
            INSERT INTO queue_items
            (id, main_thread_id, task_id, user_id, item_type, priority,
             title, content, context, options, status, created_at, expires_at)
            VALUES (:id, :main_thread_id, :task_id, :user_id, :item_type, :priority,
                    :title, :content, :context, :options, :status, :created_at, :expires_at)
        """
        ),
        {
            "id": item.id,
            "main_thread_id": item.main_thread_id,
            "task_id": item.task_id,
            "user_id": item.user_id,
            "item_type": item.item_type.value,
            "priority": item.priority.value,
            "title": item.title,
            "content": item.content,
            "context": json.dumps(item.context) if item.context else "{}",
            "options": item.options,
            "status": item.status,
            "created_at": item.created_at,
            "expires_at": item.expires_at,
        },
    )
    return item


@DBOS.transaction()
def update_queue_item_response(item_id: str, response: str) -> None:
    """Update queue item with response."""
    DBOS.sql_session.execute(
        text(
            """
            UPDATE queue_items
            SET status = :status, response = :response, responded_at = :responded_at
            WHERE id = :id
        """
        ),
        {
            "id": item_id,
            "status": "responded",
            "response": response,
            "responded_at": datetime.now(timezone.utc),
        },
    )


# ============= Message Transactions =============


@DBOS.transaction()
def save_assistant_message(conversation_id: str, content: str) -> None:
    """Save an assistant message to a conversation."""
    message_id = str(uuid.uuid4())
    now = datetime.now(timezone.utc)

    # Insert the message
    DBOS.sql_session.execute(
        text(
            """
            INSERT INTO messages (id, conversation_id, role, content, created_at)
            VALUES (:id, :conversation_id, :role, :content, :created_at)
        """
        ),
        {
            "id": message_id,
            "conversation_id": conversation_id,
            "role": "assistant",
            "content": content,
            "created_at": now,
        },
    )

    # Update conversation's updated_at
    DBOS.sql_session.execute(
        text("UPDATE conversations SET updated_at = :updated_at WHERE id = :id"),
        {"updated_at": now, "id": conversation_id},
    )


@DBOS.transaction()
def add_message_to_conversation(conversation_id: str, role: str, content: str) -> str:
    """Add a message to the session's conversation and increment count."""
    message_id = str(uuid.uuid4())
    now = datetime.now(timezone.utc)

    # Insert the message
    DBOS.sql_session.execute(
        text(
            """
            INSERT INTO messages (id, conversation_id, role, content, created_at)
            VALUES (:id, :conversation_id, :role, :content, :created_at)
        """
        ),
        {
            "id": message_id,
            "conversation_id": conversation_id,
            "role": role,
            "content": content,
            "created_at": now,
        },
    )

    # Increment message count
    DBOS.sql_session.execute(
        text(
            """
            UPDATE conversations
            SET message_count = message_count + 1, updated_at = :updated_at
            WHERE id = :id
        """
        ),
        {"updated_at": now, "id": conversation_id},
    )

    return message_id


# ============= Session Transactions =============


@DBOS.transaction()
def load_session(session_id: str) -> Session | None:
    """Load session from database."""
    result = DBOS.sql_session.execute(
        text("SELECT * FROM sessions WHERE id = :id"),
        {"id": session_id},
    )
    row = result.mappings().first()
    if not row:
        return None

    return Session(
        id=row["id"],
        user_id=row["user_id"],
        main_thread_id=row["main_thread_id"],
        title=row["title"],
        description=row["description"],
        prompt=row["prompt"],
        conversation_id=row["conversation_id"],
        status=SessionStatus(row["status"]),
        worker_pod_name=row.get("worker_pod_name"),
        created_at=row["created_at"],
        started_at=row.get("started_at"),
        completed_at=row.get("completed_at"),
        summary=row.get("summary"),
        error=row.get("error"),
        repo_url=row.get("repo_url"),
        project_id=row.get("project_id"),
        branch_name=row.get("branch_name"),
        base_branch=row.get("base_branch", "main"),
        model=row.get("model"),
        issue_url=row.get("issue_url"),
        issue_number=row.get("issue_number"),
        issue_etag=row.get("issue_etag"),
        issue_last_modified=row.get("issue_last_modified"),
        pr_url=row.get("pr_url"),
        pr_number=row.get("pr_number"),
        pr_etag=row.get("pr_etag"),
        pr_last_modified=row.get("pr_last_modified"),
        commit_sha=row.get("commit_sha"),
        anchor_message_id=row.get("anchor_message_id"),
        color=row.get("color"),
        result=_parse_json_field(row.get("result")),
    )


@DBOS.transaction()
def update_session_status(
    session_id: str,
    status: SessionStatus | None = None,
    started_at: datetime | None = None,
    completed_at: datetime | None = None,
    error: str | None = None,
) -> None:
    """Update session status fields."""
    params: dict = {"id": session_id}
    set_clauses = []

    if status is not None:
        set_clauses.append("status = :status")
        params["status"] = status.value
    if started_at is not None:
        set_clauses.append("started_at = :started_at")
        params["started_at"] = started_at
    if completed_at is not None:
        set_clauses.append("completed_at = :completed_at")
        params["completed_at"] = completed_at
    if error is not None:
        set_clauses.append("error = :error")
        params["error"] = error

    if set_clauses:
        DBOS.sql_session.execute(
            text(
                f"UPDATE sessions SET {', '.join(set_clauses)} WHERE id = :id"  # nosec B608
            ),
            params,
        )
