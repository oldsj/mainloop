"""Worker task workflow - executes tasks in isolated K8s namespaces with PR feedback loop."""

import logging
from datetime import datetime, timezone
from typing import Any

from dbos import DBOS

from models import WorkerTask, TaskStatus
from mainloop.db import db
from mainloop.config import settings
from mainloop.services.k8s_namespace import (
    create_task_namespace,
    copy_secrets_to_namespace,
    delete_task_namespace,
)
from mainloop.services.k8s_jobs import create_worker_job
from mainloop.services.github_pr import (
    get_pr_status,
    get_pr_comments,
    format_feedback_for_agent,
    is_pr_merged,
)

logger = logging.getLogger(__name__)

# Topic for receiving results from K8s Jobs
TOPIC_JOB_RESULT = "job_result"

# Polling interval for PR status (seconds)
PR_POLL_INTERVAL = 60


@DBOS.step()
async def load_worker_task(task_id: str) -> WorkerTask | None:
    """Load worker task from database."""
    return await db.get_worker_task(task_id)


@DBOS.step()
async def update_task_status(
    task_id: str,
    status: TaskStatus,
    pr_url: str | None = None,
    pr_number: int | None = None,
    error: str | None = None,
) -> None:
    """Update task status in database."""
    await db.update_worker_task(
        task_id,
        status=status,
        pr_url=pr_url,
        pr_number=pr_number,
        error=error,
    )


@DBOS.step()
async def setup_namespace(task_id: str) -> str:
    """Create namespace and copy secrets."""
    namespace = await create_task_namespace(task_id)
    await copy_secrets_to_namespace(task_id, namespace)
    return namespace


@DBOS.step()
async def spawn_initial_job(
    task_id: str,
    namespace: str,
    task: WorkerTask,
) -> str:
    """Spawn the initial Job to implement the task and create PR."""
    callback_url = f"{settings.backend_internal_url}/internal/tasks/{task_id}/complete"

    job_name = await create_worker_job(
        task_id=task_id,
        namespace=namespace,
        prompt=task.description,
        mode="initial",
        callback_url=callback_url,
        model=task.model,
        repo_url=task.repo_url,
    )

    return job_name


@DBOS.step()
async def spawn_feedback_job(
    task_id: str,
    namespace: str,
    task: WorkerTask,
    pr_number: int,
    feedback: str,
    iteration: int = 1,
) -> str:
    """Spawn a Job to address PR feedback."""
    callback_url = f"{settings.backend_internal_url}/internal/tasks/{task_id}/complete"

    job_name = await create_worker_job(
        task_id=task_id,
        namespace=namespace,
        prompt=task.description,
        mode="feedback",
        callback_url=callback_url,
        model=task.model,
        repo_url=task.repo_url,
        pr_number=pr_number,
        feedback_context=feedback,
        iteration=iteration,
    )

    return job_name


@DBOS.step()
async def check_pr_status(repo_url: str, pr_number: int) -> dict:
    """Check the current status of a PR."""
    status = await get_pr_status(repo_url, pr_number)
    if not status:
        return {"state": "not_found"}

    return {
        "state": status.state,
        "merged": status.merged,
        "updated_at": status.updated_at.isoformat(),
    }


@DBOS.step()
async def check_for_new_comments(
    repo_url: str,
    pr_number: int,
    since: datetime,
) -> list[dict]:
    """Check for new comments on a PR."""
    comments = await get_pr_comments(repo_url, pr_number, since=since)
    return [
        {
            "id": c.id,
            "user": c.user,
            "body": c.body,
            "created_at": c.created_at.isoformat(),
        }
        for c in comments
    ]


@DBOS.step()
async def get_feedback_context(
    repo_url: str,
    pr_number: int,
    since: datetime,
) -> str:
    """Get formatted feedback for the agent."""
    return await format_feedback_for_agent(repo_url, pr_number, since=since)


@DBOS.step()
async def cleanup_namespace(task_id: str) -> None:
    """Delete the task namespace."""
    await delete_task_namespace(task_id)


def notify_main_thread(
    user_id: str,
    task_id: str,
    message_type: str,
    payload: dict,
) -> None:
    """Send notification to main thread workflow. Must be called from within a workflow."""
    main_thread_workflow_id = f"main-thread-{user_id}"
    DBOS.send(
        main_thread_workflow_id,
        {
            "type": message_type,
            "payload": {"task_id": task_id, **payload},
        },
    )


@DBOS.workflow()
async def worker_task_workflow(task_id: str) -> dict[str, Any]:
    """
    Worker workflow that executes a task in an isolated K8s namespace.

    This workflow:
    1. Creates an isolated namespace for the task
    2. Spawns a K8s Job to implement the feature and create a PR
    3. Polls GitHub for PR comments and spawns Jobs to address feedback
    4. Waits for human to merge the PR
    5. Cleans up the namespace

    The workflow uses DBOS for durable execution - it survives restarts
    and will resume from the last completed step.
    """
    logger.info(f"Starting worker for task: {task_id}")

    # Load the task
    task = await load_worker_task(task_id)
    if not task:
        return {"status": "failed", "error": "Task not found"}

    # Mark as running
    await update_task_status(task_id, TaskStatus.RUNNING)

    # Phase 1: Setup namespace
    logger.info(f"Creating namespace for task: {task_id}")
    namespace = await setup_namespace(task_id)

    try:
        # Phase 2: Spawn initial Job to create PR
        logger.info(f"Spawning initial job in namespace: {namespace}")
        await spawn_initial_job(task_id, namespace, task)

        # Wait for Job to complete (via callback endpoint + DBOS.recv)
        # The callback endpoint will send a message when the Job finishes
        logger.info("Waiting for initial job to complete...")

        # Wait for result with 1 hour timeout
        result = await DBOS.recv_async(topic=TOPIC_JOB_RESULT, timeout_seconds=3600)

        if result is None:
            raise RuntimeError("Timed out waiting for initial job to complete")

        if result.get("status") == "failed":
            raise RuntimeError(result.get("error", "Job failed"))

        # Extract PR URL from result
        pr_url = result.get("result", {}).get("pr_url")
        if not pr_url:
            # No PR created - task is done
            logger.info("Task completed without PR")
            notify_main_thread(
                task.user_id,
                task_id,
                "worker_result",
                {"status": "completed", "result": result.get("result")},
            )
            return {"status": "completed", "result": result.get("result")}

        # Extract PR number from URL
        pr_number = int(pr_url.split("/")[-1])
        await update_task_status(task_id, TaskStatus.WAITING_HUMAN, pr_url=pr_url, pr_number=pr_number)

        # Notify human that PR is ready for review
        notify_main_thread(
            task.user_id,
            task_id,
            "worker_result",
            {
                "status": "needs_review",
                "result": {"pr_url": pr_url, "message": "PR ready for review"},
            },
        )

        # Phase 3: PR Polling Loop
        logger.info(f"Starting PR polling loop for PR #{pr_number}")
        last_check = datetime.now(timezone.utc)
        feedback_iteration = 0  # Track feedback job iterations for unique naming

        while True:
            # Durable sleep - survives restarts
            await DBOS.sleep_async(PR_POLL_INTERVAL)

            # Check PR status
            pr_status = await check_pr_status(task.repo_url, pr_number)

            if pr_status["state"] == "not_found":
                logger.warning(f"PR #{pr_number} not found")
                break

            if pr_status["merged"]:
                # PR merged - task complete!
                logger.info(f"PR #{pr_number} has been merged")
                await update_task_status(task_id, TaskStatus.COMPLETED)
                notify_main_thread(
                    task.user_id,
                    task_id,
                    "worker_result",
                    {"status": "completed", "result": {"pr_url": pr_url, "merged": True}},
                )
                break

            if pr_status["state"] == "closed":
                # PR closed without merge - task cancelled
                logger.info(f"PR #{pr_number} was closed without merge")
                await update_task_status(task_id, TaskStatus.CANCELLED)
                notify_main_thread(
                    task.user_id,
                    task_id,
                    "worker_result",
                    {"status": "cancelled", "result": {"pr_url": pr_url, "closed": True}},
                )
                break

            # Check for new comments
            new_comments = await check_for_new_comments(task.repo_url, pr_number, last_check)

            if new_comments:
                logger.info(f"Found {len(new_comments)} new comments on PR #{pr_number}")

                # Get formatted feedback
                feedback = await get_feedback_context(task.repo_url, pr_number, last_check)

                if feedback:
                    # Spawn Job to address feedback
                    feedback_iteration += 1
                    logger.info(f"Spawning feedback job (iteration {feedback_iteration})...")
                    await spawn_feedback_job(task_id, namespace, task, pr_number, feedback, feedback_iteration)

                    # Wait for feedback Job to complete
                    feedback_result = await DBOS.recv_async(topic=TOPIC_JOB_RESULT, timeout_seconds=3600)

                    if feedback_result and feedback_result.get("status") == "completed":
                        # Notify human that feedback was addressed
                        notify_main_thread(
                            task.user_id,
                            task_id,
                            "worker_result",
                            {
                                "status": "feedback_addressed",
                                "result": {"pr_url": pr_url, "message": "Addressed PR feedback"},
                            },
                        )

            # Update last check time
            last_check = datetime.now(timezone.utc)

        return {"status": "completed", "pr_url": pr_url}

    except Exception as e:
        logger.error(f"Worker task failed: {e}")
        await update_task_status(task_id, TaskStatus.FAILED, error=str(e))
        notify_main_thread(
            task.user_id,
            task_id,
            "worker_result",
            {"status": "failed", "error": str(e)},
        )
        return {"status": "failed", "error": str(e)}

    finally:
        # Cleanup namespace (always run) - retry up to 3 times
        logger.info(f"Cleaning up namespace: {namespace}")
        cleanup_attempts = 3
        for attempt in range(1, cleanup_attempts + 1):
            try:
                await cleanup_namespace(task_id)
                logger.info(f"Successfully cleaned up namespace: {namespace}")
                break
            except Exception as e:
                if attempt < cleanup_attempts:
                    logger.warning(f"Cleanup attempt {attempt} failed: {e}, retrying...")
                    import asyncio
                    await asyncio.sleep(2 ** attempt)  # Exponential backoff
                else:
                    logger.error(f"Failed to cleanup namespace after {cleanup_attempts} attempts: {e}")
