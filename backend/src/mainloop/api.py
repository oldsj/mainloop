"""FastAPI application with DBOS durable workflows."""

from datetime import datetime
from typing import Any

from dbos import DBOS
from fastapi import FastAPI, Header, HTTPException, Request
from fastapi.middleware.cors import CORSMiddleware
from mainloop.config import settings
from mainloop.db import db
from mainloop.models import (
    ChatRequest,
    ChatResponse,
    ConversationListResponse,
    ConversationResponse,
)
from mainloop.services.chat_handler import process_message
from mainloop.services.github_pr import (
    CommitSummary,
    ProjectPRSummary,
    add_issue_comment,
    get_repo_metadata,
    list_open_prs,
    list_recent_commits,
    update_github_issue,
)
from mainloop.sse import (
    create_sse_response,
    event_stream,
    notify_inbox_updated,
    notify_task_updated,
    task_log_stream,
)

# Import DBOS config to initialize DBOS before defining workflows
from mainloop.workflows.dbos_config import dbos_config  # noqa: F401

# Import workflows so they are registered with DBOS
from mainloop.workflows.main_thread import (
    get_or_start_main_thread,
)
from mainloop.workflows.worker import worker_task_workflow  # noqa: F401
from pydantic import BaseModel

from models import (
    MainThread,
    Project,
    QueueItem,
    QueueItemResponse,
    QueueItemType,
    TaskStatus,
    WorkerTask,
)

app = FastAPI(
    title="Mainloop API",
    description="AI agent orchestrator API with durable workflows",
    version="0.2.0",
)

# CORS middleware
app.add_middleware(
    CORSMiddleware,
    allow_origin_regex=r"http://localhost:\d+",  # All localhost ports
    allow_origins=[settings.frontend_origin],  # Production
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)


def _apply_mock_github():
    """Replace github_pr functions with mocks if USE_MOCK_GITHUB=true."""
    if not settings.use_mock_github:
        return

    import mainloop.services.github_pr as github_pr
    from mainloop.services import github_mock

    # Replace functions with mocks
    funcs_to_mock = [
        "create_github_issue",
        "update_github_issue",
        "add_issue_comment",
        "get_issue_status",
        "get_issue_comments",
        "get_comment_reactions",
        "get_pr_status",
        "get_pr_comments",
        "get_pr_reviews",
        "get_check_status",
        "get_check_failure_logs",
        "add_reaction_to_comment",
        "acknowledge_comments",
        "is_pr_merged",
        "is_pr_approved",
    ]
    for name in funcs_to_mock:
        if hasattr(github_mock, name):
            setattr(github_pr, name, getattr(github_mock, name))

    print("[startup] GitHub mock enabled - API calls will be simulated")


@app.on_event("startup")
async def startup_event():
    """Initialize database and DBOS on startup."""
    # Apply mocks before anything else
    _apply_mock_github()

    # Connect to PostgreSQL
    await db.connect()
    await db.ensure_tables_exist()

    # Launch DBOS
    DBOS.launch()


@app.on_event("shutdown")
async def shutdown_event():
    """Clean up on shutdown."""
    await db.disconnect()


def get_user_id_from_cf_header() -> str:
    """Get user ID - always returns local-dev-user for now."""
    return "local-dev-user"


# ============= Health & Info =============


@app.get("/")
async def root():
    """Root endpoint."""
    return {"message": "Mainloop API", "version": "0.2.0"}


@app.get("/health")
async def health():
    """Health check endpoint."""
    return {"status": "healthy", "dbos": "active"}


# ============= SSE Endpoints =============


@app.get("/events")
async def sse_events(
    request: Request,
    user_id: str = Header(alias="X-User-ID", default=None),
    user_id_query: str | None = None,
):
    """SSE endpoint for real-time updates.

    Streams events for:
    - task:updated - when a task status changes
    - inbox:updated - when inbox items change
    - heartbeat - periodic keepalive (every 30s)

    The client should reconnect automatically on disconnect.
    EventSource handles this natively.
    """
    if not user_id:
        if user_id_query and settings.is_test_env:
            user_id = user_id_query
        else:
            user_id = get_user_id_from_cf_header()

    return create_sse_response(event_stream(user_id, request))


@app.get("/tasks/{task_id}/logs/stream")
async def sse_task_logs(
    task_id: str,
    request: Request,
    user_id: str = Header(alias="X-User-ID", default=None),
):
    """SSE endpoint for streaming task logs.

    Streams events for:
    - log - new log lines from the K8s pod
    - status - task status changes
    - end - stream is ending (task completed/failed)

    Polls K8s logs every 2 seconds and streams new content.
    """
    if not user_id:
        user_id = get_user_id_from_cf_header()

    # Verify task exists and belongs to user
    task = await db.get_worker_task(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="Task not found")
    if task.user_id != user_id:
        raise HTTPException(status_code=403, detail="Not your task")

    return create_sse_response(task_log_stream(task_id, user_id, request))


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

    # Get or create main thread record
    main_thread = await db.get_main_thread_by_user(user_id)
    if not main_thread:
        # Create main thread record if it doesn't exist (e.g., after DB reset)
        from models import MainThread

        main_thread = MainThread(user_id=user_id, workflow_run_id=main_thread_id)
        main_thread = await db.create_main_thread(main_thread)
    thread_id = main_thread.id

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
        user_id: User ID from X-User-ID header
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

    # Notify SSE clients of unread count change
    unread_count = await db.count_unread_queue_items(user_id)
    await notify_inbox_updated(user_id, item_id=item_id, unread_count=unread_count)

    return {"status": "ok"}


@app.post("/queue/read-all")
async def mark_all_queue_items_read(
    user_id: str = Header(alias="X-User-ID", default=None),
):
    """Mark all queue items as read for the user."""
    if not user_id:
        user_id = get_user_id_from_cf_header()

    await db.mark_all_queue_items_read(user_id)

    # Notify SSE clients
    await notify_inbox_updated(user_id, unread_count=0)

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

    # Notify SSE clients
    unread_count = await db.count_unread_queue_items(user_id)
    await notify_inbox_updated(user_id, item_id=item_id, unread_count=unread_count)

    return {"status": "ok", "message": "Response sent"}


# ============= Project Endpoints =============


@app.get("/projects", response_model=list[Project])
async def list_projects(
    user_id: str = Header(alias="X-User-ID", default=None),
    limit: int = 20,
):
    """List user's projects ordered by last used."""
    if not user_id:
        user_id = get_user_id_from_cf_header()

    projects = await db.list_projects(user_id=user_id, limit=limit)
    return projects


@app.get("/projects/{project_id}", response_model=Project)
async def get_project(project_id: str):
    """Get a project by ID."""
    project = await db.get_project(project_id)
    if not project:
        raise HTTPException(status_code=404, detail="Project not found")
    return project


class ProjectDetail(BaseModel):
    """Detailed project info including GitHub data."""

    project: Project
    open_prs: list[ProjectPRSummary]
    recent_commits: list[CommitSummary]
    tasks: list[WorkerTask]


@app.get("/projects/{project_id}/detail", response_model=ProjectDetail)
async def get_project_detail(
    project_id: str,
    user_id: str = Header(alias="X-User-ID", default=None),
):
    """Get project with GitHub data (PRs, commits, tasks)."""
    if not user_id:
        user_id = get_user_id_from_cf_header()

    project = await db.get_project(project_id)
    if not project:
        raise HTTPException(status_code=404, detail="Project not found")

    # Fetch GitHub data in parallel
    open_prs = await list_open_prs(project.html_url, limit=10)
    recent_commits = await list_recent_commits(
        project.html_url, branch=project.default_branch, limit=10
    )

    # Fetch tasks for this project
    tasks = await db.list_worker_tasks(user_id=user_id, project_id=project_id, limit=50)

    return ProjectDetail(
        project=project,
        open_prs=open_prs,
        recent_commits=recent_commits,
        tasks=tasks,
    )


@app.post("/projects/{project_id}/refresh")
async def refresh_project_metadata(project_id: str):
    """Force refresh GitHub metadata for a project."""
    project = await db.get_project(project_id)
    if not project:
        raise HTTPException(status_code=404, detail="Project not found")

    # Fetch fresh metadata from GitHub
    metadata = await get_repo_metadata(project.html_url)
    if metadata:
        await db.update_project_metadata(
            project_id,
            description=metadata.description,
            avatar_url=metadata.avatar_url,
            open_issue_count=metadata.open_issues_count,
        )

    return {"status": "ok", "message": "Project metadata refreshed"}


# ============= Task Endpoints =============


@app.get("/tasks", response_model=list[WorkerTask])
async def list_tasks(
    user_id: str = Header(alias="X-User-ID", default=None),
    status: str | None = None,
    project_id: str | None = None,
):
    """List worker tasks for the user, optionally filtered by project."""
    if not user_id:
        user_id = get_user_id_from_cf_header()

    tasks = await db.list_worker_tasks(
        user_id=user_id, status=status, project_id=project_id
    )
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
        user_id: User ID from X-User-ID header

    """
    if not user_id:
        user_id = get_user_id_from_cf_header()

    task = await db.get_worker_task(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="Task not found")

    if task.user_id != user_id:
        raise HTTPException(status_code=403, detail="Not your task")

    # Try to get live logs from K8s
    import logging

    from mainloop.services.k8s_jobs import get_job_logs

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
    """Cancel a running task and close associated GitHub issue/PR."""
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

    # Close GitHub issue if exists
    if task.repo_url and task.issue_number:
        await add_issue_comment(
            task.repo_url, task.issue_number, "❌ Task cancelled by user."
        )
        await update_github_issue(task.repo_url, task.issue_number, state="closed")

    # Close GitHub PR if exists (PRs are also issues in GitHub API)
    if task.repo_url and task.pr_number and task.pr_number != task.issue_number:
        await add_issue_comment(
            task.repo_url, task.pr_number, "❌ Task cancelled by user."
        )
        await update_github_issue(task.repo_url, task.pr_number, state="closed")

    # Notify SSE clients
    await notify_task_updated(user_id, task_id, "cancelled")

    return {"status": "cancelled"}


class AnswerQuestionsRequest(BaseModel):
    """Request to answer task questions."""

    answers: dict[str, str]  # question_id -> answer text
    action: str = "answer"  # "answer" or "cancel"


@app.post("/tasks/{task_id}/answer-questions")
async def answer_task_questions(
    task_id: str,
    body: AnswerQuestionsRequest,
    user_id: str = Header(alias="X-User-ID", default=None),
):
    """Answer questions asked by an agent during planning.

    The agent can ask questions via AskUserQuestion tool during plan mode.
    These questions are stored on the task and surfaced in the UI.
    This endpoint sends the user's answers back to the waiting worker workflow.
    """
    if not user_id:
        user_id = get_user_id_from_cf_header()

    task = await db.get_worker_task(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="Task not found")

    if task.user_id != user_id:
        raise HTTPException(status_code=403, detail="Not your task")

    if task.status != TaskStatus.WAITING_QUESTIONS:
        raise HTTPException(
            status_code=400,
            detail=f"Task is not waiting for questions (status: {task.status})",
        )

    # Send answers to the worker workflow
    from dbos import error as dbos_error
    from mainloop.workflows.worker import TOPIC_QUESTION_RESPONSE

    # Try to send to worker workflow, but continue if workflow doesn't exist (e.g., in tests)
    try:
        DBOS.send(
            task_id,  # Worker workflow ID is the task ID
            {
                "action": body.action,
                "answers": body.answers,
            },
            topic=TOPIC_QUESTION_RESPONSE,
        )
    except dbos_error.DBOSNonExistentWorkflowError:
        # Workflow doesn't exist (e.g., test environment) - that's okay, continue with status update
        pass

    # Update task status immediately so frontend sees correct state on refetch
    # The workflow will also update this, but we do it here to prevent race conditions
    if body.action == "cancel":
        await db.update_worker_task(
            task_id, status=TaskStatus.CANCELLED, pending_questions=[]
        )
        # Close GitHub issue if exists
        if task.repo_url and task.issue_number:
            await add_issue_comment(
                task.repo_url, task.issue_number, "❌ Task cancelled by user."
            )
            await update_github_issue(task.repo_url, task.issue_number, state="closed")
        await notify_task_updated(user_id, task_id, "cancelled")
    else:
        await db.update_worker_task(
            task_id, status=TaskStatus.PLANNING, pending_questions=[]
        )
        await notify_task_updated(user_id, task_id, "planning")

    return {"status": "ok", "message": f"Sent {len(body.answers)} answer(s) to task"}


@app.post("/tasks/{task_id}/approve-plan")
async def approve_task_plan(
    task_id: str,
    action: str = "approve",
    revision_text: str | None = None,
    user_id: str = Header(alias="X-User-ID", default=None),
):
    """Approve or revise a plan shown in the task UI.

    This replaces the queue-item-based plan approval for the embedded task UI flow.
    """
    if not user_id:
        user_id = get_user_id_from_cf_header()

    task = await db.get_worker_task(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="Task not found")

    if task.user_id != user_id:
        raise HTTPException(status_code=403, detail="Not your task")

    if task.status != TaskStatus.WAITING_PLAN_REVIEW:
        raise HTTPException(
            status_code=400,
            detail=f"Task is not waiting for plan review (status: {task.status})",
        )

    # Send response to the worker workflow
    from mainloop.workflows.worker import TOPIC_PLAN_RESPONSE

    DBOS.send(
        task_id,
        {
            "action": action,  # "approve", "cancel", or revision text
            "text": revision_text or "",
        },
        topic=TOPIC_PLAN_RESPONSE,
    )

    # Update task status immediately so frontend sees correct state on refetch
    if action == "cancel":
        await db.update_worker_task(
            task_id, status=TaskStatus.CANCELLED, plan_text=None
        )
        # Close GitHub issue if exists
        if task.repo_url and task.issue_number:
            await add_issue_comment(
                task.repo_url, task.issue_number, "❌ Task cancelled by user."
            )
            await update_github_issue(task.repo_url, task.issue_number, state="closed")
        await notify_task_updated(user_id, task_id, "cancelled")
    elif action == "approve":
        await db.update_worker_task(task_id, status=TaskStatus.READY_TO_IMPLEMENT)
        await notify_task_updated(user_id, task_id, "ready_to_implement")
    else:
        # Revision - back to planning
        await db.update_worker_task(task_id, status=TaskStatus.PLANNING)
        await notify_task_updated(user_id, task_id, "planning")

    return {"status": "ok", "action": action}


@app.post("/tasks/{task_id}/start-implementation")
async def start_task_implementation(
    task_id: str,
    user_id: str = Header(alias="X-User-ID", default=None),
):
    """Start implementation of an approved plan.

    This triggers the worker to proceed from ready_to_implement to implementing.
    """
    if not user_id:
        user_id = get_user_id_from_cf_header()

    task = await db.get_worker_task(task_id)
    if not task:
        raise HTTPException(status_code=404, detail="Task not found")

    if task.user_id != user_id:
        raise HTTPException(status_code=403, detail="Not your task")

    if task.status != TaskStatus.READY_TO_IMPLEMENT:
        raise HTTPException(
            status_code=400,
            detail=f"Task is not ready for implementation (status: {task.status})",
        )

    # Send implementation trigger to the worker workflow
    from mainloop.workflows.worker import TOPIC_START_IMPLEMENTATION

    DBOS.send(
        task_id,
        {"action": "start"},
        topic=TOPIC_START_IMPLEMENTATION,
    )

    # Update task status immediately so frontend sees correct state on refetch
    await db.update_worker_task(task_id, status=TaskStatus.IMPLEMENTING)

    # Notify SSE clients
    await notify_task_updated(user_id, task_id, "implementing")

    return {"status": "ok", "message": "Implementation started"}


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
    """Handle K8s Job completion callbacks.

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

    from mainloop.services.k8s_namespace import get_k8s_client, namespace_exists

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
                pass  # nosec B110

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


# ============= Test Helpers (E2E only) =============


class SeedTaskRequest(BaseModel):
    """Request to seed a task for testing."""

    status: TaskStatus
    task_type: str = "feature"
    description: str = "Test task"
    repo_url: str | None = None
    plan: str | None = None
    questions: list[dict] | None = None  # For waiting_questions status


@app.post("/internal/test/seed-task")
async def seed_task_for_testing(
    request: SeedTaskRequest,
    user_id: str = Header(alias="X-User-ID", default=None),
):
    """Create a task in a specific state for E2E testing.

    WARNING: Only available in test environments. Do not use in production.
    """
    if not settings.is_test_env:
        raise HTTPException(
            status_code=403, detail="Only available in test environment"
        )

    from uuid import uuid4

    # Get or create a test main thread
    if not user_id:
        user_id = get_user_id_from_cf_header()
    thread = await db.get_main_thread_by_user(user_id)
    if not thread:
        # Create test thread directly (no workflow needed for tests)
        thread = MainThread(user_id=user_id, workflow_run_id="test-workflow")
        thread = await db.create_main_thread(thread)
    thread_id = thread.id

    # Create task
    # Convert questions dict to TaskQuestion models if provided
    pending_questions = None
    if request.questions:
        from models.workflow import QuestionOption, TaskQuestion

        pending_questions = [
            TaskQuestion(
                id=q["id"],
                header=q.get(
                    "header", q["question"][:30]
                ),  # Default header from question
                question=q["question"],
                options=[
                    QuestionOption(
                        label=opt["label"], description=opt.get("description")
                    )
                    for opt in q.get("options", [])
                ],
                multi_select=q.get("multi_select", False),
                response=None,
            )
            for q in request.questions
        ]
        print(f"[DEBUG] Created {len(pending_questions)} pending_questions")
        print(f"[DEBUG] pending_questions: {pending_questions}")

    task = WorkerTask(
        id=str(uuid4()),
        main_thread_id=thread_id,
        user_id=user_id,
        task_type=request.task_type,
        description=request.description,
        prompt=f"Implement: {request.description}",
        repo_url=request.repo_url,
        status=request.status,
        plan_text=request.plan,  # Store plan on task for UI display
        pending_questions=pending_questions,  # Store questions on task for UI
        created_at=datetime.now(),
        updated_at=datetime.now(),
    )

    await db.create_worker_task(task)

    # If task needs plan review, create a queue item
    if request.status == TaskStatus.WAITING_PLAN_REVIEW and request.plan:
        queue_item = QueueItem(
            id=str(uuid4()),
            main_thread_id=thread_id,
            task_id=task.id,
            user_id=user_id,
            item_type=QueueItemType.PLAN_REVIEW,
            title="Review Plan",
            content=request.plan,
            created_at=datetime.now(),
        )
        await db.create_queue_item(queue_item)

    # If task has questions, create a queue item
    if request.status == TaskStatus.WAITING_QUESTIONS and request.questions:
        import json

        queue_item = QueueItem(
            id=str(uuid4()),
            main_thread_id=thread_id,
            task_id=task.id,
            user_id=user_id,
            item_type=QueueItemType.QUESTION,
            title="Answer Questions",
            content=json.dumps(request.questions),  # Serialize questions to string
            created_at=datetime.now(),
        )
        await db.create_queue_item(queue_item)

    return {"task_id": task.id, "status": task.status}


@app.post("/internal/test/reset")
async def reset_test_data():
    """Clear all data for fresh test runs.

    WARNING: Only available in test environments. Do not use in production.
    """
    if not settings.is_test_env:
        raise HTTPException(
            status_code=403, detail="Only available in test environment"
        )

    # Truncate all app tables (CASCADE handles foreign keys)
    async with db.connection() as conn:
        await conn.execute(
            """
            TRUNCATE TABLE
                queue_items, messages, worker_tasks, projects,
                conversations, main_threads, planning_sessions
            CASCADE
        """
        )
        # Clear DBOS workflow state
        await conn.execute(
            "TRUNCATE TABLE dbos.workflow_events, dbos.operation_outputs, dbos.workflow_status CASCADE"
        )

    # Reset mock state if mocking is enabled
    if settings.use_mock_github:
        from mainloop.services.github_mock import mock_state

        mock_state.reset()

    return {"status": "reset"}


class SeedRepoRequest(BaseModel):
    """Request to seed a fixture repo for testing."""

    owner: str = "test"
    name: str = "fixture-repo"
    files: dict[str, str] | None = None  # filename -> content


@app.post("/internal/test/seed-repo")
async def seed_repo_for_testing(request: SeedRepoRequest):
    """Create a fixture git repo in the cache for E2E testing.

    This creates a real git repo with the specified files, allowing
    planning tests to work without network access to GitHub.

    WARNING: Only available in test environments. Do not use in production.
    """
    if not settings.is_test_env:
        raise HTTPException(
            status_code=403, detail="Only available in test environment"
        )

    import subprocess
    from pathlib import Path

    from mainloop.services.repo_cache import get_repo_cache

    repo_cache = get_repo_cache()
    repo_path = repo_cache.cache_path / request.owner / request.name

    # Clean up existing repo if present
    if repo_path.exists():
        import shutil

        shutil.rmtree(repo_path)

    # Create repo directory
    repo_path.mkdir(parents=True, exist_ok=True)

    # Default test files if none provided
    files = request.files or {
        "README.md": "# Test Fixture Repo\n\nThis is a test repository for E2E testing.\n",
        "src/main.py": '''"""Main application module."""


def hello(name: str = "World") -> str:
    """Return a greeting message."""
    return f"Hello, {name}!"


def add(a: int, b: int) -> int:
    """Add two numbers."""
    return a + b


if __name__ == "__main__":
    print(hello())
''',
        "src/utils.py": '''"""Utility functions."""


def format_name(first: str, last: str) -> str:
    """Format a full name."""
    return f"{first} {last}"


def validate_email(email: str) -> bool:
    """Basic email validation."""
    return "@" in email and "." in email
''',
        "tests/test_main.py": '''"""Tests for main module."""

from src.main import hello, add


def test_hello():
    assert hello() == "Hello, World!"
    assert hello("Test") == "Hello, Test!"


def test_add():
    assert add(1, 2) == 3
    assert add(-1, 1) == 0
''',
        "pyproject.toml": '''[project]
name = "fixture-repo"
version = "0.1.0"
description = "Test fixture repository"
requires-python = ">=3.10"

[project.optional-dependencies]
dev = ["pytest"]
''',
    }

    # Write files
    for filepath, content in files.items():
        file_path = repo_path / filepath
        file_path.parent.mkdir(parents=True, exist_ok=True)
        file_path.write_text(content)

    # Initialize git repo
    subprocess.run(["git", "init"], cwd=repo_path, capture_output=True, check=True)
    subprocess.run(
        ["git", "config", "user.email", "test@example.com"],
        cwd=repo_path,
        capture_output=True,
        check=True,
    )
    subprocess.run(
        ["git", "config", "user.name", "Test User"],
        cwd=repo_path,
        capture_output=True,
        check=True,
    )
    subprocess.run(["git", "add", "."], cwd=repo_path, capture_output=True, check=True)
    subprocess.run(
        ["git", "commit", "-m", "Initial commit"],
        cwd=repo_path,
        capture_output=True,
        check=True,
    )
    # Add a dummy remote so pull operations don't fail
    # (the repo cache tries to pull on existing repos)
    dummy_remote = f"https://github.com/{request.owner}/{request.name}.git"
    subprocess.run(
        ["git", "remote", "add", "origin", dummy_remote],
        cwd=repo_path,
        capture_output=True,
        check=True,
    )

    return {
        "status": "created",
        "repo_url": f"https://github.com/{request.owner}/{request.name}",
        "path": str(repo_path),
        "files": list(files.keys()),
    }


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
