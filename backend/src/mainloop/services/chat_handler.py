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
from mainloop.config import settings
from mainloop.db import db
from mainloop.services.planning import (
    approve_plan,
    cancel_planning,
)
from mainloop.services.task_router import (
    find_matching_tasks,
)

from models import (
    Message,
    QueueItem,
    QueueItemPriority,
    QueueItemType,
    WorkerTask,
)

logger = logging.getLogger(__name__)


def build_chat_system_prompt(
    recent_repos: list[str] | None = None,
    planning_active: bool = False,
) -> str:
    """Build the system prompt for chat, including recent repos if available."""
    if planning_active:
        # During active planning, use a focused prompt that directs Claude to explore
        return """You are in planning mode, helping design an implementation approach.

IMPORTANT: You have read-only access to the codebase via these tools:
- Read: Read file contents
- Glob: Find files by pattern (e.g., "**/*.py", "src/**/*.ts")
- Grep: Search file contents with regex
- LS: List directory contents

ALWAYS explore the codebase first using these tools before asking clarifying questions.
Start by examining the project structure with Glob or LS, then Read relevant files.

When you've finished exploring and understand the codebase, present your plan:
1. **Summary**: Brief overview of the approach
2. **Files to Modify**: List of files that need changes
3. **Implementation Steps**: Numbered steps to complete the task
4. **Considerations**: Any trade-offs, risks, or alternatives

After presenting the plan, ask the user if they want to:
- Approve the plan (use approve_plan tool)
- Request changes (continue refining)
- Cancel planning (use cancel_planning tool)
"""

    base_prompt = """You are an AI assistant in a chat interface with background workers.

RESPOND DIRECTLY (no tools) for:
- Questions & answers ("How does X work?")
- Clarifying context ("What repo?" / "Which file?")
- Planning discussion ("Here's my approach...")
- Quick confirmations ("Should I use TypeScript?")

USE start_planning TOOL when user wants CODE CHANGES:
- Implementation tasks ("Add dark mode", "Fix the login bug")
- PR creation, commits, file modifications
- Long-running work (refactors, migrations)

The tool spawns a background worker. You'll explore the codebase, propose a plan,
and after approval the worker implements autonomously while the user continues chatting.

Be concise. If a task needs a repo URL and you don't have one, ask briefly: "Which repo?"
"""

    if recent_repos:
        repos_list = "\n".join(f"  - {repo}" for repo in recent_repos)
        base_prompt += f"""
Recent repositories:
{repos_list}
"""

    return base_prompt


def create_planning_tools(
    user_id: str,
    main_thread_id: str,
    conversation_id: str,
):
    """Create planning tools with context baked in.

    Returns a list of tools for starting, approving, and cancelling planning.
    """

    @tool(
        "start_planning",
        "Start a background task for code changes. "
        "This creates a thread under the user's message - DO NOT respond after calling this. "
        "The thread handles all progress display.",
        {
            "repo_url": str,
            "task_description": str,
        },
    )
    async def start_planning_tool(args: dict[str, Any]) -> dict[str, Any]:
        """Start a planning session and create thread immediately."""
        from dbos import SetWorkflowID
        from mainloop.services.planning import create_pending_task
        from mainloop.sse import notify_task_updated
        from mainloop.workflows.dbos_config import planning_queue
        from mainloop.workflows.planning_workflow import planning_workflow

        repo_url = args.get("repo_url", "")
        task_description = args.get("task_description", "")

        if not repo_url:
            return {
                "content": [
                    {
                        "type": "text",
                        "text": "Error: repo_url is required. Ask the user for the GitHub repository URL.",
                    }
                ],
                "is_error": True,
            }

        if not task_description:
            return {
                "content": [
                    {"type": "text", "text": "Error: task_description is required"}
                ],
                "is_error": True,
            }

        try:
            # Create task immediately
            task = await create_pending_task(
                user_id=user_id,
                main_thread_id=main_thread_id,
                conversation_id=conversation_id,
                description=task_description,
                repo_url=repo_url,
            )

            # Create assistant message with short ACK
            assistant_message = await db.create_message(
                conversation_id=conversation_id,
                role="assistant",
                content="On it!",
            )
            await db.increment_message_count(conversation_id)

            # Link task to message so thread appears under it
            await db.update_worker_task(
                task.id, originating_message_id=assistant_message.id
            )

            # Notify frontend
            await notify_task_updated(user_id, task.id, "pending")

            # Enqueue async planning workflow
            with SetWorkflowID(task.id):
                planning_queue.enqueue(planning_workflow, task.id)

            logger.info(
                f"Started task {task.id} with thread under message {assistant_message.id}"
            )

            return {
                "content": [
                    {
                        "type": "text",
                        "text": "Thread created. Do not respond further - the thread shows progress.",
                    }
                ]
            }
        except Exception as e:
            logger.error(f"Failed to start planning: {e}")
            return {
                "content": [
                    {"type": "text", "text": f"Error starting planning: {str(e)}"}
                ],
                "is_error": True,
            }

    @tool(
        "approve_plan",
        "Approve the current plan and create a GitHub issue. "
        "This will spawn a worker agent to implement the plan. "
        "Use this when the user approves the implementation plan.",
        {
            "plan_text": str,
        },
    )
    async def approve_plan_tool(args: dict[str, Any]) -> dict[str, Any]:
        """Approve the plan and create a task."""
        plan_text = args.get("plan_text", "")

        if not plan_text:
            return {
                "content": [{"type": "text", "text": "Error: plan_text is required"}],
                "is_error": True,
            }

        # Get active planning session
        session = await db.get_active_planning_session(conversation_id)
        if not session:
            return {
                "content": [
                    {
                        "type": "text",
                        "text": "Error: No active planning session. Use start_planning first.",
                    }
                ],
                "is_error": True,
            }

        try:
            task, result_message = await approve_plan(session, plan_text)
            logger.info(f"Approved plan, created task {task.id}")

            return {
                "content": [
                    {
                        "type": "text",
                        "text": result_message,
                    }
                ]
            }
        except Exception as e:
            logger.error(f"Failed to approve plan: {e}")
            return {
                "content": [
                    {"type": "text", "text": f"Error approving plan: {str(e)}"}
                ],
                "is_error": True,
            }

    @tool(
        "cancel_planning",
        "Cancel the current planning session. "
        "No GitHub issue will be created. "
        "Use this if the user decides not to proceed.",
        {},
    )
    async def cancel_planning_tool(args: dict[str, Any]) -> dict[str, Any]:
        """Cancel the planning session."""
        # Get active planning session
        session = await db.get_active_planning_session(conversation_id)
        if not session:
            return {
                "content": [
                    {
                        "type": "text",
                        "text": "No active planning session to cancel.",
                    }
                ],
            }

        try:
            result_message = await cancel_planning(session)
            logger.info(f"Cancelled planning session {session.id}")

            return {
                "content": [
                    {
                        "type": "text",
                        "text": result_message,
                    }
                ]
            }
        except Exception as e:
            logger.error(f"Failed to cancel planning: {e}")
            return {
                "content": [{"type": "text", "text": f"Error cancelling: {str(e)}"}],
                "is_error": True,
            }

    return [start_planning_tool, approve_plan_tool, cancel_planning_tool]


def format_conversation_history(messages: list[Message]) -> str:
    """Format conversation history for inclusion in prompt."""
    if not messages:
        return ""

    lines = []
    for msg in messages:
        role = "User" if msg.role == "user" else "Assistant"
        lines.append(f"{role}: {msg.content}")

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
    message_already_created: bool = False  # Tool already created the assistant message
    message_id: str | None = None  # ID of message created by tool


@dataclass
class ClaudeResponse:
    """Response from Claude."""

    text: str
    compacted: bool = False
    compaction_count: int = 0


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
    """Get a response from Claude with conversation context and planning tools.

    Context is provided via:
    - summary: Compacted summary of older messages (from PostgreSQL)
    - recent_messages: Recent unsummarized messages (from PostgreSQL)

    If user_id, main_thread_id, and conversation_id are provided, Claude
    will have access to planning tools for in-thread planning sessions.

    This ensures continuity across sessions, pod restarts, and deployments.
    """
    try:
        # Build prompt with summary and recent messages
        prompt_text = build_context_prompt(summary, recent_messages or [], message)

        # Create MCP server with planning tools if context is provided
        mcp_servers = {}
        allowed_tools = []
        system_prompt = None
        permission_mode = "bypassPermissions"

        if user_id and main_thread_id and conversation_id:
            planning_tools = create_planning_tools(
                user_id, main_thread_id, conversation_id
            )
            mcp_server = create_sdk_mcp_server(
                name="mainloop",
                version="1.0.0",
                tools=planning_tools,
            )
            mcp_servers["mainloop"] = mcp_server
            allowed_tools.extend(
                [
                    "mcp__mainloop__start_planning",
                    "mcp__mainloop__approve_plan",
                    "mcp__mainloop__cancel_planning",
                ]
            )

            # Fetch recent repos for system prompt
            recent_repos = await db.get_recent_repos(main_thread_id)
            # Main thread always uses normal chat mode - planning runs in background
            system_prompt = build_chat_system_prompt(recent_repos, planning_active=False)

        options = ClaudeAgentOptions(
            model=model,
            permission_mode=permission_mode,
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
        )
    except Exception as e:
        logger.error(f"Claude Agent SDK error: {e}")
        return ClaudeResponse(text=f"Sorry, I encountered an error: {str(e)}")


async def process_message(
    user_id: str,
    message: str,
    conversation_id: str,
    main_thread_id: str,
    summary: str | None = None,
    recent_messages: list[Message] | None = None,
) -> ChatResult:
    """Process a user message and return an immediate response.

    This handles:
    - Routing to existing tasks
    - Claude response with spawn_task tool (Claude decides when to spawn workers)

    Args:
        user_id: The user's unique identifier.
        message: The user's message text.
        conversation_id: The conversation's unique identifier.
        main_thread_id: The main thread workflow ID.
        summary: Compacted summary of older messages.
        recent_messages: Recent unsummarized messages for context.

    """
    # Check for routing to existing tasks first
    matches = await find_matching_tasks(user_id, message)

    if matches:
        best_match = matches[0]

        if best_match.confidence >= 0.7:
            # High confidence match - add to inbox for confirmation
            queue_item = await _create_routing_queue_item(
                main_thread_id=main_thread_id,
                user_id=user_id,
                task=best_match.task,
                message=message,
                conversation_id=conversation_id,
                matches=[best_match],
            )
            return ChatResult(
                response=f"This looks related to an existing task: {best_match.task.description[:100]}. Check your inbox to confirm.",
                needs_inbox_action=True,
                queue_item=queue_item,
            )

        elif len(matches) > 1 and matches[0].confidence >= 0.4:
            # Multiple matches - ask user to choose
            queue_item = await _create_routing_queue_item(
                main_thread_id=main_thread_id,
                user_id=user_id,
                task=matches[0].task,
                message=message,
                conversation_id=conversation_id,
                matches=matches[:3],
                multiple=True,
            )
            return ChatResult(
                response="Multiple active tasks might match. Check your inbox to choose.",
                needs_inbox_action=True,
                queue_item=queue_item,
            )

    # Get Claude response with spawn_task tool available
    # Claude will naturally decide when to ask for confirmation and spawn tasks
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

    # Check if start_planning tool created a task with message already
    # (tool creates "On it!" message and links task to it)
    task_id = None
    message_already_created = False
    message_id = None

    # Get most recent task for this conversation
    recent_task = await db.get_recent_task_for_conversation(conversation_id)
    if recent_task and recent_task.originating_message_id:
        # Tool already created the message - don't create another
        task_id = recent_task.id
        message_already_created = True
        message_id = recent_task.originating_message_id

    return ChatResult(
        response=claude_response.text,
        task_id=task_id,
        message_already_created=message_already_created,
        message_id=message_id,
    )


async def _create_routing_queue_item(
    main_thread_id: str,
    user_id: str,
    task: WorkerTask,
    message: str,
    conversation_id: str,
    matches: list,
    multiple: bool = False,
) -> QueueItem:
    """Create a queue item for routing confirmation."""
    if multiple:
        task_options = [f"{m.task.description[:50]}..." for m in matches]
        task_options.append("Create new task")
        content = f"Multiple active tasks might match: {message[:100]}"
        title = "Which task?"
        context = {
            "matches": [
                {"task_id": m.task.id, "confidence": m.confidence} for m in matches
            ],
            "original_message": message,
            "conversation_id": conversation_id,
        }
    else:
        task_options = ["Route to this task", "Create new task"]
        content = f"This looks related to: {task.description[:100]}"
        title = "Route to existing task?"
        context = {
            "suggested_task_id": task.id,
            "confidence": matches[0].confidence if matches else 0,
            "match_reasons": matches[0].match_reasons if matches else [],
            "original_message": message,
            "conversation_id": conversation_id,
        }

    queue_item = QueueItem(
        main_thread_id=main_thread_id,
        task_id=task.id,
        user_id=user_id,
        item_type=QueueItemType.ROUTING_SUGGESTION,
        priority=QueueItemPriority.HIGH,
        title=title,
        content=content,
        options=task_options,
        context=context,
    )
    return await db.create_queue_item(queue_item)
