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
    setup_worker_rbac,
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
    """Create namespace, copy secrets, and set up worker RBAC."""
    namespace = await create_task_namespace(task_id)
    await copy_secrets_to_namespace(task_id, namespace)
    await setup_worker_rbac(task_id, namespace)
    return namespace


@DBOS.step()
async def spawn_plan_job(
    task_id: str,
    namespace: str,
    task: WorkerTask,
    feedback: str | None = None,
    iteration: int = 1,
) -> str:
    """Spawn a Job to create the implementation plan (draft PR)."""
    callback_url = f"{settings.backend_internal_url}/internal/tasks/{task_id}/complete"

    job_name = await create_worker_job(
        task_id=task_id,
        namespace=namespace,
        prompt=task.description,
        mode="plan",
        callback_url=callback_url,
        model=task.model,
        repo_url=task.repo_url,
        feedback_context=feedback,
        iteration=iteration,
    )

    return job_name


@DBOS.step()
async def spawn_implement_job(
    task_id: str,
    namespace: str,
    task: WorkerTask,
    pr_number: int,
) -> str:
    """Spawn a Job to implement the approved plan."""
    callback_url = f"{settings.backend_internal_url}/internal/tasks/{task_id}/complete"

    job_name = await create_worker_job(
        task_id=task_id,
        namespace=namespace,
        prompt=task.description,
        mode="implement",
        callback_url=callback_url,
        model=task.model,
        repo_url=task.repo_url,
        pr_number=pr_number,
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


def is_approval_comment(comments: list[dict]) -> bool:
    """Check if any comment approves the plan."""
    approval_phrases = [
        "looks good", "lgtm", "approved", "proceed", "go ahead", "ship it",
        "good to go", "approve", "let's do it", "sounds good", "perfect",
        "all good", "nice", "great plan", "go for it"
    ]
    for comment in comments:
        body = comment.get("body", "").lower().strip()
        # Check if any approval phrase is in the comment
        if any(phrase in body for phrase in approval_phrases):
            return True
    return False


@DBOS.workflow()
async def worker_task_workflow(task_id: str) -> dict[str, Any]:
    """
    Worker workflow that executes a task in an isolated K8s namespace.

    This workflow follows a plan-first approach:
    1. Creates an isolated namespace for the task
    2. PLAN PHASE: Spawns a Job to create a draft PR with implementation plan
    3. Polls for plan approval (user comments "looks good", "lgtm", etc.)
    4. IMPLEMENT PHASE: Spawns a Job to implement the approved plan
    5. REVIEW PHASE: Polls for code review feedback, addresses comments
    6. Waits for human to merge the PR
    7. Cleans up the namespace

    The workflow uses DBOS for durable execution - it survives restarts
    and will resume from the last completed step.
    """
    logger.info(f"Starting worker for task: {task_id}")

    # Load the task
    task = await load_worker_task(task_id)
    if not task:
        return {"status": "failed", "error": "Task not found"}

    # Setup namespace
    logger.info(f"Creating namespace for task: {task_id}")
    namespace = await setup_namespace(task_id)

    try:
        pr_url: str | None = None
        pr_number: int | None = None
        plan_iteration = 0

        # ============================================================
        # PHASE 1: PLANNING (unless skip_plan is True)
        # ============================================================
        if not task.skip_plan:
            await update_task_status(task_id, TaskStatus.PLANNING)
            logger.info(f"Spawning plan job in namespace: {namespace}")
            await spawn_plan_job(task_id, namespace, task, iteration=1)

            # Wait for plan job to complete
            logger.info("Waiting for plan job to complete...")
            result = await DBOS.recv_async(topic=TOPIC_JOB_RESULT, timeout_seconds=3600)

            if result is None:
                raise RuntimeError("Timed out waiting for plan job to complete")
            if result.get("status") == "failed":
                raise RuntimeError(result.get("error", "Plan job failed"))

            # Extract PR URL from result
            pr_url = result.get("result", {}).get("pr_url")
            if not pr_url:
                raise RuntimeError("Plan job did not create a draft PR")

            pr_number = int(pr_url.split("/")[-1])
            await update_task_status(
                task_id, TaskStatus.WAITING_PLAN_REVIEW,
                pr_url=pr_url, pr_number=pr_number
            )

            # Notify human that plan is ready for review
            notify_main_thread(
                task.user_id,
                task_id,
                "worker_result",
                {
                    "status": "plan_ready",
                    "result": {"pr_url": pr_url, "message": "Plan ready for review"},
                },
            )

            # Poll for plan approval
            logger.info(f"Waiting for plan approval on PR #{pr_number}")
            last_check = datetime.now(timezone.utc)
            plan_iteration = 0

            while True:
                await DBOS.sleep_async(PR_POLL_INTERVAL)

                # Check PR status
                pr_status = await check_pr_status(task.repo_url, pr_number)
                if pr_status["state"] == "not_found":
                    raise RuntimeError(f"PR #{pr_number} not found")
                if pr_status["state"] == "closed":
                    await update_task_status(task_id, TaskStatus.CANCELLED)
                    notify_main_thread(
                        task.user_id, task_id, "worker_result",
                        {"status": "cancelled", "result": {"pr_url": pr_url, "closed": True}},
                    )
                    return {"status": "cancelled", "pr_url": pr_url}

                # Check for new comments
                new_comments = await check_for_new_comments(task.repo_url, pr_number, last_check)

                if new_comments:
                    if is_approval_comment(new_comments):
                        logger.info("Plan approved! Proceeding to implementation...")
                        break
                    else:
                        # Address plan feedback
                        logger.info(f"Found {len(new_comments)} comments, updating plan...")
                        feedback = await get_feedback_context(task.repo_url, pr_number, last_check)
                        if feedback:
                            plan_iteration += 1
                            await spawn_plan_job(task_id, namespace, task, feedback, plan_iteration)
                            plan_result = await DBOS.recv_async(topic=TOPIC_JOB_RESULT, timeout_seconds=3600)
                            if plan_result and plan_result.get("status") == "completed":
                                notify_main_thread(
                                    task.user_id, task_id, "worker_result",
                                    {"status": "plan_updated", "result": {"pr_url": pr_url}},
                                )

                last_check = datetime.now(timezone.utc)

        # ============================================================
        # PHASE 2: IMPLEMENTATION
        # ============================================================
        await update_task_status(task_id, TaskStatus.IMPLEMENTING)

        if task.skip_plan:
            # Skip plan mode: create PR directly with implementation
            logger.info("Skip plan mode: spawning implement job to create PR")
            await spawn_implement_job(task_id, namespace, task, pr_number=0)
        else:
            # Normal mode: implement the approved plan
            logger.info(f"Spawning implement job for approved plan on PR #{pr_number}")
            await spawn_implement_job(task_id, namespace, task, pr_number)

        # Wait for implementation to complete
        impl_result = await DBOS.recv_async(topic=TOPIC_JOB_RESULT, timeout_seconds=3600)

        if impl_result is None:
            raise RuntimeError("Timed out waiting for implement job to complete")
        if impl_result.get("status") == "failed":
            raise RuntimeError(impl_result.get("error", "Implement job failed"))

        # If we skipped planning, extract PR URL now
        if task.skip_plan:
            pr_url = impl_result.get("result", {}).get("pr_url")
            if not pr_url:
                # No PR created - task is done
                logger.info("Task completed without PR")
                notify_main_thread(
                    task.user_id, task_id, "worker_result",
                    {"status": "completed", "result": impl_result.get("result")},
                )
                return {"status": "completed", "result": impl_result.get("result")}
            pr_number = int(pr_url.split("/")[-1])

        await update_task_status(task_id, TaskStatus.WAITING_HUMAN, pr_url=pr_url, pr_number=pr_number)

        # Notify human that code is ready for review
        notify_main_thread(
            task.user_id,
            task_id,
            "worker_result",
            {
                "status": "code_ready",
                "result": {"pr_url": pr_url, "message": "Code ready for review"},
            },
        )

        # ============================================================
        # PHASE 3: CODE REVIEW LOOP
        # ============================================================
        logger.info(f"Starting code review loop for PR #{pr_number}")
        last_check = datetime.now(timezone.utc)
        feedback_iteration = 0

        while True:
            await DBOS.sleep_async(PR_POLL_INTERVAL)

            # Check PR status
            pr_status = await check_pr_status(task.repo_url, pr_number)

            if pr_status["state"] == "not_found":
                logger.warning(f"PR #{pr_number} not found")
                break

            if pr_status["merged"]:
                logger.info(f"PR #{pr_number} has been merged")
                await update_task_status(task_id, TaskStatus.COMPLETED)
                notify_main_thread(
                    task.user_id, task_id, "worker_result",
                    {"status": "completed", "result": {"pr_url": pr_url, "merged": True}},
                )
                break

            if pr_status["state"] == "closed":
                logger.info(f"PR #{pr_number} was closed without merge")
                await update_task_status(task_id, TaskStatus.CANCELLED)
                notify_main_thread(
                    task.user_id, task_id, "worker_result",
                    {"status": "cancelled", "result": {"pr_url": pr_url, "closed": True}},
                )
                break

            # Check for new comments
            new_comments = await check_for_new_comments(task.repo_url, pr_number, last_check)

            if new_comments:
                logger.info(f"Found {len(new_comments)} new comments on PR #{pr_number}")
                feedback = await get_feedback_context(task.repo_url, pr_number, last_check)

                if feedback:
                    feedback_iteration += 1
                    logger.info(f"Spawning feedback job (iteration {feedback_iteration})...")
                    await spawn_feedback_job(task_id, namespace, task, pr_number, feedback, feedback_iteration)

                    feedback_result = await DBOS.recv_async(topic=TOPIC_JOB_RESULT, timeout_seconds=3600)

                    if feedback_result and feedback_result.get("status") == "completed":
                        notify_main_thread(
                            task.user_id, task_id, "worker_result",
                            {"status": "feedback_addressed", "result": {"pr_url": pr_url}},
                        )

            last_check = datetime.now(timezone.utc)

        return {"status": "completed", "pr_url": pr_url}

    except Exception as e:
        logger.error(f"Worker task failed: {e}")
        await update_task_status(task_id, TaskStatus.FAILED, error=str(e))
        notify_main_thread(
            task.user_id, task_id, "worker_result",
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
                    await asyncio.sleep(2 ** attempt)
                else:
                    logger.error(f"Failed to cleanup namespace after {cleanup_attempts} attempts: {e}")
