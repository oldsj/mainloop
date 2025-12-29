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
    """Send a message through the main thread workflow."""
    if not user_id:
        user_id = get_user_id_from_cf_header()

    # Ensure main thread is running
    get_or_start_main_thread(user_id)

    # Get or create conversation
    if request.conversation_id:
        conversation = await db.get_conversation(request.conversation_id)
        if not conversation:
            raise HTTPException(status_code=404, detail="Conversation not found")
    else:
        conversation = await db.create_conversation(user_id)

    # Save user message
    user_message = await db.create_message(
        conversation_id=conversation.id,
        role="user",
        content=request.message,
    )

    # Send message to the main thread workflow
    send_user_message(user_id, request.message, conversation.id)

    return ChatResponse(
        conversation_id=conversation.id,
        message=user_message,
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


# ============= Queue Endpoints =============


@app.get("/queue", response_model=list[QueueItem])
async def list_queue_items(
    user_id: str = Header(alias="X-User-ID", default=None),
    status: str = "pending",
):
    """List queue items for the user."""
    if not user_id:
        user_id = get_user_id_from_cf_header()

    items = await db.list_queue_items(user_id=user_id, status=status)
    return items


@app.get("/queue/{item_id}", response_model=QueueItem)
async def get_queue_item(item_id: str):
    """Get a specific queue item."""
    item = await db.get_queue_item(item_id)
    if not item:
        raise HTTPException(status_code=404, detail="Queue item not found")
    return item


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

    # Send response to the main thread workflow
    send_queue_response(
        user_id=user_id,
        queue_item_id=item_id,
        response=response.response,
        task_id=item.task_id,
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


@app.get("/tasks/{task_id}", response_model=WorkerTask)
async def get_task(task_id: str):
    """Get a specific worker task."""
    task = await db.get_worker_task(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="Task not found")
    return task


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

    # Update task status in database
    if result.status == "completed":
        await db.update_worker_task(
            task_id,
            status=TaskStatus.COMPLETED,
            result=result.result,
            pr_url=result.result.get("pr_url") if result.result else None,
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
