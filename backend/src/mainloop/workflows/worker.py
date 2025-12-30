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
    get_check_status,
    get_check_failure_logs,
    acknowledge_comments,
    # Issue support
    get_issue_status,
    get_issue_comments,
    format_issue_feedback_for_agent,
    generate_branch_name,
    parse_comments_for_command,
)

logger = logging.getLogger(__name__)

# Topic for receiving results from K8s Jobs
TOPIC_JOB_RESULT = "job_result"

# Polling intervals (seconds)
ISSUE_POLL_INTERVAL = 60  # Plan issues - less urgent, rate-limit friendly
PR_POLL_INTERVAL = 30     # Implementation PRs - more urgent during CI


@DBOS.step()
async def load_worker_task(task_id: str) -> WorkerTask | None:
    """Load worker task from database."""
    return await db.get_worker_task(task_id)


@DBOS.step()
async def update_worker_task_status(
    task_id: str,
    status: TaskStatus,
    issue_url: str | None = None,
    issue_number: int | None = None,
    pr_url: str | None = None,
    pr_number: int | None = None,
    branch_name: str | None = None,
    error: str | None = None,
) -> None:
    """Update task status in database."""
    await db.update_worker_task(
        task_id,
        status=status,
        issue_url=issue_url,
        issue_number=issue_number,
        pr_url=pr_url,
        pr_number=pr_number,
        branch_name=branch_name,
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
    """Spawn a Job to create the implementation plan (GitHub issue)."""
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
    issue_number: int | None = None,
    branch_name: str | None = None,
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
        issue_number=issue_number,
        branch_name=branch_name,
    )

    return job_name


@DBOS.step()
async def spawn_feedback_job(
    task_id: str,
    namespace: str,
    task: WorkerTask,
    pr_number: int,
    feedback: str,
    branch_name: str | None = None,
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
        branch_name=branch_name,
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
async def check_issue_status_step(
    repo_url: str,
    issue_number: int,
    etag: str | None = None,
) -> dict:
    """Check the current status of an issue with conditional request support."""
    response = await get_issue_status(repo_url, issue_number, etag=etag)

    if response.not_modified:
        return {"not_modified": True}

    if not response.data or response.data.get("state") == "not_found":
        return {"state": "not_found"}

    return {
        "not_modified": False,
        "state": response.data["state"],
        "title": response.data["title"],
        "updated_at": response.data["updated_at"],
        "etag": response.etag,
    }


@DBOS.step()
async def check_for_new_issue_comments(
    repo_url: str,
    issue_number: int,
    since: datetime,
    etag: str | None = None,
) -> dict:
    """Check for new comments on an issue with conditional request support."""
    response = await get_issue_comments(repo_url, issue_number, since=since, etag=etag)

    if response.not_modified:
        return {"not_modified": True, "comments": []}

    return {
        "not_modified": False,
        "comments": response.data or [],
        "etag": response.etag,
    }


@DBOS.step()
async def get_issue_feedback_context(
    repo_url: str,
    issue_number: int,
    since: datetime,
) -> str:
    """Get formatted issue feedback for the agent."""
    return await format_issue_feedback_for_agent(repo_url, issue_number, since=since)


@DBOS.step()
async def generate_branch_name_step(
    issue_number: int,
    title: str,
    task_type: str,
) -> str:
    """Generate an intelligent branch name from issue metadata."""
    return generate_branch_name(issue_number, title, task_type)


@DBOS.step()
async def update_task_etag(
    task_id: str,
    issue_etag: str | None = None,
    pr_etag: str | None = None,
) -> None:
    """Update the stored ETag for polling."""
    await db.update_worker_task(
        task_id,
        issue_etag=issue_etag,
        pr_etag=pr_etag,
    )


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
    """Get formatted feedback for the agent and acknowledge comments."""
    # Get comments first to acknowledge them
    from mainloop.services.github_pr import get_pr_comments, _should_agent_act_on_comment

    comments = await get_pr_comments(repo_url, pr_number, since=since)
    actionable_comments = [c for c in comments if _should_agent_act_on_comment(c)]

    # Add 👀 reaction to acknowledge we've seen the comments
    if actionable_comments:
        await acknowledge_comments(repo_url, actionable_comments)

    return await format_feedback_for_agent(repo_url, pr_number, since=since)


@DBOS.step()
async def cleanup_namespace(task_id: str) -> None:
    """Delete the task namespace."""
    await delete_task_namespace(task_id)


@DBOS.step()
async def get_check_status_step(repo_url: str, pr_number: int) -> dict:
    """Get combined status of GitHub Actions checks for a PR."""
    status = await get_check_status(repo_url, pr_number)
    return {
        "status": status.status,
        "total_count": status.total_count,
        "failed_count": len(status.failed_runs),
        "failed_names": [r.name for r in status.failed_runs],
    }


@DBOS.step()
async def get_check_failure_logs_step(repo_url: str, pr_number: int) -> str:
    """Get formatted failure logs from failed check runs."""
    return await get_check_failure_logs(repo_url, pr_number)


@DBOS.step()
async def spawn_fix_job(
    task_id: str,
    namespace: str,
    task: WorkerTask,
    pr_number: int,
    failure_logs: str,
    branch_name: str | None = None,
    iteration: int = 1,
) -> str:
    """Spawn a Job to fix CI failures."""
    callback_url = f"{settings.backend_internal_url}/internal/tasks/{task_id}/complete"

    job_name = await create_worker_job(
        task_id=task_id,
        namespace=namespace,
        prompt=task.description,
        mode="fix",
        callback_url=callback_url,
        model=task.model,
        repo_url=task.repo_url,
        pr_number=pr_number,
        branch_name=branch_name,
        feedback_context=failure_logs,
        iteration=iteration,
    )

    return job_name


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


async def _run_code_review_loop(
    task_id: str,
    task: Any,
    namespace: str,
    pr_url: str,
    pr_number: int,
    since: datetime | None = None,
) -> dict[str, Any]:
    """Run the code review loop for an existing PR.

    This is used both by the main workflow and when resuming a task
    that already has a PR.

    Args:
        since: Check for comments from this time. If None, uses task.created_at
               for resumed tasks, or now for new PRs.
    """
    try:
        logger.info(f"Starting code review loop for PR #{pr_number}")
        # For resumed tasks, check from task creation time to catch missed comments
        last_check = since or task.created_at.replace(tzinfo=timezone.utc)
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
                await update_worker_task_status(task_id, TaskStatus.COMPLETED)
                notify_main_thread(
                    task.user_id, task_id, "worker_result",
                    {"status": "completed", "result": {"pr_url": pr_url, "merged": True}},
                )
                return {"status": "completed", "pr_url": pr_url, "merged": True}

            if pr_status["state"] == "closed":
                logger.info(f"PR #{pr_number} was closed without merge")
                await update_worker_task_status(task_id, TaskStatus.CANCELLED)
                notify_main_thread(
                    task.user_id, task_id, "worker_result",
                    {"status": "cancelled", "result": {"pr_url": pr_url, "closed": True}},
                )
                return {"status": "cancelled", "pr_url": pr_url}

            # Check for new comments
            new_comments = await check_for_new_comments(task.repo_url, pr_number, last_check)

            if new_comments:
                logger.info(f"Found {len(new_comments)} new comments on PR #{pr_number}")
                feedback = await get_feedback_context(task.repo_url, pr_number, last_check)

                if feedback:
                    feedback_iteration += 1
                    logger.info(f"Spawning feedback job (iteration {feedback_iteration})...")

                    # Set status to implementing while Claude works
                    await update_worker_task_status(task_id, TaskStatus.IMPLEMENTING)

                    await spawn_feedback_job(task_id, namespace, task, pr_number, feedback, task.branch_name, feedback_iteration)

                    feedback_result = await DBOS.recv_async(topic=TOPIC_JOB_RESULT, timeout_seconds=3600)

                    # Set status back to under_review after job completes
                    await update_worker_task_status(task_id, TaskStatus.UNDER_REVIEW, pr_url=pr_url, pr_number=pr_number)

                    if feedback_result and feedback_result.get("status") == "completed":
                        notify_main_thread(
                            task.user_id, task_id, "worker_result",
                            {"status": "feedback_addressed", "result": {"pr_url": pr_url}},
                        )

            last_check = datetime.now(timezone.utc)

        return {"status": "completed", "pr_url": pr_url}

    except Exception as e:
        logger.error(f"Code review loop failed: {e}")
        await update_worker_task_status(task_id, TaskStatus.FAILED, error=str(e))
        notify_main_thread(
            task.user_id, task_id, "worker_result",
            {"status": "failed", "error": str(e)},
        )
        return {"status": "failed", "error": str(e)}

    finally:
        # Cleanup namespace
        logger.info(f"Cleaning up namespace: {namespace}")
        try:
            await cleanup_namespace(task_id)
        except Exception as e:
            logger.warning(f"Cleanup failed: {e}")


@DBOS.workflow()
async def worker_task_workflow(task_id: str) -> dict[str, Any]:
    """
    Worker workflow that executes a task in an isolated K8s namespace.

    This workflow follows a plan-first approach:
    1. Creates an isolated namespace for the task
    2. PLAN PHASE: Spawns a Job to create a GitHub issue with implementation plan
    3. Polls for plan approval (user comments "looks good", "lgtm", etc.)
    4. IMPLEMENT PHASE: Spawns a Job to implement the approved plan, creates PR
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

    # Check if task already has a PR - if so, skip to code review
    if task.pr_url and task.pr_number:
        logger.info(f"Task already has PR #{task.pr_number}, skipping to code review")
        pr_url = task.pr_url
        pr_number = task.pr_number

        # Setup namespace for any feedback jobs
        namespace = await setup_namespace(task_id)

        try:
            # Jump directly to code review loop
            await update_worker_task_status(task_id, TaskStatus.UNDER_REVIEW, pr_url=pr_url, pr_number=pr_number)
        except Exception as e:
            logger.error(f"Failed to resume task: {e}")
            await cleanup_namespace(task_id)
            raise

        # Go to code review loop (PHASE 3)
        return await _run_code_review_loop(task_id, task, namespace, pr_url, pr_number)

    # Setup namespace
    logger.info(f"Creating namespace for task: {task_id}")
    namespace = await setup_namespace(task_id)

    try:
        issue_url: str | None = None
        issue_number: int | None = None
        branch_name: str | None = None
        pr_url: str | None = None
        pr_number: int | None = None
        plan_iteration = 0

        # ============================================================
        # PHASE 1: PLANNING (unless skip_plan is True)
        # Creates a GitHub ISSUE with the implementation plan
        # ============================================================
        if not task.skip_plan:
            await update_worker_task_status(task_id, TaskStatus.PLANNING)
            logger.info(f"Spawning plan job in namespace: {namespace}")
            await spawn_plan_job(task_id, namespace, task, iteration=1)

            # Wait for plan job to complete
            logger.info("Waiting for plan job to complete...")
            result = await DBOS.recv_async(topic=TOPIC_JOB_RESULT, timeout_seconds=3600)

            if result is None:
                raise RuntimeError("Timed out waiting for plan job to complete")
            if result.get("status") == "failed":
                raise RuntimeError(result.get("error", "Plan job failed"))

            # Extract ISSUE URL from result (not PR)
            issue_url = result.get("result", {}).get("issue_url")
            if not issue_url:
                raise RuntimeError("Plan job did not create a GitHub issue")

            issue_number = int(issue_url.split("/")[-1])

            # Get issue details for branch name generation
            issue_status = await check_issue_status_step(task.repo_url, issue_number)
            issue_title = issue_status.get("title", task.description[:50])
            current_etag = issue_status.get("etag")

            # Generate intelligent branch name
            branch_name = await generate_branch_name_step(
                issue_number, issue_title, task.task_type
            )
            logger.info(f"Generated branch name: {branch_name}")

            # Store issue info and branch name
            await update_worker_task_status(
                task_id, TaskStatus.WAITING_PLAN_REVIEW,
                issue_url=issue_url, issue_number=issue_number,
                branch_name=branch_name
            )

            # Notify human that plan is ready for review
            notify_main_thread(
                task.user_id,
                task_id,
                "worker_result",
                {
                    "status": "plan_ready",
                    "result": {"issue_url": issue_url, "message": "Plan ready for review"},
                },
            )

            # Poll for plan approval on ISSUE (with ETag for rate limiting)
            logger.info(f"Waiting for plan approval on issue #{issue_number}")
            last_check = datetime.now(timezone.utc)
            plan_iteration = 0

            while True:
                await DBOS.sleep_async(ISSUE_POLL_INTERVAL)  # 60 seconds for issues

                # Check issue status with ETag (conditional request)
                issue_status = await check_issue_status_step(
                    task.repo_url, issue_number, etag=current_etag
                )

                if issue_status.get("not_modified"):
                    # No changes - skip comment check too (saves API calls)
                    continue

                # Update stored ETag
                if issue_status.get("etag"):
                    current_etag = issue_status["etag"]
                    await update_task_etag(task_id, issue_etag=current_etag)

                if issue_status.get("state") == "not_found":
                    raise RuntimeError(f"Issue #{issue_number} not found")

                if issue_status["state"] == "closed":
                    await update_worker_task_status(task_id, TaskStatus.CANCELLED)
                    notify_main_thread(
                        task.user_id, task_id, "worker_result",
                        {"status": "cancelled", "result": {"issue_url": issue_url, "closed": True}},
                    )
                    return {"status": "cancelled", "issue_url": issue_url}

                # Check for new comments (only if issue was modified)
                comments_result = await check_for_new_issue_comments(
                    task.repo_url, issue_number, last_check
                )

                if comments_result.get("comments"):
                    comments = comments_result["comments"]

                    # Parse for slash commands (/implement, /revise)
                    command = parse_comments_for_command(comments)

                    if command.command == "implement":
                        logger.info(f"Plan approved via /implement by {command.user}! Proceeding to implementation...")
                        break
                    elif command.command == "revise":
                        # User wants changes - use feedback from command
                        logger.info(f"User requested plan revision via /revise: {command.feedback}")
                        plan_iteration += 1
                        await spawn_plan_job(task_id, namespace, task, command.feedback, plan_iteration)
                        plan_result = await DBOS.recv_async(topic=TOPIC_JOB_RESULT, timeout_seconds=3600)
                        if plan_result and plan_result.get("status") == "completed":
                            notify_main_thread(
                                task.user_id, task_id, "worker_result",
                                {"status": "plan_updated", "result": {"issue_url": issue_url}},
                            )
                    # No recognized command - ignore general discussion

                last_check = datetime.now(timezone.utc)

        # ============================================================
        # PHASE 2: IMPLEMENTATION
        # Creates a PR that references the plan issue
        # ============================================================
        await update_worker_task_status(task_id, TaskStatus.IMPLEMENTING)

        if task.skip_plan:
            # Skip plan mode: create PR directly with implementation
            logger.info("Skip plan mode: spawning implement job to create PR")
            await spawn_implement_job(task_id, namespace, task)
        else:
            # Normal mode: implement the approved plan
            logger.info(f"Spawning implement job for approved plan (issue #{issue_number}, branch: {branch_name})")
            await spawn_implement_job(
                task_id, namespace, task,
                issue_number=issue_number,
                branch_name=branch_name
            )

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

        # ============================================================
        # PHASE 2.5: CI VERIFICATION LOOP
        # ============================================================
        # Wait for GitHub Actions to pass before entering code review
        logger.info(f"Waiting for GitHub Actions to complete on PR #{pr_number}")
        ci_iteration = 0
        MAX_CI_ITERATIONS = 5  # Limit fix attempts

        while ci_iteration < MAX_CI_ITERATIONS:
            await DBOS.sleep_async(PR_POLL_INTERVAL)

            check_status = await get_check_status_step(task.repo_url, pr_number)

            if check_status["status"] == "success":
                logger.info("All CI checks passed! Continuing to code review...")
                break
            elif check_status["status"] == "failure":
                ci_iteration += 1
                failed_names = check_status.get("failed_names", [])
                logger.info(
                    f"CI checks failed ({', '.join(failed_names)}), "
                    f"spawning fix job (iteration {ci_iteration})..."
                )

                # Notify human about CI failure
                notify_main_thread(
                    task.user_id,
                    task_id,
                    "worker_result",
                    {
                        "status": "ci_failed",
                        "result": {
                            "pr_url": pr_url,
                            "failed_checks": failed_names,
                            "iteration": ci_iteration,
                        },
                    },
                )

                failure_logs = await get_check_failure_logs_step(task.repo_url, pr_number)
                await spawn_fix_job(task_id, namespace, task, pr_number, failure_logs, branch_name, ci_iteration)

                fix_result = await DBOS.recv_async(topic=TOPIC_JOB_RESULT, timeout_seconds=3600)
                if fix_result is None:
                    raise RuntimeError("Timed out waiting for fix job to complete")
                if fix_result.get("status") == "failed":
                    raise RuntimeError(fix_result.get("error", "Fix job failed"))

                # After fix, wait a bit for new checks to start
                await DBOS.sleep_async(30)
            else:
                # Pending - continue waiting
                continue

        if ci_iteration >= MAX_CI_ITERATIONS:
            raise RuntimeError(f"CI checks still failing after {MAX_CI_ITERATIONS} fix attempts")

        await update_worker_task_status(task_id, TaskStatus.UNDER_REVIEW, pr_url=pr_url, pr_number=pr_number)

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
                await update_worker_task_status(task_id, TaskStatus.COMPLETED)
                notify_main_thread(
                    task.user_id, task_id, "worker_result",
                    {"status": "completed", "result": {"pr_url": pr_url, "merged": True}},
                )
                break

            if pr_status["state"] == "closed":
                logger.info(f"PR #{pr_number} was closed without merge")
                await update_worker_task_status(task_id, TaskStatus.CANCELLED)
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

                    # Set status to implementing while Claude works
                    await update_worker_task_status(task_id, TaskStatus.IMPLEMENTING)

                    await spawn_feedback_job(task_id, namespace, task, pr_number, feedback, branch_name, feedback_iteration)

                    feedback_result = await DBOS.recv_async(topic=TOPIC_JOB_RESULT, timeout_seconds=3600)

                    # Set status back to under_review after job completes
                    await update_worker_task_status(task_id, TaskStatus.UNDER_REVIEW, pr_url=pr_url, pr_number=pr_number)

                    if feedback_result and feedback_result.get("status") == "completed":
                        notify_main_thread(
                            task.user_id, task_id, "worker_result",
                            {"status": "feedback_addressed", "result": {"pr_url": pr_url}},
                        )

            last_check = datetime.now(timezone.utc)

        return {"status": "completed", "pr_url": pr_url}

    except Exception as e:
        logger.error(f"Worker task failed: {e}")
        await update_worker_task_status(task_id, TaskStatus.FAILED, error=str(e))
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
