"""Synchronous chat handler - processes messages and returns immediate responses."""

import logging
from dataclasses import dataclass

from dbos import SetWorkflowID
from claude_agent_sdk import query, ClaudeAgentOptions, AssistantMessage, ResultMessage, SystemMessage, TextBlock

from models import (
    WorkerTask,
    Message,
    QueueItem,
    QueueItemType,
    QueueItemPriority,
    TaskStatus,
)
from mainloop.db import db
from mainloop.config import settings
from mainloop.services.task_router import (
    find_matching_tasks,
    extract_keywords,
    should_skip_plan,
)
from mainloop.workflows.dbos_config import worker_queue

logger = logging.getLogger(__name__)


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


async def get_claude_response(
    message: str,
    summary: str | None = None,
    recent_messages: list[Message] | None = None,
    model: str = "sonnet",
) -> ClaudeResponse:
    """Get a response from Claude with conversation context.

    Context is provided via:
    - summary: Compacted summary of older messages (from PostgreSQL)
    - recent_messages: Recent unsummarized messages (from PostgreSQL)

    This ensures continuity across sessions, pod restarts, and deployments.
    """
    try:
        # Build prompt with summary and recent messages
        prompt = build_context_prompt(summary, recent_messages or [], message)

        options = ClaudeAgentOptions(
            model=model,
            permission_mode="bypassPermissions",
        )

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
                    logger.info(f"Context compacted ({trigger}): {pre_tokens} tokens summarized")

        return ClaudeResponse(
            text="\n".join(collected_text) if collected_text else "No response generated.",
            compacted=compaction_count > 0,
            compaction_count=compaction_count,
        )
    except Exception as e:
        logger.error(f"Claude Agent SDK error: {e}")
        return ClaudeResponse(text=f"Sorry, I encountered an error: {str(e)}")


def is_task_request(message: str) -> bool:
    """Check if the message is requesting a coding/development task."""
    message_lower = message.lower()

    # Task keywords that indicate work to be done
    task_keywords = [
        "build",
        "fix",
        "create",
        "update",
        "implement",
        "add",
        "remove",
        "change",
        "deploy",
        "refactor",
        "write code",
        "make a pr",
        "pull request",
    ]

    # Check for task keywords
    has_task_keyword = any(keyword in message_lower for keyword in task_keywords)

    # Also check for repo/project references that suggest code work
    has_repo_reference = any(
        pattern in message_lower
        for pattern in ["github.com", "my repo", "the codebase"]
    )

    return has_task_keyword and has_repo_reference


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
    - Spawning new workers for coding tasks
    - Direct Claude responses for everything else (no inbox)

    Args:
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

    # Check if this is a coding/development task request
    if is_task_request(message):
        # Extract keywords and create worker task
        keywords = extract_keywords(message)
        skip_plan = should_skip_plan(message)

        task = WorkerTask(
            main_thread_id=main_thread_id,
            user_id=user_id,
            task_type="feature",
            description=message,
            prompt=message,
            status=TaskStatus.PENDING,
            conversation_id=conversation_id,
            keywords=keywords,
            skip_plan=skip_plan,
        )
        task = await db.create_worker_task(task)

        # Enqueue the worker task
        from mainloop.workflows.worker import worker_task_workflow

        with SetWorkflowID(task.id):
            worker_queue.enqueue(worker_task_workflow, task.id)

        logger.info(f"Spawned worker task: {task.id} (skip_plan={skip_plan})")

        response = f"I'm starting work on that. I'll update you in your inbox when I have progress."

        return ChatResult(response=response, task_id=task.id)

    # For regular questions/chat, get a direct response from Claude (no inbox)
    model = settings.claude_model  # Uses haiku by default
    claude_response = await get_claude_response(
        message,
        summary=summary,
        recent_messages=recent_messages,
        model=model,
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
