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
    SessionQuestion,
    SessionStatus,
    TaskStatus,
    WorkerTask,
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


# ============= Worker Task Transactions =============


@DBOS.transaction()
def save_worker_task(task: WorkerTask) -> WorkerTask:
    """Save worker task to database."""
    pending_questions_json = (
        json.dumps([q.model_dump() for q in task.pending_questions])
        if task.pending_questions
        else None
    )

    DBOS.sql_session.execute(
        text(
            """
            INSERT INTO worker_tasks
            (id, main_thread_id, user_id, task_type, description, prompt, model,
             repo_url, branch_name, base_branch, status, created_at,
             conversation_id, message_id, keywords, skip_plan, plan_text, pending_questions)
            VALUES (:id, :main_thread_id, :user_id, :task_type, :description, :prompt, :model,
                    :repo_url, :branch_name, :base_branch, :status, :created_at,
                    :conversation_id, :message_id, :keywords, :skip_plan, :plan_text, :pending_questions)
        """
        ),
        {
            "id": task.id,
            "main_thread_id": task.main_thread_id,
            "user_id": task.user_id,
            "task_type": task.task_type,
            "description": task.description,
            "prompt": task.prompt,
            "model": task.model,
            "repo_url": task.repo_url,
            "branch_name": task.branch_name,
            "base_branch": task.base_branch,
            "status": task.status.value,
            "created_at": task.created_at,
            "conversation_id": task.conversation_id,
            "message_id": task.message_id,
            "keywords": task.keywords,
            "skip_plan": task.skip_plan,
            "plan_text": task.plan_text,
            "pending_questions": pending_questions_json,
        },
    )
    return task


@DBOS.transaction()
def load_worker_task(task_id: str) -> WorkerTask | None:
    """Load worker task from database."""
    result = DBOS.sql_session.execute(
        text("SELECT * FROM worker_tasks WHERE id = :id"),
        {"id": task_id},
    )
    row = result.mappings().first()
    if not row:
        return None

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
        result=_parse_json_field(row["result"]),
        error=row["error"],
        issue_url=row.get("issue_url"),
        issue_number=row.get("issue_number"),
        issue_etag=row.get("issue_etag"),
        issue_last_modified=row.get("issue_last_modified"),
        pr_url=row["pr_url"],
        pr_number=row.get("pr_number"),
        pr_etag=row.get("pr_etag"),
        pr_last_modified=row.get("pr_last_modified"),
        commit_sha=row["commit_sha"],
        conversation_id=row.get("conversation_id"),
        message_id=row.get("message_id"),
        keywords=list(row["keywords"]) if row.get("keywords") else [],
        skip_plan=row.get("skip_plan", False),
        pending_questions=_parse_json_field(row.get("pending_questions")),
        plan_text=row.get("plan_text"),
    )


@DBOS.transaction()
def update_task_status(
    task_id: str,
    status: TaskStatus,
    result: dict | None = None,
    error: str | None = None,
    pr_url: str | None = None,
) -> None:
    """Update worker task status."""
    params: dict = {"id": task_id, "status": status.value}
    set_clauses = ["status = :status"]

    if result is not None:
        set_clauses.append("result = :result")
        params["result"] = json.dumps(result)
    if error is not None:
        set_clauses.append("error = :error")
        params["error"] = error
    if pr_url is not None:
        set_clauses.append("pr_url = :pr_url")
        params["pr_url"] = pr_url

    DBOS.sql_session.execute(
        text(
            f"UPDATE worker_tasks SET {', '.join(set_clauses)} WHERE id = :id"  # nosec B608
        ),
        params,
    )


@DBOS.transaction()
def update_worker_task_status(
    task_id: str,
    status: TaskStatus | None = None,
    issue_url: str | None = None,
    issue_number: int | None = None,
    pr_url: str | None = None,
    pr_number: int | None = None,
    branch_name: str | None = None,
    error: str | None = None,
    pending_questions: list | None = None,
    plan_text: str | None = None,
    started_at: datetime | None = None,
    completed_at: datetime | None = None,
    result: dict | None = None,
    project_id: str | None = None,
    commit_sha: str | None = None,
    issue_etag: str | None = None,
    pr_etag: str | None = None,
) -> None:
    """Update worker task fields."""
    params: dict = {"id": task_id}
    set_clauses = []

    if status is not None:
        set_clauses.append("status = :status")
        params["status"] = status.value
    if issue_url is not None:
        set_clauses.append("issue_url = :issue_url")
        params["issue_url"] = issue_url
    if issue_number is not None:
        set_clauses.append("issue_number = :issue_number")
        params["issue_number"] = issue_number
    if pr_url is not None:
        set_clauses.append("pr_url = :pr_url")
        params["pr_url"] = pr_url
    if pr_number is not None:
        set_clauses.append("pr_number = :pr_number")
        params["pr_number"] = pr_number
    if branch_name is not None:
        set_clauses.append("branch_name = :branch_name")
        params["branch_name"] = branch_name
    if error is not None:
        set_clauses.append("error = :error")
        params["error"] = error
    if pending_questions is not None:
        set_clauses.append("pending_questions = :pending_questions")
        params["pending_questions"] = (
            json.dumps(pending_questions) if pending_questions else None
        )
    if plan_text is not None:
        set_clauses.append("plan_text = :plan_text")
        params["plan_text"] = plan_text
    if started_at is not None:
        set_clauses.append("started_at = :started_at")
        params["started_at"] = started_at
    if completed_at is not None:
        set_clauses.append("completed_at = :completed_at")
        params["completed_at"] = completed_at
    if result is not None:
        set_clauses.append("result = :result")
        params["result"] = json.dumps(result)
    if project_id is not None:
        set_clauses.append("project_id = :project_id")
        params["project_id"] = project_id
    if commit_sha is not None:
        set_clauses.append("commit_sha = :commit_sha")
        params["commit_sha"] = commit_sha
    if issue_etag is not None:
        set_clauses.append("issue_etag = :issue_etag")
        params["issue_etag"] = issue_etag
    if pr_etag is not None:
        set_clauses.append("pr_etag = :pr_etag")
        params["pr_etag"] = pr_etag

    if set_clauses:
        DBOS.sql_session.execute(
            text(
                f"UPDATE worker_tasks SET {', '.join(set_clauses)} WHERE id = :id"  # nosec B608
            ),
            params,
        )


@DBOS.transaction()
def update_task_etag(
    task_id: str,
    issue_etag: str | None = None,
    pr_etag: str | None = None,
) -> None:
    """Update task etag fields."""
    params: dict = {"id": task_id}
    set_clauses = []

    if issue_etag is not None:
        set_clauses.append("issue_etag = :issue_etag")
        params["issue_etag"] = issue_etag
    if pr_etag is not None:
        set_clauses.append("pr_etag = :pr_etag")
        params["pr_etag"] = pr_etag

    if set_clauses:
        DBOS.sql_session.execute(
            text(
                f"UPDATE worker_tasks SET {', '.join(set_clauses)} WHERE id = :id"  # nosec B608
            ),
            params,
        )


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

    # Parse pending_questions JSON
    pending_questions = None
    raw_questions = row.get("pending_questions")
    if raw_questions:
        parsed = _parse_json_field(raw_questions)
        if parsed and isinstance(parsed, list):
            pending_questions = [SessionQuestion(**q) for q in parsed]

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
        keywords=list(row["keywords"]) if row.get("keywords") else [],
        skip_plan=row.get("skip_plan", False),
        pending_questions=pending_questions,
        plan_text=row.get("plan_text"),
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
