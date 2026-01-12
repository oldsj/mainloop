"""Planning workflow - runs async planning sessions with Claude.

This workflow is enqueued immediately after a task is created, so the user
gets a fast ACK response while planning happens in the background.
"""

import logging

from dbos import DBOS
from mainloop.db import db
from mainloop.services.planning import (
    build_planning_system_prompt,
    start_planning_for_task,
)
from mainloop.services.repo_cache import get_repo_cache
from mainloop.sse import notify_task_updated

from models import TaskStatus

logger = logging.getLogger(__name__)


@DBOS.workflow()
async def planning_workflow(task_id: str) -> None:
    """Run an async planning session for a task.

    This workflow:
    1. Starts planning session (caches repo, creates session)
    2. Runs Claude with planning tools to explore and create a plan
    3. Updates task with the plan
    4. Transitions to WAITING_PLAN_REVIEW status

    Args:
        task_id: The task ID to plan for

    """
    await _run_planning(task_id)


async def _run_planning(task_id: str) -> None:
    """Async implementation of planning workflow."""
    from claude_agent_sdk import (
        AssistantMessage,
        ClaudeAgentOptions,
        ResultMessage,
        SystemMessage,
        TextBlock,
        query,
    )

    # Get the task
    task = await db.get_worker_task(task_id)
    if not task:
        logger.error(f"Task {task_id} not found")
        return

    if task.status != TaskStatus.PENDING:
        logger.warning(f"Task {task_id} is not PENDING (status={task.status})")
        return

    try:
        # Start planning session (updates status to PLANNING, caches repo)
        session, initial_message = await start_planning_for_task(task)

        # Notify frontend that planning has started
        await notify_task_updated(task.user_id, task_id, "planning")

        # Get repo path for Claude
        repo_cache = get_repo_cache()
        repo_path = repo_cache.get_repo_path(task.repo_url)

        # Build system prompt for planning
        system_prompt = build_planning_system_prompt(task.repo_url, task.description)

        # Build Claude options with codebase tools
        options = ClaudeAgentOptions(
            model="sonnet",
            permission_mode="plan",  # Read-only filesystem access
            cwd=str(repo_path),
            system_prompt=system_prompt,
            allowed_tools=["Read", "Glob", "Grep", "LS", "WebSearch"],
        )

        # Create the prompt that includes initial context
        prompt = f"""{initial_message}

Please explore this codebase and create an implementation plan for the task.
When you're done exploring, provide a clear plan with:
1. Summary of the approach
2. Files to modify
3. Implementation steps
4. Any considerations or trade-offs

Start exploring now."""

        logger.info(f"Running planning query for task {task_id}")

        collected_text: list[str] = []
        last_update_len = 0

        async for msg in query(prompt=prompt, options=options):
            if isinstance(msg, AssistantMessage):
                for block in msg.content:
                    if isinstance(block, TextBlock):
                        collected_text.append(block.text)

                        # Send SSE update every ~500 chars of new content
                        current_len = sum(len(t) for t in collected_text)
                        if current_len - last_update_len > 500:
                            last_update_len = current_len
                            # Update task with partial exploration text
                            await db.update_worker_task(
                                task_id,
                                plan_text="\n".join(collected_text),
                            )
                            await notify_task_updated(task.user_id, task_id, "planning")

            elif isinstance(msg, ResultMessage):
                if msg.is_error:
                    logger.error(f"Planning query error: {msg.result}")
                    error_text = f"Error during planning: {msg.result}"
                    await db.update_worker_task(
                        task_id,
                        status=TaskStatus.FAILED,
                        error=error_text,
                    )
                    await notify_task_updated(task.user_id, task_id, "failed")
                    return

            elif isinstance(msg, SystemMessage):
                # Track session ID if needed for resumption
                if msg.subtype == "init" and msg.data:
                    new_session_id = msg.data.get("session_id")
                    if new_session_id:
                        await db.update_planning_session(
                            session.id, claude_session_id=new_session_id
                        )

        # Save final plan and update status to waiting for review
        final_plan = (
            "\n".join(collected_text) if collected_text else "No plan generated."
        )
        await db.update_worker_task(
            task_id,
            status=TaskStatus.WAITING_PLAN_REVIEW,
            plan_text=final_plan,
        )
        await notify_task_updated(task.user_id, task_id, "waiting_plan_review")

        logger.info(f"Planning completed for task {task_id}")

    except Exception as e:
        logger.error(f"Planning workflow failed for task {task_id}: {e}")
        await db.update_worker_task(
            task_id,
            status=TaskStatus.FAILED,
            error=str(e),
        )
        await notify_task_updated(task.user_id, task_id, "failed")
