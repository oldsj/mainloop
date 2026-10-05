"""FastAPI application with DBOS durable workflows."""

import logging
import re
from dataclasses import asdict
from datetime import datetime
from urllib.parse import urlsplit

from dbos import DBOS
from fastapi import Depends, FastAPI, HTTPException, Request
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import JSONResponse
from mainloop.config import settings
from mainloop.db import db
from mainloop.identity import current_user
from mainloop.models import (
    ChatRequest,
    ChatResponse,
    ConversationListResponse,
    ConversationResponse,
)
from mainloop.runtime.preview_proxy import register_preview_proxy
from mainloop.runtime.workspace_api import router as workspace_api_router
from mainloop.services.github_pr import (
    CommitSummary,
    ProjectPRSummary,
    get_repo_metadata,
    list_open_prs,
    list_recent_commits,
)
from mainloop.sse import (
    create_sse_response,
    event_stream,
    notify_inbox_updated,
)

# Import DBOS config to initialize DBOS before defining workflows
from mainloop.workflows.dbos_config import dbos_config  # noqa: F401

# Import workflows so they are registered with DBOS
from mainloop.workflows.main_thread import (
    get_or_start_main_thread,
)
from pydantic import BaseModel

from models import (
    MainThread,
    NativeSessionInfo,
    Project,
    QueueItem,
    QueueItemResponse,
    Session,
    SessionCreate,
    SessionNotification,
    SessionStatus,
)

logger = logging.getLogger(__name__)

_SAFE_METHODS = frozenset({"GET", "HEAD", "OPTIONS"})

app = FastAPI(
    title="Mainloop API",
    description="AI agent orchestrator API with durable workflows",
    version="0.2.0",
)

_LOCALHOST_ORIGIN = re.compile(r"http://localhost:\d+")
_LOOPBACK_HOSTS = frozenset({"localhost", "127.0.0.1", "::1"})
# Kubernetes probes address the pod by IP, so the health check is exempt from the Host check.
_HOST_EXEMPT_PATHS = frozenset({"/health"})

# CORS middleware
app.add_middleware(
    CORSMiddleware,
    allow_origin_regex=_LOCALHOST_ORIGIN.pattern,  # All localhost ports
    allow_origins=[settings.frontend_origin],  # Production
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)


def _origin_allowed(origin: str, host: str) -> bool:
    return (
        origin == settings.frontend_origin
        or _LOCALHOST_ORIGIN.fullmatch(origin) is not None
        or urlsplit(origin).netloc == host  # the API's own origin
    )


def _host_allowed(host_header: str) -> bool:
    try:
        hostname = urlsplit(f"//{host_header}").hostname
    except ValueError:
        return False
    if not hostname:
        return False
    hostname = hostname.lower().rstrip(".")
    return (
        settings.is_dev and hostname in _LOOPBACK_HOSTS
    ) or hostname in settings.allowed_api_hosts


@app.middleware("http")
async def refuse_unknown_hosts(request: Request, call_next):
    """Return 404 for a Host that is neither the API's nor a preview host.

    Preview hosts are served (or refused) by the preview middleware, which wraps this one, so a
    request that reaches here with a preview-shaped Host is not a live preview. Without this
    check any name that resolves to the node (a wildcard DNS name, a rebinding name) would reach
    the API, and the preview origins are same-site with it.
    """
    if request.scope["path"] not in _HOST_EXEMPT_PATHS and not _host_allowed(
        request.headers.get("host", "")
    ):
        return JSONResponse({"detail": "Not Found"}, status_code=404)
    return await call_next(request)


@app.middleware("http")
async def refuse_foreign_origin_writes(request: Request, call_next):
    """Reject a browser write that a page on another origin sent.

    CORS only stops a page from reading the response. A body-less ``no-cors`` POST from the
    agent's own preview page (another origin, and the agent writes that page) is still sent, and
    the API has no per-request authentication, so it would cancel, archive or suspend. Browsers
    always send ``Origin`` on a cross-origin write; a client that sends none (the MCP actors,
    scripts, curl) is not a browser page and is not affected. The preview listener is handled
    before this middleware and applies its own Origin check (``preview_proxy._origin_refused``):
    a preview accepts a write or WebSocket only from its own origin.
    """
    origin = request.headers.get("origin")
    if (
        origin is not None
        and request.method not in _SAFE_METHODS
        and not _origin_allowed(origin, request.headers.get("host", ""))
    ):
        return JSONResponse(
            {"detail": "Cross-origin request refused."}, status_code=403
        )
    return await call_next(request)


def _apply_mock_github():
    """Replace github_pr functions with mocks if USE_MOCK_GITHUB=true."""
    if not settings.use_mock_github:
        return

    from mainloop.services import github_mock, github_pr

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
    from mainloop.runtime.agent_identity import require_token_key

    # Fail before touching anything when agent tokens could not be issued.
    require_token_key()

    # Apply mocks before anything else
    _apply_mock_github()

    # Connect to PostgreSQL
    await db.connect()
    await db.ensure_tables_exist()

    # Launch DBOS
    DBOS.launch()

    import asyncio

    from mainloop.runtime import native_sessions

    app.state.native_reconcile = asyncio.create_task(native_sessions.reconcile_loop())


@app.on_event("shutdown")
async def shutdown_event():
    """Clean up on shutdown."""
    import asyncio

    task = getattr(app.state, "native_reconcile", None)
    if task is not None:
        task.cancel()
        await asyncio.gather(task, return_exceptions=True)
    from mainloop.runtime import native_sessions

    await native_sessions.close_client()
    await db.disconnect()


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
    user_id: str = Depends(current_user),
):
    """SSE endpoint for real-time updates.

    Streams events for:
    - inbox:updated - when inbox items change
    - heartbeat - periodic keepalive (every 30s)

    The client should reconnect automatically on disconnect.
    EventSource handles this natively.
    """
    return create_sse_response(event_stream(user_id, request))


# ============= Main Thread Endpoints =============


@app.post("/threads", response_model=MainThread)
async def create_or_get_thread(
    user_id: str = Depends(current_user),
):
    """Get or create the user's main thread and start the workflow."""
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
async def get_thread(thread_id: str, user_id: str = Depends(current_user)):
    """Get a main thread by ID."""
    thread = await db.get_main_thread(thread_id)
    if not thread or thread.user_id != user_id:
        raise HTTPException(status_code=404, detail="Thread not found")
    return thread


# ============= Chat Endpoints =============


@app.post("/chat", response_model=ChatResponse)
async def chat(
    request: ChatRequest,
    user_id: str = Depends(current_user),
):
    """Record and deliver a message to the user's native main session."""
    return await _chat_native(request, user_id)


async def _chat_native(request: ChatRequest, user_id: str) -> ChatResponse:
    """Record and deliver to the native main session through kagent. The reply is mirrored from
    the A2A task, so the client polls the conversation.
    """
    from mainloop.runtime import delegation, native_sessions

    binding = await delegation.ensure_main_session(user_id)
    session = await db.get_session(binding["session_id"])
    try:
        message_id = await native_sessions.submit_message(
            binding["session_id"], request.message
        )
    except ValueError as exc:
        raise HTTPException(status_code=409, detail=str(exc)) from exc
    return ChatResponse(
        conversation_id=session.conversation_id,
        pending=True,
        delivery_message_id=message_id,
    )


class MainThreadInfo(BaseModel):
    mode: str
    session_id: str | None = None
    conversation_id: str | None = None
    native: NativeSessionInfo | None = None
    topics: list[dict] = []


@app.get("/main-thread", response_model=MainThreadInfo)
async def get_main_thread_info(user_id: str = Depends(current_user)):
    """Main-thread mode, native identity strip, and the topic index."""
    from mainloop.runtime import delegation, native_sessions

    binding = await delegation.ensure_main_session(user_id)
    await native_sessions.sync(binding["session_id"])
    session = await db.get_session(binding["session_id"])
    topics = await delegation._topic_lines(user_id)
    return MainThreadInfo(
        mode="native",
        session_id=binding["session_id"],
        conversation_id=session.conversation_id,
        native=await native_sessions.identity(binding["session_id"]),
        topics=[asdict(t) for t in topics],
    )


@app.get("/topics")
async def list_topics(user_id: str = Depends(current_user)):
    """Topic index with records (notes, decisions, pending intent, reports) for the UI."""
    async with db.connection() as conn:
        topics = await conn.fetch(
            "SELECT * FROM topics WHERE user_id=$1 ORDER BY updated_at DESC", user_id
        )
        out = []
        for t in topics:
            recs = await conn.fetch(
                "SELECT id, kind, text, status, session_id, created_at FROM topic_records WHERE topic_id=$1 ORDER BY created_at DESC LIMIT 50",
                t["id"],
            )
            out.append(
                {
                    "id": t["id"],
                    "name": t["name"],
                    "status_line": t["status_line"],
                    "records": [dict(r) for r in recs],
                }
            )
    return out


app.include_router(workspace_api_router)
register_preview_proxy(app)


# ============= Conversation Endpoints =============


@app.get("/conversations", response_model=ConversationListResponse)
async def list_conversations(
    user_id: str = Depends(current_user),
):
    """List user's conversations."""
    conversations = await db.list_conversations(user_id)
    return ConversationListResponse(
        conversations=conversations,
        total=len(conversations),
    )


@app.get("/conversations/{conversation_id}", response_model=ConversationResponse)
async def get_conversation(conversation_id: str, user_id: str = Depends(current_user)):
    """Get a conversation with its messages."""
    conversation = await db.get_conversation(conversation_id)
    if not conversation or conversation.user_id != user_id:
        raise HTTPException(status_code=404, detail="Conversation not found")

    from mainloop.runtime import native_sessions

    async with db.connection() as conn:
        main_sid = await conn.fetchval(
            """SELECT b.session_id FROM native_bindings b JOIN sessions s ON s.id=b.session_id
               WHERE b.role='main' AND s.conversation_id=$1""",
            conversation_id,
        )
    if main_sid:
        await native_sessions.sync(main_sid)

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
    user_id: str = Depends(current_user),
    status: str = "pending",
    unread_only: bool = False,
    task_id: str | None = None,
):
    """List queue items for the user.

    Args:
        user_id: The owner
        status: Filter by status (default: "pending")
        unread_only: Only return unread items
        task_id: Filter by task ID

    """
    items = await db.list_queue_items(
        user_id=user_id,
        status=status,
        unread_only=unread_only,
        task_id=task_id,
    )
    return items


@app.get("/queue/unread/count", response_model=UnreadCountResponse)
async def get_unread_count(
    user_id: str = Depends(current_user),
):
    """Get the count of unread queue items (for inbox badge)."""
    count = await db.count_unread_queue_items(user_id)
    return UnreadCountResponse(count=count)


@app.get("/queue/{item_id}", response_model=QueueItem)
async def get_queue_item(item_id: str, user_id: str = Depends(current_user)):
    """Get a specific queue item."""
    item = await db.get_queue_item(item_id)
    if not item or item.user_id != user_id:
        raise HTTPException(status_code=404, detail="Queue item not found")
    return item


@app.post("/queue/{item_id}/read")
async def mark_queue_item_read(
    item_id: str,
    user_id: str = Depends(current_user),
):
    """Mark a queue item as read."""
    item = await db.get_queue_item(item_id)
    if not item:
        raise HTTPException(status_code=404, detail="Queue item not found")

    if item.user_id != user_id:
        raise HTTPException(status_code=404, detail="Queue item not found")

    await db.mark_queue_item_read(item_id)

    # Notify SSE clients of unread count change
    unread_count = await db.count_unread_queue_items(user_id)
    await notify_inbox_updated(user_id, item_id=item_id, unread_count=unread_count)

    return {"status": "ok"}


@app.post("/queue/read-all")
async def mark_all_queue_items_read(
    user_id: str = Depends(current_user),
):
    """Mark all queue items as read for the user."""
    await db.mark_all_queue_items_read(user_id)

    # Notify SSE clients
    await notify_inbox_updated(user_id, unread_count=0)

    return {"status": "ok"}


@app.post("/queue/{item_id}/respond")
async def respond_to_queue_item(
    item_id: str,
    response: QueueItemResponse,
    user_id: str = Depends(current_user),
):
    """Respond to a queue item."""
    item = await db.get_queue_item(item_id)
    if not item:
        raise HTTPException(status_code=404, detail="Queue item not found")

    if item.user_id != user_id:
        raise HTTPException(status_code=404, detail="Queue item not found")

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
    user_id: str = Depends(current_user),
    limit: int = 20,
):
    """List user's projects ordered by last used."""
    projects = await db.list_projects(user_id=user_id, limit=limit)
    return projects


@app.get("/projects/{project_id}", response_model=Project)
async def get_project(project_id: str, user_id: str = Depends(current_user)):
    """Get a project by ID."""
    project = await db.get_project(project_id)
    if not project or project.user_id != user_id:
        raise HTTPException(status_code=404, detail="Project not found")
    return project


class ProjectDetail(BaseModel):
    """Detailed project info including GitHub data."""

    project: Project
    open_prs: list[ProjectPRSummary]
    recent_commits: list[CommitSummary]
    sessions: list[Session]


@app.get("/projects/{project_id}/detail", response_model=ProjectDetail)
async def get_project_detail(
    project_id: str,
    user_id: str = Depends(current_user),
):
    """Get project with GitHub data (PRs, commits, sessions)."""
    project = await db.get_project(project_id)
    if not project or project.user_id != user_id:
        raise HTTPException(status_code=404, detail="Project not found")

    # Fetch GitHub data in parallel
    open_prs = await list_open_prs(project.html_url, limit=10)
    recent_commits = await list_recent_commits(
        project.html_url, branch=project.default_branch, limit=10
    )

    # Fetch sessions for this project
    sessions = await db.list_sessions(user_id=user_id, project_id=project_id, limit=50)

    return ProjectDetail(
        project=project,
        open_prs=open_prs,
        recent_commits=recent_commits,
        sessions=sessions,
    )


@app.post("/projects/{project_id}/refresh")
async def refresh_project_metadata(
    project_id: str, user_id: str = Depends(current_user)
):
    """Force refresh GitHub metadata for a project."""
    project = await db.get_project(project_id)
    if not project or project.user_id != user_id:
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


# ============= Session Endpoints =============


@app.get("/sessions", response_model=list[Session])
async def list_sessions(
    user_id: str = Depends(current_user),
    status: str | None = None,
):
    """List user's sessions."""
    session_status = SessionStatus(status) if status else None
    sessions = await db.list_sessions(user_id=user_id, status=session_status)
    if sessions:
        async with db.connection() as conn:
            rows = await conn.fetch(
                """SELECT b.session_id, b.parent_session_id, t.name AS topic FROM native_bindings b
                   LEFT JOIN topics t ON t.id=b.topic_id WHERE b.session_id = ANY($1)""",
                [s.id for s in sessions],
            )
        info = {r["session_id"]: r for r in rows}
        for s in sessions:
            if s.id in info:
                s.parent_session_id = info[s.id]["parent_session_id"]
                s.topic = info[s.id]["topic"]
    return sessions


# Color palette for session assignment - uses CSS variables for theme compatibility
SESSION_COLORS = [
    "var(--term-cyan)",
    "var(--term-green)",
    "var(--term-orange)",
    "var(--term-purple)",
    "var(--term-magenta)",
    "var(--term-yellow)",
]


async def _get_next_session_color(user_id: str) -> str:
    """Get the next color for a session based on existing session count."""
    existing_sessions = await db.list_sessions(user_id, limit=100)
    return SESSION_COLORS[len(existing_sessions) % len(SESSION_COLORS)]


@app.post("/sessions", response_model=Session)
async def create_session(
    request: SessionCreate,
    user_id: str = Depends(current_user),
):
    """Create a new session with its own conversation."""
    import uuid

    # Ensure main thread exists
    main_thread_id = get_or_start_main_thread(user_id)
    main_thread = await db.get_main_thread_by_user(user_id)
    if not main_thread:
        main_thread = MainThread(user_id=user_id, workflow_run_id=main_thread_id)
        main_thread = await db.create_main_thread(main_thread)

    # Create conversation for this session
    conversation = await db.create_conversation(user_id, title=request.title)

    # Assign color automatically
    color = await _get_next_session_color(user_id)

    # Create session
    session = Session(
        id=str(uuid.uuid4()),
        user_id=user_id,
        main_thread_id=main_thread.id,
        title=request.title,
        description=request.description,
        prompt=request.prompt,
        conversation_id=conversation.id,
        status=SessionStatus.PENDING,
        anchor_message_id=request.anchor_message_id,
        color=color,
    )
    session = await db.create_session(session)

    from mainloop.runtime import native_sessions

    await native_sessions.create_binding(session.id, request.agent_kind or "claude")
    await db.update_session(session.id, status=SessionStatus.ACTIVE)
    await native_sessions.submit_message(session.id, request.prompt)
    return await db.get_session(session.id)


@app.get("/sessions/{session_id}", response_model=Session)
async def get_session(session_id: str, user_id: str = Depends(current_user)):
    """Get a session by ID."""
    session = await db.get_session(session_id)
    if not session or session.user_id != user_id:
        raise HTTPException(status_code=404, detail="Session not found")
    return session


class SessionConversationResponse(BaseModel):
    """Response for session conversation."""

    session: Session
    messages: list


@app.get(
    "/sessions/{session_id}/conversation", response_model=SessionConversationResponse
)
async def get_session_conversation(
    session_id: str, user_id: str = Depends(current_user)
):
    """Get a session's conversation messages."""
    session = await db.get_session(session_id)
    if not session or session.user_id != user_id:
        raise HTTPException(status_code=404, detail="Session not found")

    from mainloop.runtime import native_sessions

    if await native_sessions.get_binding(session_id):
        await native_sessions.sync(session_id)  # observe the A2A task first
        session = await db.get_session(session_id)

    messages = await db.get_messages(session.conversation_id)
    return SessionConversationResponse(session=session, messages=messages)


@app.get("/sessions/{session_id}/native", response_model=NativeSessionInfo)
async def get_session_native(session_id: str, user_id: str = Depends(current_user)):
    """Identity strip for a session bound to a native agent in kagent."""
    owner = await db.get_session(session_id)
    if owner is None or owner.user_id != user_id:
        raise HTTPException(status_code=404, detail="Session not found")
    from mainloop.runtime import native_sessions

    info = await native_sessions.identity(session_id)
    if info is None:
        raise HTTPException(
            status_code=404, detail="Session has no native agent binding"
        )
    return info


class SessionMessageRequest(BaseModel):
    """Request to send a message to a session."""

    message: str


@app.post("/sessions/{session_id}/message")
async def send_session_message(
    session_id: str,
    request: SessionMessageRequest,
    user_id: str = Depends(current_user),
):
    """Send a message to a session's conversation."""

    session = await db.get_session(session_id)
    if not session:
        raise HTTPException(status_code=404, detail="Session not found")

    if session.user_id != user_id:
        raise HTTPException(status_code=404, detail="Session not found")

    from mainloop.runtime import native_sessions

    if not await native_sessions.get_binding(session_id):
        raise HTTPException(
            status_code=409, detail="Session has no native agent binding"
        )
    try:
        message_id = await native_sessions.submit_message(session_id, request.message)
    except ValueError as exc:
        raise HTTPException(status_code=409, detail=str(exc)) from exc
    return {"status": "ok", "message_id": message_id}


# Done: nothing more will run, so the session can be cleared from the list.
FINISHED_STATUSES = frozenset(
    {SessionStatus.COMPLETED, SessionStatus.FAILED, SessionStatus.CANCELLED}
)


@app.post("/sessions/{session_id}/cancel")
async def cancel_session(
    session_id: str,
    user_id: str = Depends(current_user),
):
    """Cancel a running session."""
    session = await db.get_session(session_id)
    if not session:
        raise HTTPException(status_code=404, detail="Session not found")

    if session.user_id != user_id:
        raise HTTPException(status_code=404, detail="Session not found")

    if session.status in FINISHED_STATUSES:
        return {"status": session.status.value, "agent": "not_running"}

    from mainloop.runtime import native_sessions

    if not await native_sessions.get_binding(session_id):
        raise HTTPException(
            status_code=409, detail="Session has no native agent binding"
        )
    try:
        agent = await native_sessions.cancel(session_id)
    except ValueError as exc:
        raise HTTPException(status_code=409, detail=str(exc)) from exc

    return {"status": "cancelled", "agent": agent}


@app.post("/sessions/{session_id}/stop-turn")
async def stop_session_turn(
    session_id: str,
    user_id: str = Depends(current_user),
):
    """Stop the open turn of a session (the main thread included) without ending the session.

    ``status`` is ``stopped``, ``finished`` (the turn ended on its own first) or ``no_open_turn``.
    A kagent error is returned as 502 and leaves the delivery as it was.
    """
    session = await db.get_session(session_id)
    if not session:
        raise HTTPException(status_code=404, detail="Session not found")

    if session.user_id != user_id:
        raise HTTPException(status_code=404, detail="Session not found")

    from mainloop.runtime import native_sessions
    from mainloop.runtime.kagent_client import KagentError

    if not await native_sessions.get_binding(session_id):
        raise HTTPException(
            status_code=409, detail="Session has no native agent binding"
        )
    try:
        status = await native_sessions.stop_turn(session_id)
    except native_sessions.StopUnconfirmed as exc:
        raise HTTPException(status_code=409, detail=str(exc)) from exc
    except KagentError as exc:
        raise HTTPException(
            status_code=502, detail=f"Could not stop the turn: {exc}"
        ) from exc

    return {"status": status}


@app.post("/sessions/archive-finished")
async def archive_finished_sessions(
    user_id: str = Depends(current_user),
):
    """Clear every finished session (done, failed, cancelled) from the list.

    Rows are kept for audit; sessions still running or waiting are left alone.
    """
    archived = await db.archive_sessions(user_id)
    return {"archived": archived}


@app.post("/sessions/{session_id}/archive")
async def archive_session(
    session_id: str,
    user_id: str = Depends(current_user),
):
    """Clear one finished session from the list. A live one must be cancelled first."""
    session = await db.get_session(session_id)
    if not session:
        raise HTTPException(status_code=404, detail="Session not found")
    if session.user_id != user_id:
        raise HTTPException(status_code=404, detail="Session not found")
    if session.status not in FINISHED_STATUSES:
        raise HTTPException(
            status_code=409,
            detail="This session is still running or waiting; cancel it first.",
        )

    await db.archive_sessions(user_id, session_ids=[session_id])
    return {"status": "archived"}


# ============= Notification Endpoints =============


@app.get("/notifications", response_model=list[SessionNotification])
async def list_notifications(
    user_id: str = Depends(current_user),
    unread_only: bool = True,
):
    """List session notifications for the user."""
    notifications = await db.list_session_notifications(
        user_id=user_id, unread_only=unread_only
    )
    return notifications


@app.post("/notifications/{notification_id}/dismiss")
async def dismiss_notification(
    notification_id: str,
    user_id: str = Depends(current_user),
):
    """Dismiss (delete) a notification."""
    if not await db.dismiss_session_notification(notification_id, user_id):
        raise HTTPException(status_code=404, detail="Notification not found")
    return {"status": "ok"}


# ============= Test Helpers (E2E only) =============


class SeedSessionRequest(BaseModel):
    """Request to seed a session for testing."""

    status: SessionStatus = SessionStatus.ACTIVE
    title: str = "Test Session"
    description: str = "Test session description"
    prompt: str = "Test prompt"
    summary: str | None = None
    error: str | None = None
    create_notification: bool = False
    notification_title: str = "Session needs input"
    notification_preview: str = "Please provide additional details"


@app.post("/internal/test/seed-session")
async def seed_session_for_testing(
    request: SeedSessionRequest,
    user_id: str = Depends(current_user),
):
    """Create a session in a specific state for E2E testing.

    WARNING: Only available in test environments. Do not use in production.
    """
    if not settings.is_test_env:
        raise HTTPException(
            status_code=403, detail="Only available in test environment"
        )

    from uuid import uuid4

    # Get or create a test main thread
    thread = await db.get_main_thread_by_user(user_id)
    if not thread:
        # Create test thread directly (no workflow needed for tests)
        thread = MainThread(user_id=user_id, workflow_run_id="test-workflow")
        thread = await db.create_main_thread(thread)
    thread_id = thread.id

    # Create conversation for the session
    conversation = await db.create_conversation(user_id, title=request.title)

    # Assign color automatically
    color = await _get_next_session_color(user_id)

    # Create session
    session = Session(
        id=str(uuid4()),
        user_id=user_id,
        main_thread_id=thread_id,
        title=request.title,
        description=request.description,
        prompt=request.prompt,
        conversation_id=conversation.id,
        status=request.status,
        summary=request.summary,
        error=request.error,
        color=color,
        created_at=datetime.now(),
        started_at=datetime.now() if request.status != SessionStatus.PENDING else None,
        completed_at=(
            datetime.now()
            if request.status in [SessionStatus.COMPLETED, SessionStatus.FAILED]
            else None
        ),
    )
    session = await db.create_session(session)

    # Optionally create a notification
    notification_id = None
    if request.create_notification:
        notification = SessionNotification(
            id=str(uuid4()),
            session_id=session.id,
            user_id=user_id,
            title=request.notification_title,
            preview=request.notification_preview,
            read=False,
            created_at=datetime.now(),
        )
        await db.create_session_notification(notification)
        notification_id = notification.id

    return {
        "session_id": session.id,
        "conversation_id": conversation.id,
        "notification_id": notification_id,
        "status": session.status,
    }


@app.post("/internal/test/reset")
async def reset_test_data(all: bool = False):
    """Clear test data, optionally including dev user data.

    Args:
        all: If True, clears ALL data (dev + test). If False (default),
             only clears test user data (user_id LIKE 'test-%').

    WARNING: Only available in test environments. Do not use in production.

    """
    if not settings.is_test_env:
        raise HTTPException(
            status_code=403, detail="Only available in test environment"
        )

    if all:
        # Full reset - truncate everything
        async with db.connection() as conn:
            await conn.execute(
                """
                TRUNCATE TABLE
                    queue_items, messages, session_notifications, sessions,
                    projects, conversations, main_threads
                CASCADE
                """
            )
            await conn.execute(
                "TRUNCATE TABLE dbos.workflow_events, dbos.operation_outputs, dbos.workflow_status CASCADE"
            )

        if settings.use_mock_github:
            from mainloop.services.github_mock import mock_state

            mock_state.reset()

        return {
            "status": "reset",
            "scope": "all",
        }

    # Test-only reset
    async with db.connection() as conn:
        # Get workflow IDs for test users before deleting.
        test_workflow_ids = await conn.fetch(
            """
            SELECT workflow_run_id FROM main_threads
            WHERE user_id LIKE 'test-%' AND workflow_run_id IS NOT NULL
            """
        )
        workflow_ids = [row["workflow_run_id"] for row in test_workflow_ids]

        # Delete app data for test users only (order matters for foreign keys)
        await conn.execute("DELETE FROM queue_items WHERE user_id LIKE 'test-%'")
        await conn.execute(
            "DELETE FROM session_notifications WHERE user_id LIKE 'test-%'"
        )
        # Sessions must be deleted before messages (sessions.anchor_message_id -> messages.id)
        await conn.execute("DELETE FROM sessions WHERE user_id LIKE 'test-%'")
        await conn.execute(
            "DELETE FROM messages WHERE conversation_id IN (SELECT id FROM conversations WHERE user_id LIKE 'test-%')"
        )
        await conn.execute("DELETE FROM projects WHERE user_id LIKE 'test-%'")
        await conn.execute("DELETE FROM conversations WHERE user_id LIKE 'test-%'")
        await conn.execute("DELETE FROM main_threads WHERE user_id LIKE 'test-%'")

        # Clear DBOS workflow state for test user workflows
        if workflow_ids:
            await conn.execute(
                """
                DELETE FROM dbos.workflow_events
                WHERE workflow_uuid = ANY($1::text[])
                """,
                workflow_ids,
            )
            await conn.execute(
                """
                DELETE FROM dbos.operation_outputs
                WHERE workflow_uuid = ANY($1::text[])
                """,
                workflow_ids,
            )
            await conn.execute(
                """
                DELETE FROM dbos.workflow_status
                WHERE workflow_uuid = ANY($1::text[])
                """,
                workflow_ids,
            )

    # Reset mock state if mocking is enabled
    if settings.use_mock_github:
        from mainloop.services.github_mock import mock_state

        mock_state.reset()

    return {
        "status": "reset",
        "preserved": "non-test users",
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
