"""Worker task workflow - executes tasks using Claude Agent SDK with durable execution."""

import logging
from typing import Any

from dbos import DBOS

from models import WorkerTask, TaskStatus
from mainloop.db import db
from mainloop.services.claude_agent import get_claude_agent_client
from mainloop.config import settings

logger = logging.getLogger(__name__)

# Topic for receiving human input during task execution
TOPIC_HUMAN_RESPONSE = "human_response"


@DBOS.step()
async def load_worker_task(task_id: str) -> WorkerTask | None:
    """Load worker task from database."""
    return await db.get_worker_task(task_id)


@DBOS.step()
async def update_task_running(task_id: str) -> None:
    """Mark task as running."""
    from datetime import datetime, timezone
    await db.update_worker_task(
        task_id,
        status=TaskStatus.RUNNING,
        started_at=datetime.now(timezone.utc),
    )


@DBOS.step()
async def execute_claude_task(prompt: str, model: str | None = None) -> dict[str, Any]:
    """Execute a task using Claude Agent SDK via HTTP API.

    Claude-agent manages its own isolated workspace internally.
    """
    client = get_claude_agent_client()

    response = await client.execute(
        prompt=prompt,
        model=model or settings.claude_worker_model,
        timeout=600.0,  # 10 minute timeout for complex tasks
    )

    if response.error:
        raise RuntimeError(f"Claude execution failed: {response.error}")

    return {
        "output": response.output,
        "session_id": response.session_id,
        "cost_usd": response.cost_usd,
    }


@DBOS.workflow()
async def worker_task_workflow(task_id: str) -> dict[str, Any]:
    """
    Worker workflow that executes a task.

    This workflow:
    1. Loads the task from the database
    2. Sends the task to Claude Agent SDK (which manages its own isolated workspace)
    3. Reports results back to the main thread

    Claude-agent handles all workspace management, git operations, and PR creation
    internally for full isolation between workers.
    """
    logger.info(f"Starting worker for task: {task_id}")

    # Load the task
    task = await load_worker_task(task_id)
    if not task:
        return {"status": "failed", "error": "Task not found"}

    # Mark as running
    await update_task_running(task_id)

    try:
        # Build the prompt for Claude - include all context needed
        # Claude-agent will handle workspace setup, git, and PR creation
        prompt = f"""
Task ID: {task_id[:8]}
Task: {task.description}

{f"Repository to work on: {task.repo_url}" if task.repo_url else "No repository - just complete the task."}
{f"Base branch: {task.base_branch}" if task.repo_url else ""}
{f"Create feature branch: mainloop/{task_id[:8]}" if task.repo_url else ""}

Instructions:
1. {"Clone the repository and create a feature branch" if task.repo_url else "Create any needed files"}
2. Complete the task described above
3. {"Commit your changes and create a pull request" if task.repo_url else "Summarize what you did"}

Additional context:
{task.prompt}
"""

        # Execute the task - Claude-agent manages workspace isolation
        result = await execute_claude_task(prompt, model=task.model)
        agent_output = result["output"]

        # Send result to main thread
        main_thread_workflow_id = f"main-thread-{task.user_id}"
        DBOS.send(
            main_thread_workflow_id,
            {
                "type": "worker_result",
                "payload": {
                    "task_id": task_id,
                    "status": "completed",
                    "result": {
                        "summary": agent_output,
                        "cost_usd": result.get("cost_usd"),
                    },
                },
            },
        )

        return {
            "status": "completed",
            "summary": agent_output,
        }

    except Exception as e:
        logger.error(f"Worker task failed: {e}")

        # Send failure to main thread
        main_thread_workflow_id = f"main-thread-{task.user_id}"
        DBOS.send(
            main_thread_workflow_id,
            {
                "type": "worker_result",
                "payload": {
                    "task_id": task_id,
                    "status": "failed",
                    "error": str(e),
                },
            },
        )

        return {"status": "failed", "error": str(e)}


@DBOS.workflow()
async def interactive_worker_task(task_id: str) -> dict[str, Any]:
    """
    Worker workflow that can ask for human input during execution.

    This workflow uses DBOS.recv_async() to wait for human responses when needed.
    """
    logger.info(f"Starting interactive worker for task: {task_id}")

    task = await load_worker_task(task_id)
    if not task:
        return {"status": "failed", "error": "Task not found"}

    await update_task_running(task_id)

    # Notify that we might need input
    main_thread_workflow_id = f"main-thread-{task.user_id}"

    # Example: Ask a clarifying question
    DBOS.send(
        main_thread_workflow_id,
        {
            "type": "worker_result",
            "payload": {
                "task_id": task_id,
                "status": "needs_input",
                "result": {
                    "question": f"Before I start on '{task.description}', do you have any specific requirements?",
                    "options": ["No, proceed as described", "Yes, let me clarify"],
                },
            },
        },
    )

    # Wait for human response (timeout after 1 hour)
    response = await DBOS.recv_async(topic=TOPIC_HUMAN_RESPONSE, timeout_seconds=3600)

    if response is None:
        return {"status": "failed", "error": "Timed out waiting for human input"}

    human_input = response.get("response", "")
    logger.info(f"Received human input: {human_input}")

    # Continue with the task using the human input
    # ... (same as worker_task_workflow but with human_input incorporated)

    return {"status": "completed", "summary": f"Completed with input: {human_input}"}
