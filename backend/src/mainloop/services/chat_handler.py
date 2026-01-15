"""Synchronous chat handler - processes messages and returns immediate responses."""

import logging
from dataclasses import dataclass
from typing import Any, AsyncIterator

from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    ResultMessage,
    SystemMessage,
    TextBlock,
    create_sdk_mcp_server,
    query,
    tool,
)
from dbos import SetWorkflowID
from mainloop.config import settings
from mainloop.db import db
from mainloop.workflows.dbos_config import worker_queue

from models import (
    Message,
    QueueItem,
    Session,
    SessionStatus,
)

logger = logging.getLogger(__name__)


def build_chat_system_prompt(recent_repos: list[str] | None = None) -> str:
    """Build the system prompt for chat, including recent repos if available."""
    base_prompt = """You are a helpful AI assistant that can spawn background sessions to work on tasks independently.

## spawn_session
Use spawn_session when the user requests work that should run in the background.

When to use spawn_session WITH repo_url (for code work):
- Creating, modifying, or deleting code files
- Making commits or pull requests
- Running builds, tests, or deployments
- Any work that requires access to a codebase

When to use spawn_session WITHOUT repo_url (for other background work):
- Research tasks that take time
- Analysis or investigation work
- Planning or brainstorming that needs multiple steps
- Any work that can run in the background

Usage:
1. For code work: suggest a recent repo or ask for the GitHub repository URL
2. Always get explicit confirmation before spawning a session
3. Call spawn_session with just a title (and repo_url for code work)
   - The session automatically receives the user's original request from the conversation

Do NOT use spawn_session for:
- Answering simple questions
- Explaining concepts or providing information
- General conversation you can handle directly"""

    if recent_repos:
        repos_list = "\n".join(f"  - {repo}" for repo in recent_repos)
        base_prompt += f"""

The user has recently worked with these repositories:
{repos_list}

If the request involves code work, suggest using one of these repos. For example:
"I can spawn a session to work on this. Should I use {recent_repos[0]}?"
"""
    else:
        base_prompt += """

If the request involves code work, ask for the GitHub repo URL like:
"I can spawn a session to work on this. Would you like me to proceed? Please provide the GitHub repo URL."
"""

    return base_prompt


def create_spawn_session_callable(
    user_id: str,
    main_thread_id: str,
    conversation_id: str,
    spawned_session_ids: list[str],  # Mutable list to track spawned sessions
):
    """Create a raw spawn_session callable for Claude to use.

    Sessions are unified background work - they can be simple Claude conversations
    or code work with GitHub integration.
    """

    async def spawn_session_impl(args: dict[str, Any]) -> dict[str, Any]:
        """Spawn a background session to work on a task independently."""
        print(f"[SESSION] spawn_session_impl called with args: {args}")
        title = args.get("title", "")
        repo_url = args.get("repo_url")  # Optional - if provided, this is code work
        request_message_id = args.get(
            "request_message_id"
        )  # ID of the user's original request

        # Fetch the original request message from DB
        anchor_message_id = None
        prompt = ""
        try:
            if request_message_id:
                # Claude told us which message contains the request - fetch it
                request_msg = await db.get_message(request_message_id)
                if request_msg and request_msg.role == "user":
                    anchor_message_id = request_msg.id
                    prompt = request_msg.content
                    print(
                        f"[SESSION] Using specified message {request_message_id}: {prompt[:100]}..."
                    )

            # Fallback: use last user message if no ID provided or not found
            if not prompt:
                conv_messages = await db.get_messages(conversation_id)
                if conv_messages:
                    for msg in reversed(conv_messages):
                        if msg.role == "user":
                            anchor_message_id = msg.id
                            prompt = msg.content
                            break
                print(f"[SESSION] Fallback to last user message: {prompt[:100]}...")
        except Exception as e:
            print(f"[SESSION] Warning: Could not get message: {e}")

        if not title:
            return {
                "content": [{"type": "text", "text": "Error: title is required"}],
                "is_error": True,
            }

        if not prompt:
            return {
                "content": [
                    {
                        "type": "text",
                        "text": "Error: Could not find a user message in the conversation to use as the session prompt.",
                    }
                ],
                "is_error": True,
            }

        # Use title as description
        description = title

        # Validate repo URL format if provided
        if repo_url and not repo_url.startswith("https://github.com/"):
            return {
                "content": [
                    {
                        "type": "text",
                        "text": f"Error: Invalid repo URL format. Expected https://github.com/owner/repo, got: {repo_url}",
                    }
                ],
                "is_error": True,
            }

        try:
            is_code_work = repo_url is not None
            print(f"[SESSION] Creating session: {title} (code_work={is_code_work})")

            # Create conversation for this session
            conv = await db.create_conversation(user_id, title=title)
            print(f"[SESSION] Created conversation: {conv.id}")

            # Create project from repo URL if provided (so it shows in sidebar)
            project_id = None
            if repo_url:
                print(f"[SESSION] Creating project for repo: {repo_url}")
                project = await db.get_or_create_project_from_url(user_id, repo_url)
                project_id = project.id
                print(
                    f"[SESSION] Project created/found: {project.id} - {project.full_name}"
                )
                # Record this repo as recently used
                await db.add_recent_repo(main_thread_id, repo_url)

            # Create session anchored to user's message
            from mainloop.api import _get_next_session_color

            color = await _get_next_session_color(user_id)
            session = Session(
                user_id=user_id,
                main_thread_id=main_thread_id,
                title=title,
                description=description or title,
                prompt=prompt,
                conversation_id=conv.id,
                status=SessionStatus.PENDING,
                anchor_message_id=anchor_message_id,
                color=color,
                # Code work fields (optional)
                repo_url=repo_url,
                project_id=project_id,
            )
            session = await db.create_session(session)
            print(f"[SESSION] Session saved to DB: {session.id}")

            # Track this session for anchor update after assistant message is saved
            spawned_session_ids.append(session.id)

            # Start the session workflow
            from mainloop.workflows.session_worker import session_worker_workflow

            print(f"[SESSION] Enqueueing workflow for session {session.id}")
            with SetWorkflowID(session.id):
                handle = worker_queue.enqueue(session_worker_workflow, session.id)
                print(f"[SESSION] Workflow enqueued, handle: {handle}")

            logger.info(
                f"Spawned session via tool: {session.id} (code_work={is_code_work})"
            )

            response_text = (
                f"Session started successfully!\n"
                f"Session ID: {session.id[:8]}\n"
                f"Title: {title}\n"
            )
            if repo_url:
                response_text += f"Repository: {repo_url}\n"
            response_text += (
                "\nThe session is now running in the background. "
                "It will appear in the user's Sessions panel."
            )

            return {
                "content": [
                    {
                        "type": "text",
                        "text": response_text,
                    }
                ]
            }
        except Exception as e:
            import traceback

            print(f"[SESSION] ERROR: {e}")
            print(f"[SESSION] Traceback: {traceback.format_exc()}")
            logger.error(f"Failed to spawn session: {e}")
            return {
                "content": [
                    {"type": "text", "text": f"Error spawning session: {str(e)}"}
                ],
                "is_error": True,
            }

    return spawn_session_impl


def create_spawn_session_tool(
    user_id: str,
    main_thread_id: str,
    conversation_id: str,
    spawned_session_ids: list[str],  # Mutable list to track spawned sessions
):
    """Create a spawn_session tool with context baked in.

    This factory creates a tool that has access to the current user/conversation context.
    Returns an SdkMcpTool for use with Claude Agent SDK.
    """
    # Get the raw callable
    spawn_session_impl = create_spawn_session_callable(
        user_id, main_thread_id, conversation_id, spawned_session_ids
    )

    # Wrap it with the @tool decorator for Claude
    @tool(
        "spawn_session",
        "Spawn a background session to work on the user's request. "
        "Use this for: (1) code work - provide repo_url for GitHub integration, "
        "(2) research/analysis - omit repo_url for general background work. "
        "IMPORTANT: Pass the request_message_id from the conversation history [ID: ...] "
        "that contains the user's actual request (not a confirmation like 'yes').",
        {
            "title": str,  # Short title for the session (e.g., "Add quickstart to README")
            "request_message_id": str,  # ID from conversation [ID: ...] with the user's request
            "repo_url": str,  # Optional - if provided, enables code work with GitHub
        },
    )
    async def spawn_session(args: dict[str, Any]) -> dict[str, Any]:
        return await spawn_session_impl(args)

    return spawn_session


def format_conversation_history(messages: list[Message]) -> str:
    """Format conversation history for inclusion in prompt.

    Includes message IDs so Claude can reference them when spawning sessions.
    """
    if not messages:
        return ""

    lines = []
    for msg in messages:
        role = "User" if msg.role == "user" else "Assistant"
        lines.append(f"[ID: {msg.id}] {role}: {msg.content}")

    return "\n\n".join(lines)


def build_context_prompt(
    summary: str | None,
    recent_messages: list[Message],
    new_message: str,
) -> str:
    """Build the full prompt with summary and recent messages.

    Structure:
    1. Summary of earlier conversation (if exists)
    2. Recent messages (unsummarized)
    3. New user message
    """
    parts = []

    if summary:
        parts.append(f"[Summary of earlier conversation]\n{summary}")

    if recent_messages:
        history = format_conversation_history(recent_messages)
        parts.append(f"[Recent conversation]\n{history}")

    parts.append(f"User: {new_message}")

    if parts:
        context = "\n\n".join(parts)
        return f"""Continue this conversation naturally, taking into account the full context above.

{context}

Respond to the user's latest message."""
    else:
        return new_message


@dataclass
class ChatResult:
    """Result of processing a chat message."""

    response: str
    task_id: str | None = None
    needs_inbox_action: bool = False
    queue_item: QueueItem | None = None
    spawned_session_ids: list[str] | None = None  # Sessions created during this turn
    suppress_response: bool = False  # Don't save assistant message to main thread


@dataclass
class ClaudeResponse:
    """Response from Claude."""

    text: str
    compacted: bool = False
    compaction_count: int = 0
    spawned_session_ids: list[str] | None = None  # Sessions created during this turn


def _create_message_generator(prompt_text: str) -> AsyncIterator[dict]:
    """Create an async generator that yields the user message.

    This is required when using MCP servers with Claude Agent SDK.
    The SDK requires an async iterable for streaming input when MCP tools are configured.
    """

    async def generator():
        yield {
            "type": "user",
            "message": {
                "role": "user",
                "content": prompt_text,
            },
        }

    return generator()


async def get_claude_response(
    message: str,
    summary: str | None = None,
    recent_messages: list[Message] | None = None,
    model: str = "sonnet",
    user_id: str | None = None,
    main_thread_id: str | None = None,
    conversation_id: str | None = None,
) -> ClaudeResponse:
    """Get a response from Claude with conversation context and spawn_task tool.

    Context is provided via:
    - summary: Compacted summary of older messages (from PostgreSQL)
    - recent_messages: Recent unsummarized messages (from PostgreSQL)

    If user_id, main_thread_id, and conversation_id are provided, Claude
    will have access to the spawn_task tool to spawn autonomous worker agents.

    This ensures continuity across sessions, pod restarts, and deployments.
    """
    try:
        # Build prompt with summary and recent messages
        prompt_text = build_context_prompt(summary, recent_messages or [], message)

        # Create MCP server with spawn_task tool if context is provided
        mcp_servers = {}
        allowed_tools = []
        system_prompt = None
        spawned_session_ids: list[str] = []  # Track sessions created during this turn

        if user_id and main_thread_id and conversation_id:
            spawn_session_tool = create_spawn_session_tool(
                user_id, main_thread_id, conversation_id, spawned_session_ids
            )
            mcp_server = create_sdk_mcp_server(
                name="mainloop",
                version="1.0.0",
                tools=[spawn_session_tool],
            )
            mcp_servers["mainloop"] = mcp_server
            allowed_tools.append("mcp__mainloop__spawn_session")

            # Fetch recent repos for system prompt
            recent_repos = await db.get_recent_repos(main_thread_id)
            system_prompt = build_chat_system_prompt(recent_repos)

        options = ClaudeAgentOptions(
            model=model,
            permission_mode="bypassPermissions",
            system_prompt=system_prompt,
            mcp_servers=mcp_servers if mcp_servers else None,
            allowed_tools=allowed_tools if allowed_tools else None,
        )

        print(
            f"[CLAUDE] query - model={model}, mcp_servers={list(mcp_servers.keys()) if mcp_servers else None}, "
            f"allowed_tools={allowed_tools}"
        )

        # CRITICAL: When using MCP servers, must use async generator for prompt.
        # This is a Claude Agent SDK requirement - string prompts fail with
        # "ProcessTransport is not ready for writing" error.
        if mcp_servers:
            prompt = _create_message_generator(prompt_text)
        else:
            prompt = prompt_text

        collected_text = []
        compaction_count: int = 0

        async for msg in query(prompt=prompt, options=options):
            if isinstance(msg, AssistantMessage):
                for block in msg.content:
                    if isinstance(block, TextBlock):
                        collected_text.append(block.text)
            elif isinstance(msg, ResultMessage):
                if msg.is_error:
                    return ClaudeResponse(
                        text=f"Sorry, I encountered an error: {msg.result or 'Unknown error'}",
                        compacted=compaction_count > 0,
                        compaction_count=compaction_count,
                        spawned_session_ids=(
                            spawned_session_ids if spawned_session_ids else None
                        ),
                    )
            elif isinstance(msg, SystemMessage):
                # Track compaction events (context was automatically summarized)
                if msg.subtype == "compact_boundary":
                    compaction_count += 1
                    data = msg.data or {}
                    pre_tokens = data.get("pre_tokens", 0)
                    trigger = data.get("trigger", "unknown")
                    logger.info(
                        f"Context compacted ({trigger}): {pre_tokens} tokens summarized"
                    )

        return ClaudeResponse(
            text=(
                "\n".join(collected_text)
                if collected_text
                else "No response generated."
            ),
            compacted=compaction_count > 0,
            compaction_count=compaction_count,
            spawned_session_ids=spawned_session_ids if spawned_session_ids else None,
        )
    except Exception as e:
        logger.error(f"Claude Agent SDK error: {e}")
        return ClaudeResponse(
            text=f"Sorry, I encountered an error: {str(e)}",
            spawned_session_ids=spawned_session_ids if spawned_session_ids else None,
        )


async def process_message(
    user_id: str,
    message: str,
    conversation_id: str,
    main_thread_id: str,
    summary: str | None = None,
    recent_messages: list[Message] | None = None,
) -> ChatResult:
    """Process a user message and return an immediate response.

    Claude has access to spawn_session tool to create background sessions
    when the user wants to start tasks.

    Args:
        user_id: The user's unique identifier.
        message: The user's message text.
        conversation_id: The conversation's unique identifier.
        main_thread_id: The main thread workflow ID.
        summary: Compacted summary of older messages.
        recent_messages: Recent unsummarized messages for context.

    """
    # Get Claude response with spawn_session tool available
    # Claude will naturally decide when to ask for confirmation and spawn sessions
    model = settings.claude_model  # Uses haiku by default
    claude_response = await get_claude_response(
        message,
        summary=summary,
        recent_messages=recent_messages,
        model=model,
        user_id=user_id,
        main_thread_id=main_thread_id,
        conversation_id=conversation_id,
    )

    # If sessions were spawned, suppress the main thread response
    # The user interacts with the session directly
    suppress = bool(claude_response.spawned_session_ids)

    return ChatResult(
        response=claude_response.text,
        spawned_session_ids=claude_response.spawned_session_ids,
        suppress_response=suppress,
    )
