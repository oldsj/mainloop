"""FastAPI application with DBOS durable workflows."""

import uuid
from fastapi import FastAPI, HTTPException, Header
from fastapi.middleware.cors import CORSMiddleware
from dbos import DBOS

from mainloop.config import settings
from mainloop.db import db
from mainloop.models import (
    ConversationListResponse,
    ConversationResponse,
    ChatRequest,
    ChatResponse,
)
from models import (
    MainThread,
    QueueItem,
    QueueItemResponse,
    WorkerTask,
    TaskStatus,
)
from pydantic import BaseModel
from typing import Any
from datetime import datetime

# Import DBOS config to initialize DBOS before defining workflows
from mainloop.workflows.dbos_config import dbos_config  # noqa: F401

# Import workflows so they are registered with DBOS
from mainloop.workflows.main_thread import (
    get_or_start_main_thread,
    send_user_message,
    send_queue_response,
)
from mainloop.workflows.worker import worker_task_workflow  # noqa: F401
from mainloop.services.chat_handler import process_message

app = FastAPI(
    title="Mainloop API",
    description="AI agent orchestrator API with durable workflows",
    version="0.2.0",
)

# CORS middleware
app.add_middleware(
    CORSMiddleware,
    allow_origins=[
        "http://localhost:3000",
        "http://localhost:3030",
        "https://mainloop.olds.network",
    ],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)


@app.on_event("startup")
async def startup_event():
    """Initialize database and DBOS on startup."""
    # Connect to PostgreSQL
    await db.connect()
    await db.ensure_tables_exist()

    # Launch DBOS
    DBOS.launch()


@app.on_event("shutdown")
async def shutdown_event():
    """Clean up on shutdown."""
    await db.disconnect()


def get_user_id_from_cf_header(cf_access_jwt_assertion: str | None = Header(None)) -> str:
    """Extract user ID from Cloudflare Access JWT header."""
    if not cf_access_jwt_assertion:
        return "local-dev-user"
    # TODO: Decode and verify CF Access JWT
    return "user-from-cf-jwt"


# ============= Health & Info =============


@app.get("/")
async def root():
    """Root endpoint."""
    return {"message": "Mainloop API", "version": "0.2.0"}


@app.get("/health")
async def health():
    """Health check endpoint."""
    return {"status": "healthy", "dbos": "active"}


# ============= Main Thread Endpoints =============


@app.post("/threads", response_model=MainThread)
async def create_or_get_thread(
    user_id: str = Header(alias="X-User-ID", default=None),
):
    """Get or create the user's main thread and start the workflow."""
    if not user_id:
        user_id = get_user_id_from_cf_header()

    # Start the main thread workflow (idempotent - uses user_id as workflow ID)
    workflow_id = get_or_start_main_thread(user_id)

    # Get the thread record
    thread = await db.get_main_thread_by_user(user_id)
    if not thread:
        # Workflow just started, create the record
        thread = MainThread(user_id=user_id, workflow_run_id=workflow_id)
        thread = await db.create_main_thread(thread)

    return thread


@app.get("/threads/{thread_id}", response_model=MainThread)
async def get_thread(thread_id: str):
    """Get a main thread by ID."""
    thread = await db.get_main_thread(thread_id)
    if not thread:
        raise HTTPException(status_code=404, detail="Thread not found")
    return thread


# ============= Chat Endpoints =============


@app.post("/chat", response_model=ChatResponse)
async def chat(
    request: ChatRequest,
    user_id: str = Header(alias="X-User-ID", default=None),
):
    """Send a message and get an immediate response."""
    from mainloop.services.compaction import trigger_compaction

    if not user_id:
        user_id = get_user_id_from_cf_header()

    # Ensure main thread is running (for background coordination)
    main_thread_id = get_or_start_main_thread(user_id)

    # Get or create conversation
    if request.conversation_id:
        conversation = await db.get_conversation(request.conversation_id)
        if not conversation:
            raise HTTPException(status_code=404, detail="Conversation not found")
    else:
        conversation = await db.create_conversation(user_id)

    # Load context: summary + recent messages after last summarized point
    recent_messages = await db.get_messages_after(
        conversation.id,
        conversation.summarized_through_id,
        limit=20,
    )

    # Save user message and increment count
    await db.create_message(
        conversation_id=conversation.id,
        role="user",
        content=request.message,
    )
    await db.increment_message_count(conversation.id)

    # Get main thread record for the ID
    main_thread = await db.get_main_thread_by_user(user_id)
    thread_id = main_thread.id if main_thread else main_thread_id

    # Process message with summary + recent messages for context
    result = await process_message(
        user_id=user_id,
        message=request.message,
        conversation_id=conversation.id,
        main_thread_id=thread_id,
        summary=conversation.summary,
        recent_messages=recent_messages,
    )

    # Save assistant response and increment count
    assistant_message = await db.create_message(
        conversation_id=conversation.id,
        role="assistant",
        content=result.response,
    )
    new_count = await db.increment_message_count(conversation.id)

    # Trigger async compaction if needed (fire-and-forget)
    trigger_compaction(conversation.id, new_count)

    return ChatResponse(
        conversation_id=conversation.id,
        message=assistant_message,
    )


# ============= Conversation Endpoints =============


@app.get("/conversations", response_model=ConversationListResponse)
async def list_conversations(
    user_id: str = Header(alias="X-User-ID", default=None),
):
    """List user's conversations."""
    if not user_id:
        user_id = get_user_id_from_cf_header()

    conversations = await db.list_conversations(user_id)
    return ConversationListResponse(
        conversations=conversations,
        total=len(conversations),
    )


@app.get("/conversations/{conversation_id}", response_model=ConversationResponse)
async def get_conversation(conversation_id: str):
    """Get a conversation with its messages."""
    conversation = await db.get_conversation(conversation_id)
    if not conversation:
        raise HTTPException(status_code=404, detail="Conversation not found")

    messages = await db.get_messages(conversation_id)
    return ConversationResponse(
        conversation=conversation,
        messages=messages,
    )


# ============= Queue/Inbox Endpoints =============


class UnreadCountResponse(BaseModel):
    """Unread count response."""

    count: int


@app.get("/queue", response_model=list[QueueItem])
async def list_queue_items(
    user_id: str = Header(alias="X-User-ID", default=None),
    status: str = "pending",
    unread_only: bool = False,
    task_id: str | None = None,
):
    """List queue items for the user.

    Args:
        status: Filter by status (default: "pending")
        unread_only: Only return unread items
        task_id: Filter by task ID
    """
    if not user_id:
        user_id = get_user_id_from_cf_header()

    items = await db.list_queue_items(
        user_id=user_id,
        status=status,
        unread_only=unread_only,
        task_id=task_id,
    )
    return items


@app.get("/queue/unread/count", response_model=UnreadCountResponse)
async def get_unread_count(
    user_id: str = Header(alias="X-User-ID", default=None),
):
    """Get the count of unread queue items (for inbox badge)."""
    if not user_id:
        user_id = get_user_id_from_cf_header()

    count = await db.count_unread_queue_items(user_id)
    return UnreadCountResponse(count=count)


@app.get("/queue/{item_id}", response_model=QueueItem)
async def get_queue_item(item_id: str):
    """Get a specific queue item."""
    item = await db.get_queue_item(item_id)
    if not item:
        raise HTTPException(status_code=404, detail="Queue item not found")
    return item


@app.post("/queue/{item_id}/read")
async def mark_queue_item_read(
    item_id: str,
    user_id: str = Header(alias="X-User-ID", default=None),
):
    """Mark a queue item as read."""
    if not user_id:
        user_id = get_user_id_from_cf_header()

    item = await db.get_queue_item(item_id)
    if not item:
        raise HTTPException(status_code=404, detail="Queue item not found")

    if item.user_id != user_id:
        raise HTTPException(status_code=403, detail="Not your queue item")

    await db.mark_queue_item_read(item_id)
    return {"status": "ok"}


@app.post("/queue/read-all")
async def mark_all_queue_items_read(
    user_id: str = Header(alias="X-User-ID", default=None),
):
    """Mark all queue items as read for the user."""
    if not user_id:
        user_id = get_user_id_from_cf_header()

    await db.mark_all_queue_items_read(user_id)
    return {"status": "ok"}


@app.post("/queue/{item_id}/respond")
async def respond_to_queue_item(
    item_id: str,
    response: QueueItemResponse,
    user_id: str = Header(alias="X-User-ID", default=None),
):
    """Respond to a queue item."""
    if not user_id:
        user_id = get_user_id_from_cf_header()

    item = await db.get_queue_item(item_id)
    if not item:
        raise HTTPException(status_code=404, detail="Queue item not found")

    if item.user_id != user_id:
        raise HTTPException(status_code=403, detail="Not your queue item")

    # Mark as read when responding
    await db.mark_queue_item_read(item_id)

    # Send response to the main thread workflow with full context
    from mainloop.workflows.main_thread import TOPIC_QUEUE_RESPONSE

    main_thread_workflow_id = f"main-thread-{user_id}"
    DBOS.send(
        main_thread_workflow_id,
        {
            "type": TOPIC_QUEUE_RESPONSE,
            "payload": {
                "queue_item_id": item_id,
                "response": response.response,
                "task_id": item.task_id,
                "context": item.context,
                "item_type": item.item_type.value if item.item_type else None,
            },
        },
    )

    return {"status": "ok", "message": "Response sent"}


# ============= Task Endpoints =============


@app.get("/tasks", response_model=list[WorkerTask])
async def list_tasks(
    user_id: str = Header(alias="X-User-ID", default=None),
    status: str | None = None,
):
    """List worker tasks for the user."""
    if not user_id:
        user_id = get_user_id_from_cf_header()

    tasks = await db.list_worker_tasks(user_id=user_id, status=status)
    return tasks


class TaskContext(BaseModel):
    """Full task context for pull-based retrieval."""

    task: WorkerTask
    queue_items: list[QueueItem]


@app.get("/tasks/{task_id}", response_model=WorkerTask)
async def get_task(task_id: str):
    """Get a specific worker task."""
    task = await db.get_worker_task(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="Task not found")
    return task


@app.get("/tasks/{task_id}/context", response_model=TaskContext)
async def get_task_context(
    task_id: str,
    user_id: str = Header(alias="X-User-ID", default=None),
):
    """Get full task context including queue items.

    This endpoint is for pull-based context retrieval when the main thread
    needs to know what's happening with a specific task.
    """
    if not user_id:
        user_id = get_user_id_from_cf_header()

    task = await db.get_worker_task(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="Task not found")

    if task.user_id != user_id:
        raise HTTPException(status_code=403, detail="Not your task")

    # Get all queue items for this task
    queue_items = await db.list_queue_items(user_id=user_id, task_id=task_id)

    return TaskContext(task=task, queue_items=queue_items)


class TaskLogsResponse(BaseModel):
    """Response for task logs endpoint."""

    logs: str
    source: str  # "k8s" or "none"
    task_status: str


@app.get("/tasks/{task_id}/logs", response_model=TaskLogsResponse)
async def get_task_logs(
    task_id: str,
    tail: int = 100,
    user_id: str = Header(alias="X-User-ID", default=None),
):
    """Get logs for a worker task from its K8s pod.

    Args:
        task_id: The task ID
        tail: Number of lines to return from the end (default 100)
    """
    if not user_id:
        user_id = get_user_id_from_cf_header()

    task = await db.get_worker_task(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="Task not found")

    if task.user_id != user_id:
        raise HTTPException(status_code=403, detail="Not your task")

    # Try to get live logs from K8s
    from mainloop.services.k8s_jobs import get_job_logs
    import logging

    logger = logging.getLogger(__name__)
    namespace = f"task-{task_id[:8]}"

    logs = None
    source = "k8s"

    try:
        logs = await get_job_logs(task_id, namespace)
    except Exception as e:
        logger.warning(f"Failed to get K8s logs for task {task_id}: {e}")

    if not logs:
        logs = ""
        source = "none"
    else:
        # Tail the logs to requested number of lines
        lines = logs.split("\n")
        if len(lines) > tail:
            lines = lines[-tail:]
        logs = "\n".join(lines)

    return TaskLogsResponse(
        logs=logs,
        source=source,
        task_status=task.status.value,
    )


@app.post("/tasks/{task_id}/cancel")
async def cancel_task(
    task_id: str,
    user_id: str = Header(alias="X-User-ID", default=None),
):
    """Cancel a running task."""
    if not user_id:
        user_id = get_user_id_from_cf_header()

    task = await db.get_worker_task(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="Task not found")

    if task.user_id != user_id:
        raise HTTPException(status_code=403, detail="Not your task")

    # Cancel the DBOS workflow
    DBOS.cancel_workflow(task_id)

    await db.update_worker_task(task_id, status=TaskStatus.CANCELLED)

    return {"status": "cancelled"}


# ============= Internal Endpoints (for K8s Jobs) =============


class TaskResult(BaseModel):
    """Result from a worker Job."""

    task_id: str
    status: str  # "completed" or "failed"
    result: dict[str, Any] | None = None
    error: str | None = None
    completed_at: str | None = None


@app.post("/internal/tasks/{task_id}/complete")
async def task_complete(task_id: str, result: TaskResult):
    """Callback endpoint for K8s Jobs to report completion.

    This is called by the job_runner when a worker Job finishes.
    It updates the task status and notifies the main thread workflow.
    """
    # Verify task exists
    task = await db.get_worker_task(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="Task not found")

    # Update task with job result
    # NOTE: "completed" here means the K8s job finished, NOT that the task is done.
    # The task is only truly completed when the PR is merged (handled by workflow).
    # We just store the URL/number here without changing status.
    if result.status == "completed":
        # Handle both issue URLs (plan phase) and PR URLs (implement phase)
        issue_url = result.result.get("issue_url") if result.result else None
        pr_url = result.result.get("pr_url") if result.result else None
        issue_number = None
        pr_number = None

        if issue_url:
            # Extract issue number from URL (e.g., https://github.com/owner/repo/issues/123)
            try:
                issue_number = int(issue_url.split("/")[-1])
            except (ValueError, IndexError):
                pass

        if pr_url:
            # Extract PR number from URL (e.g., https://github.com/owner/repo/pull/123)
            try:
                pr_number = int(pr_url.split("/")[-1])
            except (ValueError, IndexError):
                pass

        # Update task with URLs - don't mark COMPLETED, workflow manages status
        if issue_url or pr_url:
            await db.update_worker_task(
                task_id,
                result=result.result,
                issue_url=issue_url,
                issue_number=issue_number,
                pr_url=pr_url,
                pr_number=pr_number,
            )
    elif result.status == "failed":
        await db.update_worker_task(
            task_id,
            status=TaskStatus.FAILED,
            error=result.error,
        )

    # Send result to the worker workflow via DBOS.send()
    # The worker workflow is waiting on TOPIC_JOB_RESULT
    from mainloop.workflows.worker import TOPIC_JOB_RESULT

    # Send to the worker workflow (which uses task_id as workflow ID via the queue)
    # Actually, we need to send to the workflow that's waiting
    # The worker_task_workflow is running with the task_id as part of its workflow context
    # We use DBOS.send with the workflow_id to target it
    workflow_id = task_id  # The worker workflow uses task_id for idempotency

    DBOS.send(
        workflow_id,
        {
            "status": result.status,
            "result": result.result,
            "error": result.error,
        },
        topic=TOPIC_JOB_RESULT,
    )

    return {"status": "ok", "task_id": task_id}


# ============= Debug Endpoints =============


class DebugTaskInfo(BaseModel):
    """Debug info for a worker task including workflow state."""

    task: WorkerTask
    workflow_status: str | None = None
    workflow_error: str | None = None
    workflow_created_at: datetime | None = None
    workflow_updated_at: datetime | None = None
    namespace_exists: bool = False
    k8s_jobs: list[str] = []


@app.get("/debug/tasks", response_model=list[DebugTaskInfo])
async def debug_list_tasks(
    limit: int = 10,
):
    """List all tasks with debug info (no auth required for debugging)."""
    from datetime import timezone
    from mainloop.services.k8s_namespace import namespace_exists, get_k8s_client

    # Get all tasks (bypass user filter for debugging)
    async with db.connection() as conn:
        task_rows = await conn.fetch(
            """
            SELECT * FROM worker_tasks
            ORDER BY created_at DESC
            LIMIT $1
            """,
            limit,
        )

        # Get workflow status for each task
        results = []
        for task_row in task_rows:
            task = db._row_to_worker_task(task_row)

            # Get DBOS workflow status
            workflow_row = await conn.fetchrow(
                """
                SELECT status, error, created_at, updated_at
                FROM dbos.workflow_status
                WHERE workflow_uuid = $1
                """,
                task.id,
            )

            workflow_status = None
            workflow_error = None
            workflow_created_at = None
            workflow_updated_at = None

            if workflow_row:
                workflow_status = workflow_row["status"]
                # Just show raw error string (it's base64-encoded pickle, but we show it raw)
                if workflow_row["error"]:
                    workflow_error = f"[encoded] {workflow_row['error'][:200]}..."
                workflow_created_at = datetime.fromtimestamp(
                    workflow_row["created_at"] / 1000, tz=timezone.utc
                )
                workflow_updated_at = datetime.fromtimestamp(
                    workflow_row["updated_at"] / 1000, tz=timezone.utc
                )

            # Check if namespace exists
            ns_exists = False
            k8s_jobs = []
            try:
                ns_exists = await namespace_exists(task.id)
                if ns_exists:
                    _, batch_v1 = get_k8s_client()
                    namespace_name = f"task-{task.id[:8]}"
                    jobs = batch_v1.list_namespaced_job(namespace=namespace_name)
                    k8s_jobs = [j.metadata.name for j in jobs.items]
            except Exception:
                pass

            results.append(
                DebugTaskInfo(
                    task=task,
                    workflow_status=workflow_status,
                    workflow_error=workflow_error,
                    workflow_created_at=workflow_created_at,
                    workflow_updated_at=workflow_updated_at,
                    namespace_exists=ns_exists,
                    k8s_jobs=k8s_jobs,
                )
            )

        return results


@app.post("/tasks/{task_id}/retry")
async def retry_task(task_id: str):
    """Retry a failed task by resetting its status and re-enqueueing."""
    from dbos import SetWorkflowID
    from mainloop.workflows.dbos_config import worker_queue
    from mainloop.workflows.worker import worker_task_workflow

    task = await db.get_worker_task(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="Task not found")

    # Reset task status to pending
    await db.update_worker_task(task_id, status=TaskStatus.PENDING, error=None)

    # Delete the failed workflow from DBOS
    async with db.connection() as conn:
        await conn.execute(
            "DELETE FROM dbos.workflow_status WHERE workflow_uuid = $1",
            task_id,
        )

    # Re-enqueue via DBOS worker queue
    with SetWorkflowID(task_id):
        worker_queue.enqueue(worker_task_workflow, task_id)

    return {"status": "retried", "task_id": task_id}


@app.delete("/debug/tasks/{task_id}/namespace")
async def debug_delete_namespace(task_id: str):
    """Force delete a task namespace."""
    from mainloop.services.k8s_namespace import delete_task_namespace

    await delete_task_namespace(task_id)
    return {"status": "deleted", "namespace": f"task-{task_id[:8]}"}


# ============= Run =============


def run():
    """Run the application."""
    import uvicorn

    uvicorn.run(
        "mainloop.api:app",
        host=settings.host,
        port=settings.port,
        reload=True,
    )


if __name__ == "__main__":
    run()
