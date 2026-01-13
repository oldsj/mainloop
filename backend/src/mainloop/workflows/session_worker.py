"""Session worker workflow - unified background work (both conversations and code work).

Sessions appear in the Sessions panel where users can:
- Follow progress in real-time
- Interact when input is needed
- See status (active, waiting, completed)

Sessions can be:
- Simple Claude conversations (no repo_url)
- Code work with GitHub integration (with repo_url)
"""

import logging
from datetime import datetime, timezone
from typing import Any

from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    TextBlock,
    query,
)
from dbos import DBOS
from mainloop.config import settings
from mainloop.db import db
from mainloop.sse import notify_session_needs_input, notify_session_updated

from models import Session, SessionNotification, SessionStatus

logger = logging.getLogger(__name__)

# Topics for DBOS messaging
TOPIC_USER_MESSAGE = "user_message"  # User sends message to session

# Timeouts
USER_INPUT_TIMEOUT = 86400  # 24 hours waiting for user input

SESSION_SYSTEM_PROMPT = """You are working on a task in a background session.

Focus on completing what's described in the initial prompt. Work independently, but when you need user input to proceed, clearly state what you need and why.

Be concise. When done, summarize what you accomplished."""


@DBOS.step()
async def load_session(session_id: str) -> Session | None:
    """Load session from database."""
    return await db.get_session(session_id)


@DBOS.step()
async def update_session_status(
    session_id: str,
    status: SessionStatus,
    worker_pod_name: str | None = None,
    started_at: datetime | None = None,
    completed_at: datetime | None = None,
    summary: str | None = None,
    error: str | None = None,
) -> None:
    """Update session status in database."""
    await db.update_session(
        session_id,
        status=status,
        worker_pod_name=worker_pod_name,
        started_at=started_at,
        completed_at=completed_at,
        summary=summary,
        error=error,
    )


@DBOS.step()
async def create_notification(
    session_id: str,
    user_id: str,
    title: str,
    preview: str,
) -> SessionNotification:
    """Create a notification for the user."""
    notification = SessionNotification(
        session_id=session_id,
        user_id=user_id,
        title=title,
        preview=preview,
    )
    return await db.create_session_notification(notification)


@DBOS.step()
async def add_message_to_conversation(
    conversation_id: str,
    role: str,
    content: str,
) -> str:
    """Add a message to the session's conversation."""
    message = await db.create_message(conversation_id, role, content)
    await db.increment_message_count(conversation_id)
    return message.id


@DBOS.step()
async def get_conversation_messages(conversation_id: str) -> list[dict]:
    """Get messages from the session's conversation."""
    messages = await db.get_messages(conversation_id)
    return [
        {
            "role": m.role,
            "content": m.content,
        }
        for m in messages
    ]


@DBOS.step()
async def get_claude_response_for_session(
    conversation_id: str,
    new_message: str | None = None,
    repo_url: str | None = None,
) -> str:
    """Get Claude's response for the session conversation.

    Loads conversation history and gets a response from Claude.
    """
    # Get conversation history
    messages = await db.get_messages(conversation_id)

    # Build conversation context
    history_parts = []
    for msg in messages:
        role = "User" if msg.role == "user" else "Assistant"
        history_parts.append(f"{role}: {msg.content}")

    if new_message:
        history_parts.append(f"User: {new_message}")

    context = "\n\n".join(history_parts)

    # Add repo context if available
    repo_context = f"\n\nRepository: {repo_url}" if repo_url else ""

    prompt_text = f"""Continue this session conversation:{repo_context}

{context}

Respond to help with the task. If you've completed the task or need user input, say so clearly."""

    # Call Claude
    model = settings.claude_model
    options = ClaudeAgentOptions(
        model=model,
        permission_mode="bypassPermissions",
        system_prompt=SESSION_SYSTEM_PROMPT,
    )

    collected_text = []
    async for msg in query(prompt=prompt_text, options=options):
        if isinstance(msg, AssistantMessage):
            for block in msg.content:
                if isinstance(block, TextBlock):
                    collected_text.append(block.text)

    return "\n".join(collected_text) if collected_text else "No response generated."


@DBOS.step()
async def post_summary_to_main_thread(
    main_thread_id: str,
    session_id: str,
    title: str,
    summary: str,
    user_id: str,
) -> None:
    """Post a summary message to the main thread when session completes."""
    # Get user's main conversation
    conversations = await db.list_conversations(user_id, limit=1)
    if not conversations:
        logger.warning(f"No conversation found for user {user_id}")
        return

    main_conversation = conversations[0]

    # Post summary
    summary_content = f"""**Session Completed: {title}**

{summary}

[View full session](/sessions/{session_id})"""

    await db.create_message(main_conversation.id, "assistant", summary_content)
    await db.increment_message_count(main_conversation.id)


async def notify_session_status_sse(user_id: str, session_id: str, status: str):
    """Send SSE notification about session status change."""
    await notify_session_updated(user_id, session_id, status)


async def notify_needs_input_sse(
    user_id: str,
    session_id: str,
    title: str,
    preview: str,
):
    """Send SSE notification that session needs user input."""
    await notify_session_needs_input(user_id, session_id, title, preview)


def _check_needs_input(response: str) -> tuple[bool, str]:
    """Check if Claude's response indicates it needs user input.

    Returns (needs_input, preview_text).
    """
    lower = response.lower()
    needs_input_phrases = [
        "need your input",
        "need more information",
        "please provide",
        "could you clarify",
        "what would you like",
        "please let me know",
        "waiting for your",
        "need you to",
        "can you provide",
        "please specify",
        "i have a question",
        "before i proceed",
        "should i",
        "do you want me to",
    ]

    for phrase in needs_input_phrases:
        if phrase in lower:
            # Extract a preview (first 100 chars or first sentence)
            preview = response[:150]
            if "." in preview[50:]:
                preview = preview[: preview.index(".", 50) + 1]
            return True, preview.strip()

    return False, ""


def _check_task_complete(response: str) -> tuple[bool, str]:
    """Check if Claude's response indicates the task is complete.

    Returns (is_complete, summary).
    """
    lower = response.lower()
    complete_phrases = [
        "task is complete",
        "task complete",
        "completed the task",
        "finished the task",
        "task has been completed",
        "i've completed",
        "i have completed",
        "that's everything",
        "all done",
        "work is done",
    ]

    for phrase in complete_phrases:
        if phrase in lower:
            # Use the response as the summary
            return True, response[:500]

    return False, ""


@DBOS.workflow()
async def session_worker_workflow(session_id: str) -> dict[str, Any]:
    """
    Session worker workflow - runs an interactive Claude conversation.

    This workflow:
    1. Sets up the session
    2. Processes the initial prompt with Claude
    3. Loops: check if done, check if needs input, wait for user message
    4. Posts summary to main thread when complete
    """
    logger.info(f"Starting session workflow: {session_id}")

    # Load the session
    session = await load_session(session_id)
    if not session:
        return {"status": "failed", "error": "Session not found"}

    try:
        # Mark session as active
        await update_session_status(
            session_id,
            SessionStatus.ACTIVE,
            started_at=datetime.now(timezone.utc),
        )
        await notify_session_status_sse(session.user_id, session_id, "active")

        # Add the initial prompt as a user message
        await add_message_to_conversation(
            session.conversation_id,
            "user",
            session.prompt,
        )

        # Main conversation loop
        max_turns = 10  # Prevent infinite loops
        turn = 0

        while turn < max_turns:
            turn += 1
            logger.info(f"Session {session_id} turn {turn}")

            # Get Claude's response (pass repo_url as context if available)
            response = await get_claude_response_for_session(
                session.conversation_id,
                repo_url=session.repo_url,
            )

            # Save response to conversation
            await add_message_to_conversation(
                session.conversation_id,
                "assistant",
                response,
            )

            # Check if task is complete
            is_complete, summary = _check_task_complete(response)
            if is_complete:
                logger.info(f"Session {session_id} completed")
                await update_session_status(
                    session_id,
                    SessionStatus.COMPLETED,
                    completed_at=datetime.now(timezone.utc),
                    summary=summary,
                )
                await notify_session_status_sse(
                    session.user_id, session_id, "completed"
                )

                # Post to main thread
                await post_summary_to_main_thread(
                    session.main_thread_id,
                    session_id,
                    session.title,
                    summary,
                    session.user_id,
                )

                return {"status": "completed", "summary": summary}

            # Check if needs user input
            needs_input, preview = _check_needs_input(response)
            if needs_input:
                logger.info(f"Session {session_id} waiting for user input")
                await update_session_status(session_id, SessionStatus.WAITING_ON_USER)
                await notify_session_status_sse(
                    session.user_id, session_id, "waiting_on_user"
                )

                # Create notification
                await create_notification(
                    session_id=session_id,
                    user_id=session.user_id,
                    title=f"Session: {session.title}",
                    preview=preview or "Claude needs your input",
                )
                await notify_needs_input_sse(
                    session.user_id,
                    session_id,
                    f"Session: {session.title}",
                    preview or "Claude needs your input",
                )

                # Wait for user message
                message_response = await DBOS.recv_async(
                    topic=TOPIC_USER_MESSAGE,
                    timeout_seconds=USER_INPUT_TIMEOUT,
                )

                if message_response is None:
                    # Timeout
                    logger.info(f"Session {session_id} timed out waiting for input")
                    await update_session_status(
                        session_id,
                        SessionStatus.COMPLETED,
                        completed_at=datetime.now(timezone.utc),
                        summary="Session timed out waiting for user input.",
                    )
                    await notify_session_status_sse(
                        session.user_id, session_id, "completed"
                    )
                    return {"status": "completed", "reason": "timeout"}

                # Got user message - add to conversation and continue
                user_message = message_response.get("message", "")
                await add_message_to_conversation(
                    session.conversation_id,
                    "user",
                    user_message,
                )

                # Mark as active again
                await update_session_status(session_id, SessionStatus.ACTIVE)
                await notify_session_status_sse(session.user_id, session_id, "active")
                continue

            # Claude didn't indicate done or need input - assume it's still working
            # Give it a small delay then continue
            await DBOS.sleep_async(1)

        # Max turns reached - complete the session
        logger.info(f"Session {session_id} reached max turns")
        summary = f"Session completed after {max_turns} turns. Last response saved to conversation."
        await update_session_status(
            session_id,
            SessionStatus.COMPLETED,
            completed_at=datetime.now(timezone.utc),
            summary=summary,
        )
        await notify_session_status_sse(session.user_id, session_id, "completed")

        await post_summary_to_main_thread(
            session.main_thread_id,
            session_id,
            session.title,
            summary,
            session.user_id,
        )

        return {"status": "completed", "summary": summary}

    except Exception as e:
        logger.error(f"Session workflow failed: {e}")
        await update_session_status(
            session_id,
            SessionStatus.FAILED,
            completed_at=datetime.now(timezone.utc),
            error=str(e),
        )
        await notify_session_status_sse(session.user_id, session_id, "failed")
        return {"status": "failed", "error": str(e)}
