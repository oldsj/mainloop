"""Session worker workflow - runs Claude in isolated K8s Jobs.

Sessions run in their own K8s namespace with full isolation:
1. User sends message
2. K8s Job spawned with prompt
3. Job POSTs result back via callback
4. Result added to conversation
5. Wait for next user message
6. Repeat
"""

import logging
from datetime import datetime, timezone
from typing import Any

from dbos import DBOS
from mainloop.config import settings
from mainloop.services.k8s_jobs import create_session_job
from mainloop.services.k8s_namespace import (
    apply_session_namespace_network_policies,
    copy_secrets_to_namespace,
    create_session_namespace,
    delete_session_namespace,
    setup_session_rbac,
)
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
TOPIC_JOB_RESULT = "job_result"

# Timeouts
USER_INPUT_TIMEOUT = 86400  # 24 hours
JOB_TIMEOUT = 3600  # 1 hour per job

SESSION_SYSTEM_PROMPT = """You are an AI assistant working in a background session. Respond directly to the user's request."""


@DBOS.step()
async def setup_namespace(session_id: str) -> str:
    """Create namespace, copy secrets, set up RBAC, and apply network policies."""
    namespace = await create_session_namespace(session_id)
    await copy_secrets_to_namespace(session_id, namespace)
    await setup_session_rbac(session_id, namespace)
    await apply_session_namespace_network_policies(session_id, namespace)
    return namespace


@DBOS.step()
async def cleanup_namespace(session_id: str) -> None:
    """Delete the session namespace."""
    await delete_session_namespace(session_id)


@DBOS.step()
async def spawn_session_job(
    session_id: str,
    namespace: str,
    prompt: str,
    model: str | None = None,
    iteration: int = 0,
) -> str:
    """Spawn a K8s Job to run Claude with the given prompt."""
    callback_url = (
        f"{settings.backend_internal_url}/internal/sessions/{session_id}/complete"
    )

    job_name = await create_session_job(
        session_id=session_id,
        namespace=namespace,
        prompt=prompt,
        callback_url=callback_url,
        model=model,
        iteration=iteration,
    )

    return job_name


def build_conversation_prompt(
    messages: list,
    repo_url: str | None = None,
) -> str:
    """Build prompt from conversation history."""
    history_parts = []
    for msg in messages:
        role = "User" if msg.role == "user" else "Assistant"
        history_parts.append(f"{role}: {msg.content}")

    context = "\n\n".join(history_parts)
    repo_context = f"\n\nRepository: {repo_url}" if repo_url else ""

    return f"""{SESSION_SYSTEM_PROMPT}

Continue this conversation:{repo_context}

{context}

Respond to the user's latest message."""


async def notify_status(user_id: str, session_id: str, status: str):
    """Send SSE notification about session status change."""
    await notify_session_updated(user_id, session_id, status)


@DBOS.workflow()
async def session_worker_workflow(session_id: str) -> dict[str, Any]:
    """Run session workflow with Claude running in isolated K8s Jobs.

    1. Set up isolated K8s namespace
    2. Spawn job for initial prompt
    3. Wait for job result (via callback)
    4. Add response to conversation
    5. Wait for user message
    6. Spawn job for response
    7. Repeat steps 3-6
    8. Clean up namespace on completion
    """
    logger.info(f"Starting session workflow: {session_id}")

    session = load_session(session_id)
    if not session:
        return {"status": "failed", "error": "Session not found"}

    namespace = None
    iteration = 0

    try:
        # Set up isolated namespace
        logger.info(f"Setting up namespace for session: {session_id}")
        namespace = await setup_namespace(session_id)

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

        # Build initial prompt and spawn job
        from mainloop.db import db

        messages = await db.get_messages(session.conversation_id)
        prompt = build_conversation_prompt(messages, session.repo_url)

        logger.info(f"Spawning initial job for session: {session_id}")
        await spawn_session_job(
            session_id,
            namespace,
            prompt,
            model=session.model,
            iteration=iteration,
        )

        # Wait for job result
        result = await DBOS.recv_async(
            topic=TOPIC_JOB_RESULT,
            timeout_seconds=JOB_TIMEOUT,
        )

        if result is None:
            raise RuntimeError("Job timed out waiting for response")

        if result.get("status") == "failed":
            raise RuntimeError(result.get("error", "Job failed"))

        # Add response to conversation
        response = result.get("result", {}).get("output", "No response generated.")
        add_message_to_conversation(
            session.conversation_id,
            "assistant",
            response,
        )

        # Now wait for user messages in a loop
        while True:
            iteration += 1

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
            # Mark as active and spawn job for response
            update_session_status(session_id, SessionStatus.ACTIVE)
            await notify_status(session.user_id, session_id, "active")

            # Get updated conversation and spawn job
            messages = await db.get_messages(session.conversation_id)
            prompt = build_conversation_prompt(messages, session.repo_url)

            logger.info(
                f"Spawning job for session: {session_id} (iteration {iteration})"
            )
            await spawn_session_job(
                session_id,
                namespace,
                prompt,
                model=session.model,
                iteration=iteration,
            )

            # Wait for job result
            result = await DBOS.recv_async(
                topic=TOPIC_JOB_RESULT,
                timeout_seconds=JOB_TIMEOUT,
            )

            if result is None:
                raise RuntimeError("Job timed out waiting for response")

            if result.get("status") == "failed":
                raise RuntimeError(result.get("error", "Job failed"))

            # Add response to conversation
            response = result.get("result", {}).get("output", "No response generated.")
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

    finally:
        # Clean up namespace
        if namespace:
            logger.info(f"Cleaning up namespace for session: {session_id}")
            try:
                await cleanup_namespace(session_id)
            except Exception as e:
                logger.warning(f"Failed to cleanup namespace: {e}")
