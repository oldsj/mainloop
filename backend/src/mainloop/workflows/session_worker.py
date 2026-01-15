"""Session worker workflow - direct conversation with agent SDK.

Simple model:
1. User sends message
2. Agent SDK responds
3. Wait for next user message
4. Repeat
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
from mainloop.sse import notify_session_updated
from mainloop.workflows.transactions import (
    add_message_to_conversation,
    load_session,
    update_session_status,
)

from models import SessionStatus

logger = logging.getLogger(__name__)

# Topics for DBOS messaging
TOPIC_USER_MESSAGE = "user_message"

# Timeouts
USER_INPUT_TIMEOUT = 86400  # 24 hours

SESSION_SYSTEM_PROMPT = """You are an AI assistant working in a background session. Respond directly to the user's request."""


@DBOS.step()
async def get_claude_response(conversation_id: str, repo_url: str | None = None) -> str:
    """Get Claude's response for the session conversation."""
    messages = await db.get_messages(conversation_id)

    # Build conversation context
    history_parts = []
    for msg in messages:
        role = "User" if msg.role == "user" else "Assistant"
        history_parts.append(f"{role}: {msg.content}")

    context = "\n\n".join(history_parts)
    repo_context = f"\n\nRepository: {repo_url}" if repo_url else ""

    prompt_text = f"""Continue this conversation:{repo_context}

{context}

Respond to the user's latest message."""

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


async def notify_status(user_id: str, session_id: str, status: str):
    """Send SSE notification about session status change."""
    await notify_session_updated(user_id, session_id, status)


@DBOS.workflow()
async def session_worker_workflow(session_id: str) -> dict[str, Any]:
    """Run session workflow with direct conversation via agent SDK.

    1. Process initial prompt
    2. Wait for user message
    3. Process user message
    4. Repeat
    """
    logger.info(f"Starting session workflow: {session_id}")

    session = load_session(session_id)
    if not session:
        return {"status": "failed", "error": "Session not found"}

    try:
        # Mark session as active
        update_session_status(
            session_id,
            SessionStatus.ACTIVE,
            started_at=datetime.now(timezone.utc),
        )
        await notify_status(session.user_id, session_id, "active")

        # Add initial prompt as user message
        add_message_to_conversation(
            session.conversation_id,
            "user",
            session.prompt,
        )

        # Get agent response to initial prompt
        response = await get_claude_response(
            session.conversation_id,
            repo_url=session.repo_url,
        )
        add_message_to_conversation(
            session.conversation_id,
            "assistant",
            response,
        )

        # Now wait for user messages in a loop
        while True:
            # Wait for user input
            update_session_status(session_id, SessionStatus.WAITING_ON_USER)
            await notify_status(session.user_id, session_id, "waiting_on_user")

            message_response = await DBOS.recv_async(
                topic=TOPIC_USER_MESSAGE,
                timeout_seconds=USER_INPUT_TIMEOUT,
            )

            if message_response is None:
                # Timeout - complete session
                update_session_status(
                    session_id,
                    SessionStatus.COMPLETED,
                    completed_at=datetime.now(timezone.utc),
                )
                await notify_status(session.user_id, session_id, "completed")
                return {"status": "completed", "reason": "timeout"}

            # Got notification that user sent a message (already saved by API)
            # Mark as active and get response
            update_session_status(session_id, SessionStatus.ACTIVE)
            await notify_status(session.user_id, session_id, "active")

            response = await get_claude_response(
                session.conversation_id,
                repo_url=session.repo_url,
            )
            add_message_to_conversation(
                session.conversation_id,
                "assistant",
                response,
            )

            # Loop back to wait for next user message

    except Exception as e:
        logger.error(f"Session workflow failed: {e}")
        update_session_status(
            session_id,
            SessionStatus.FAILED,
            completed_at=datetime.now(timezone.utc),
            error=str(e),
        )
        await notify_status(session.user_id, session_id, "failed")
        return {"status": "failed", "error": str(e)}
