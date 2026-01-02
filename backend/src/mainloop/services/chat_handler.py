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
from mainloop.services.task_router import (
    extract_keywords,
    find_matching_tasks,
)
from mainloop.workflows.dbos_config import worker_queue

from models import (
    Message,
    QueueItem,
    QueueItemPriority,
    QueueItemType,
    TaskStatus,
    WorkerTask,
)

logger = logging.getLogger(__name__)


def build_chat_system_prompt(recent_repos: list[str] | None = None) -> str:
    """Build the system prompt for chat, including recent repos if available."""
    base_prompt = """You are a helpful AI assistant that can also spawn autonomous coding agents.

When the user requests work that involves modifying code, creating files, making commits, or any development task:
1. Confirm you understand what they want
2. Ask them to confirm they want you to spawn a worker agent
3. If they have recent repos, suggest one; otherwise ask for the GitHub repository URL
4. Once confirmed, use the spawn_task tool to start the work

When to use spawn_task:
- Creating, modifying, or deleting code files
- Making commits or pull requests
- Running builds, tests, or deployments
- Any work that requires access to a codebase

Do NOT use spawn_task for:
- Answering questions about how to do something
- Explaining concepts or providing information
- General conversation

Always get explicit confirmation before spawning a task."""

    if recent_repos:
        repos_list = "\n".join(f"  - {repo}" for repo in recent_repos)
        base_prompt += f"""

The user has recently worked with these repositories:
{repos_list}

If relevant to their request, suggest using one of these repos. For example:
"I can spawn a worker agent for this. Should I use {recent_repos[0]}?"
"""
    else:
        base_prompt += """

Ask for the GitHub repo URL like:
"I can spawn a worker agent to do this. Would you like me to proceed? Please provide the GitHub repo URL."
"""

    return base_prompt


def create_spawn_task_tool(
    user_id: str,
    main_thread_id: str,
    conversation_id: str,
):
    """Create a spawn_task tool with context baked in.

    This factory creates a tool that has access to the current user/conversation context.
    """

    @tool(
        "spawn_task",
        "Spawn an autonomous coding agent to work on a development task. "
        "Use this when the user confirms they want you to create, modify, or delete code. "
        "Requires a task description and GitHub repository URL.",
        {
            "task_description": str,
            "repo_url": str,
            "skip_planning": bool,
        },
    )
    async def spawn_task(args: dict[str, Any]) -> dict[str, Any]:
        """Spawn a worker task to handle a coding request."""
        task_description = args.get("task_description", "")
        repo_url = args.get("repo_url", "")
        skip_planning = args.get("skip_planning", False)

        if not task_description:
            return {
                "content": [
                    {"type": "text", "text": "Error: task_description is required"}
                ],
                "is_error": True,
            }

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

        # Validate repo URL format
        if not repo_url.startswith("https://github.com/"):
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
            keywords = extract_keywords(task_description)

            task = WorkerTask(
                main_thread_id=main_thread_id,
                user_id=user_id,
                task_type="feature",
                description=task_description,
                prompt=task_description,
                repo_url=repo_url,
                status=TaskStatus.PENDING,
                conversation_id=conversation_id,
                keywords=keywords,
                skip_plan=skip_planning,
            )
            task = await db.create_worker_task(task)

            # Enqueue the worker task
            from mainloop.workflows.worker import worker_task_workflow

            with SetWorkflowID(task.id):
                worker_queue.enqueue(worker_task_workflow, task.id)

            logger.info(
                f"Spawned worker task via tool: {task.id} (skip_plan={skip_planning})"
            )

            # Record this repo as recently used
            await db.add_recent_repo(main_thread_id, repo_url)

            return {
                "content": [
                    {
                        "type": "text",
                        "text": f"Worker task spawned successfully!\n"
                        f"Task ID: {task.id[:8]}\n"
                        f"Repository: {repo_url}\n"
                        f"Description: {task_description}\n"
                        f"Skip planning: {skip_planning}\n\n"
                        f"The agent will start working and update the user via their inbox.",
                    }
                ]
            }
        except Exception as e:
            logger.error(f"Failed to spawn task: {e}")
            return {
                "content": [{"type": "text", "text": f"Error spawning task: {str(e)}"}],
                "is_error": True,
            }

    return spawn_task


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

        if user_id and main_thread_id and conversation_id:
            spawn_task_tool = create_spawn_task_tool(
                user_id, main_thread_id, conversation_id
            )
            mcp_server = create_sdk_mcp_server(
                name="mainloop",
                version="1.0.0",
                tools=[spawn_task_tool],
            )
            mcp_servers["mainloop"] = mcp_server
            allowed_tools.append("mcp__mainloop__spawn_task")

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

        # CRITICAL: When using MCP servers, must use async generator for prompt.
        # This is a Claude Agent SDK requirement - string prompts fail with
        # "ProcessTransport is not ready for writing" error.
        if mcp_servers:
            prompt = _create_message_generator(prompt_text)
        else:
            prompt = prompt_text

        collected_text: list[str] = []
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

    return ChatResult(response=claude_response.text)


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
