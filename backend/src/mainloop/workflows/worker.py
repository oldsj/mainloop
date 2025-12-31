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
    create_github_issue,
    update_github_issue,
    add_issue_comment,
    get_comment_reactions,
    # Question/plan approval support
    format_questions_for_issue,
    format_plan_for_issue,
    parse_question_answers_from_comment,
    parse_plan_approval_from_comment,
)

logger = logging.getLogger(__name__)

# Topics for DBOS messaging
TOPIC_JOB_RESULT = "job_result"  # Results from K8s Jobs
TOPIC_PLAN_RESPONSE = "plan_response"  # User approval/revision of plan from inbox
TOPIC_QUESTION_RESPONSE = "question_response"  # User answers to agent questions
TOPIC_START_IMPLEMENTATION = "start_implementation"  # User triggers implementation after plan approval

# Polling intervals (seconds)
ISSUE_POLL_INTERVAL = 60  # Plan issues - less urgent, rate-limit friendly
PR_POLL_INTERVAL = 30     # Implementation PRs - more urgent during CI

# Plan review timeout (wait for user to approve/revise plan)
PLAN_REVIEW_TIMEOUT_SECONDS = 86400  # 24 hours

# Retry configuration
MAX_JOB_RETRIES = 5  # Max retry attempts for failed jobs
JOB_TIMEOUT_SECONDS = 3600  # 1 hour timeout per job attempt


def _build_issue_body(
    original_prompt: str,
    task_id: str,
    requirements: dict[str, str] | None = None,
    plan_text: str | None = None,
    status: str = "Planning",
) -> str:
    """Build the GitHub issue body with evolving sections.

    The issue body is structured with clear sections that get filled in
    as the planning process progresses.
    """
    sections = []

    # Original request section
    sections.append(f"""## Original Request

> {original_prompt}
""")

    # Requirements section (filled in after questions are answered)
    if requirements:
        req_lines = "\n".join([f"- **{k}**: {v}" for k, v in requirements.items()])
        sections.append(f"""## Requirements

{req_lines}
""")

    # Plan section (filled in when plan is ready)
    if plan_text:
        sections.append(f"""## Implementation Plan

{plan_text}
""")

    # Footer
    sections.append(f"""---

_Task ID: `{task_id}`_ | _Status: {status}_
_Managed by [Mainloop](https://github.com/oldsj/mainloop)_
""")

    return "\n".join(sections)


def _generate_issue_title(description: str, max_length: int = 70) -> str:
    """Generate a brief, intelligent issue title from a task description.

    Extracts the core intent and truncates at word boundaries.
    """
    # Take first line only
    first_line = description.split("\n")[0].strip()

    # Remove common prefixes that add noise
    prefixes_to_remove = [
        "please ", "can you ", "could you ", "i want to ", "i need to ",
        "help me ", "i'd like to ", "let's ", "we should ", "we need to ",
    ]
    lower = first_line.lower()
    for prefix in prefixes_to_remove:
        if lower.startswith(prefix):
            first_line = first_line[len(prefix):]
            break

    # Capitalize first letter
    if first_line:
        first_line = first_line[0].upper() + first_line[1:]

    # If already short enough, return it
    if len(first_line) <= max_length:
        return first_line

    # Truncate at word boundary
    truncated = first_line[:max_length]
    last_space = truncated.rfind(" ")
    if last_space > max_length // 2:
        truncated = truncated[:last_space]

    return truncated.rstrip(".,;:") + "..."


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
    pending_questions: list[dict] | None = None,
    plan_text: str | None = None,
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
        pending_questions=pending_questions,
        plan_text=plan_text,
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
async def update_github_issue_step(
    repo_url: str,
    issue_number: int,
    title: str | None = None,
    body: str | None = None,
    state: str | None = None,
    labels: list[str] | None = None,
) -> bool:
    """Update a GitHub issue."""
    return await update_github_issue(repo_url, issue_number, title, body, state, labels)


@DBOS.step()
async def add_issue_comment_step(
    repo_url: str,
    issue_number: int,
    body: str,
) -> bool:
    """Add a comment to a GitHub issue."""
    return await add_issue_comment(repo_url, issue_number, body)


@DBOS.step()
async def post_questions_to_issue(
    repo_url: str,
    issue_number: int,
    questions: list[dict],
) -> bool:
    """Format and post questions as a GitHub issue comment."""
    body = format_questions_for_issue(questions)
    return await add_issue_comment(repo_url, issue_number, body)


@DBOS.step()
async def check_issue_for_question_answers(
    repo_url: str,
    issue_number: int,
    questions: list[dict],
    since: datetime,
) -> dict[str, str] | None:
    """Check issue comments for answers to questions.

    Returns:
        Dict mapping question ID to answer, or None if no answers found.
    """
    response = await get_issue_comments(repo_url, issue_number, since=since)

    if response.not_modified or not response.data:
        return None

    # Check each comment for answers (newest first)
    sorted_comments = sorted(
        response.data,
        key=lambda c: c.get("created_at", ""),
        reverse=True,
    )

    for comment in sorted_comments:
        body = comment.get("body", "")
        answers = parse_question_answers_from_comment(body, questions)
        if answers:
            logger.info(f"Found answers in issue comment: {answers}")
            return answers

    return None


@DBOS.step()
async def post_plan_to_issue(
    repo_url: str,
    issue_number: int,
    plan_text: str,
) -> int:
    """Format and post plan as a GitHub issue comment with approval instructions.

    Returns:
        The comment ID (for checking reactions later), or 0 on failure.
    """
    body = format_plan_for_issue(plan_text)
    return await add_issue_comment(repo_url, issue_number, body, return_id=True)


@DBOS.step()
async def check_plan_comment_reactions(
    repo_url: str,
    comment_id: int,
) -> bool:
    """Check if the plan comment has approval reactions (+1, rocket, heart, hooray).

    Returns:
        True if approval reaction found, False otherwise.
    """
    if not comment_id:
        return False

    reactions = await get_comment_reactions(repo_url, comment_id)

    # Approval reactions
    approval_reactions = {"+1", "rocket", "heart", "hooray"}
    for reaction in reactions:
        if reaction in approval_reactions:
            logger.info(f"Found approval reaction on plan comment: {reaction}")
            return True

    return False


@DBOS.step()
async def check_issue_for_plan_approval(
    repo_url: str,
    issue_number: int,
    since: datetime,
) -> tuple[str, str | None] | None:
    """Check issue comments for plan approval or revision request.

    Returns:
        Tuple of (action, text) where action is "approve" or "revise",
        or None if no relevant comments found.
    """
    response = await get_issue_comments(repo_url, issue_number, since=since)

    if response.not_modified or not response.data:
        return None

    # Check each comment (newest first)
    sorted_comments = sorted(
        response.data,
        key=lambda c: c.get("created_at", ""),
        reverse=True,
    )

    for comment in sorted_comments:
        body = comment.get("body", "")
        result = parse_plan_approval_from_comment(body)
        if result:
            if result == "approve":
                logger.info("Found plan approval in issue comment")
                return ("approve", None)
            else:
                logger.info(f"Found revision request in issue comment: {result[:50]}...")
                return ("revise", result)

    return None


@DBOS.step()
async def create_github_issue_step(
    repo_url: str,
    title: str,
    body: str,
    labels: list[str] | None = None,
) -> dict | None:
    """Create a GitHub issue from the approved plan.

    Args:
        repo_url: GitHub repository URL
        title: Issue title (derived from task description)
        body: Issue body (the approved plan)
        labels: Optional labels to apply

    Returns:
        Dict with issue number, url, and title, or None if failed
    """
    result = await create_github_issue(repo_url, title, body, labels)
    if result:
        return {
            "number": result.number,
            "url": result.url,
            "title": result.title,
        }
    return None


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


async def run_job_with_retry(
    spawn_fn,
    job_name: str,
    max_retries: int = MAX_JOB_RETRIES,
) -> dict:
    """Run a job with exponential backoff retry on failure.

    Args:
        spawn_fn: Async function that spawns the job (already bound with args)
        job_name: Human-readable job name for logging
        max_retries: Maximum number of retry attempts

    Returns:
        The successful job result

    Raises:
        RuntimeError: If all retries are exhausted
    """
    last_error = None

    for attempt in range(1, max_retries + 1):
        logger.info(f"Spawning {job_name} (attempt {attempt}/{max_retries})...")
        await spawn_fn()

        # Wait for job result
        result = await DBOS.recv_async(topic=TOPIC_JOB_RESULT, timeout_seconds=JOB_TIMEOUT_SECONDS)

        if result is None:
            last_error = f"Timed out waiting for {job_name} to complete"
            logger.warning(f"{last_error} (attempt {attempt}/{max_retries})")
        elif result.get("status") == "failed":
            last_error = result.get("error", f"{job_name} failed")
            logger.warning(f"{job_name} failed: {last_error} (attempt {attempt}/{max_retries})")
        else:
            # Success!
            return result

        # If we have more retries, wait with exponential backoff
        if attempt < max_retries:
            backoff_seconds = 2 ** attempt  # 2, 4, 8, 16, 32 seconds
            logger.info(f"Retrying {job_name} in {backoff_seconds}s...")
            await DBOS.sleep_async(backoff_seconds)

    # All retries exhausted
    raise RuntimeError(f"{job_name} failed after {max_retries} attempts: {last_error}")


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

                    # Set status to implementing while Claude works
                    await update_worker_task_status(task_id, TaskStatus.IMPLEMENTING)

                    await run_job_with_retry(
                        lambda: spawn_feedback_job(task_id, namespace, task, pr_number, feedback, task.branch_name, feedback_iteration),
                        f"feedback job (iteration {feedback_iteration})",
                    )

                    # Set status back to under_review after job completes
                    await update_worker_task_status(task_id, TaskStatus.UNDER_REVIEW, pr_url=pr_url, pr_number=pr_number)
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


@DBOS.workflow()  # v2: Interactive plan review in inbox
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
        # Creates GitHub issue immediately, updates as plan evolves
        # Pauses after plan approval, waiting for user to trigger implementation
        # ============================================================
        if not task.skip_plan:
            await update_worker_task_status(task_id, TaskStatus.PLANNING)
            plan_iteration = 0
            plan_text: str | None = None
            suggested_options: list[str] = []
            user_answers: dict[str, str] = {}  # Accumulated answers to questions

            # Create GitHub issue immediately with original prompt
            issue_title = _generate_issue_title(task.description)
            initial_issue_body = _build_issue_body(
                original_prompt=task.description,
                task_id=task_id,
            )

            issue_result = await create_github_issue_step(
                task.repo_url,
                issue_title,
                initial_issue_body,
                labels=["mainloop-plan", "planning"],
            )

            if not issue_result:
                raise RuntimeError("Failed to create GitHub issue")

            issue_url = issue_result["url"]
            issue_number = issue_result["number"]
            logger.info(f"Created GitHub issue #{issue_number}: {issue_url}")

            # Store issue info immediately
            await update_worker_task_status(
                task_id, TaskStatus.PLANNING,
                issue_url=issue_url, issue_number=issue_number,
            )

            # Add comment that we're starting to explore
            await add_issue_comment_step(
                task.repo_url, issue_number,
                "🤖 Starting to explore the codebase and gather requirements..."
            )

            while True:
                plan_iteration += 1

                # Build feedback context from previous iteration or user answers
                feedback_context = None
                if plan_iteration > 1 and plan_text:
                    feedback_context = plan_text
                if user_answers:
                    # Format answers as context for the agent
                    answers_text = "\n".join([f"- {k}: {v}" for k, v in user_answers.items()])
                    if feedback_context:
                        feedback_context = f"{feedback_context}\n\nUser answered your questions:\n{answers_text}"
                    else:
                        feedback_context = f"User answered your questions:\n{answers_text}"

                # Run plan job with retry - returns plan content and any questions
                result = await run_job_with_retry(
                    lambda iteration=plan_iteration: spawn_plan_job(
                        task_id, namespace, task,
                        feedback=feedback_context,
                        iteration=iteration
                    ),
                    f"plan job (iteration {plan_iteration})",
                )

                # Extract plan content and questions from result
                plan_result = result.get("result", {})
                plan_text = plan_result.get("plan_text")
                questions = plan_result.get("questions", [])
                suggested_options = plan_result.get("suggested_options", [])

                if not plan_text:
                    raise RuntimeError("Plan job did not return plan content")

                logger.info(f"Plan ready with {len(questions)} questions, {len(suggested_options)} options")

                # PHASE 1a: If agent asked questions, get user answers first
                if questions:
                    logger.info(f"Agent asked {len(questions)} questions - waiting for user answers")

                    # Update task with questions
                    await update_worker_task_status(
                        task_id, TaskStatus.WAITING_QUESTIONS,
                        pending_questions=questions,
                        plan_text=plan_text,
                    )

                    # Post formatted questions to GitHub issue
                    await post_questions_to_issue(task.repo_url, issue_number, questions)

                    # Notify main thread about questions (task UI will show them)
                    notify_main_thread(
                        task.user_id,
                        task_id,
                        "worker_result",
                        {
                            "status": "questions",
                            "result": {
                                "questions": questions,
                                "plan_text": plan_text,
                                "issue_url": issue_url,
                                "message": "Please answer these questions to continue",
                            },
                        },
                    )

                    # Poll for answers with exponential backoff
                    # Check both: 1) DBOS message from UI, 2) GitHub issue comments
                    # Start at 10s, max out at 5 minutes
                    poll_interval = 10  # seconds
                    max_poll_interval = 300  # 5 minutes
                    questions_posted_at = datetime.now(timezone.utc)
                    total_wait = 0
                    response = None
                    answer_source = None

                    while total_wait < PLAN_REVIEW_TIMEOUT_SECONDS:
                        # First, check for DBOS message (non-blocking with short timeout)
                        response = await DBOS.recv_async(
                            topic=TOPIC_QUESTION_RESPONSE,
                            timeout_seconds=poll_interval,
                        )

                        if response is not None:
                            answer_source = "ui"
                            logger.info("Received answers via UI")
                            break

                        # No UI response - check GitHub issue comments
                        gh_answers = await check_issue_for_question_answers(
                            task.repo_url, issue_number, questions, questions_posted_at
                        )

                        if gh_answers:
                            # Found answers in GitHub comments!
                            response = {"answers": gh_answers, "action": "answer"}
                            answer_source = "github"
                            logger.info(f"Found answers in GitHub issue comment: {gh_answers}")
                            break

                        # No answers yet - increase poll interval (exponential backoff)
                        total_wait += poll_interval
                        poll_interval = min(poll_interval * 1.5, max_poll_interval)
                        logger.debug(f"No answers yet, next poll in {poll_interval:.0f}s (total wait: {total_wait}s)")

                    if response is None:
                        raise RuntimeError("Question response timed out after 24 hours")

                    if response.get("action") == "cancel":
                        logger.info("Task cancelled by user during questions")
                        await update_worker_task_status(task_id, TaskStatus.CANCELLED)
                        await add_issue_comment_step(
                            task.repo_url, issue_number,
                            "❌ Task cancelled by user."
                        )
                        await update_github_issue_step(
                            task.repo_url, issue_number, state="closed"
                        )
                        notify_main_thread(
                            task.user_id, task_id, "worker_result",
                            {"status": "cancelled", "result": {"message": "Cancelled by user"}},
                        )
                        return {"status": "cancelled", "message": "Cancelled by user"}

                    # Store answers and update issue with Q&A
                    user_answers = response.get("answers", {})
                    logger.info(f"User answered {len(user_answers)} questions via {answer_source}, re-running plan...")

                    # Update issue body with requirements
                    updated_body = _build_issue_body(
                        original_prompt=task.description,
                        requirements=user_answers,
                        task_id=task_id,
                    )
                    await update_github_issue_step(
                        task.repo_url, issue_number, body=updated_body
                    )

                    # Post confirmation comment (mention source if from GitHub)
                    if answer_source == "github":
                        await add_issue_comment_step(
                            task.repo_url, issue_number,
                            "✅ Got your answers! Generating implementation plan..."
                        )
                    else:
                        await add_issue_comment_step(
                            task.repo_url, issue_number,
                            "✅ Requirements gathered. Generating implementation plan..."
                        )

                    # Clear questions from task since they've been answered
                    await update_worker_task_status(
                        task_id, TaskStatus.PLANNING,
                        pending_questions=[],  # Empty list clears questions
                    )
                    continue  # Re-run plan job with answers

                # PHASE 1b: No questions - proceed to plan review
                # Update issue body with the plan
                updated_body = _build_issue_body(
                    original_prompt=task.description,
                    requirements=user_answers,
                    plan_text=plan_text,
                    task_id=task_id,
                )
                await update_github_issue_step(
                    task.repo_url, issue_number, body=updated_body
                )

                # Post plan as comment with approval instructions (get comment ID for reactions)
                plan_comment_id = await post_plan_to_issue(task.repo_url, issue_number, plan_text)

                # Update status to waiting for plan review
                await update_worker_task_status(
                    task_id, TaskStatus.WAITING_PLAN_REVIEW,
                    plan_text=plan_text,
                )

                # Notify main thread - task UI will show plan for review
                notify_main_thread(
                    task.user_id,
                    task_id,
                    "worker_result",
                    {
                        "status": "plan_review",
                        "result": {
                            "plan_text": plan_text,
                            "suggested_options": suggested_options,
                            "issue_url": issue_url,
                            "message": "Plan ready for review",
                        },
                    },
                )

                # Poll for approval with exponential backoff
                # Check: 1) DBOS message from UI, 2) GitHub comments, 3) Reactions on plan comment
                poll_interval = 10  # seconds
                max_poll_interval = 300  # 5 minutes
                plan_posted_at = datetime.now(timezone.utc)
                total_wait = 0
                response = None
                approval_source = None

                logger.info(f"Waiting for plan approval via UI, GitHub comment, or reaction...")

                while total_wait < PLAN_REVIEW_TIMEOUT_SECONDS:
                    # First, check for DBOS message (non-blocking with short timeout)
                    response = await DBOS.recv_async(
                        topic=TOPIC_PLAN_RESPONSE,
                        timeout_seconds=poll_interval,
                    )

                    if response is not None:
                        approval_source = "ui"
                        logger.info("Received plan response via UI")
                        break

                    # Check for approval reaction (👍, 🚀, etc.) on plan comment
                    if plan_comment_id:
                        has_approval_reaction = await check_plan_comment_reactions(
                            task.repo_url, plan_comment_id
                        )
                        if has_approval_reaction:
                            response = {"action": "approve", "text": ""}
                            approval_source = "github_reaction"
                            logger.info("Found approval reaction on plan comment")
                            break

                    # No UI response - check GitHub issue comments
                    gh_response = await check_issue_for_plan_approval(
                        task.repo_url, issue_number, plan_posted_at
                    )

                    if gh_response:
                        action, text = gh_response
                        response = {"action": action, "text": text or ""}
                        approval_source = "github"
                        logger.info(f"Found plan response in GitHub comment: {action}")
                        break

                    # No response yet - increase poll interval (exponential backoff)
                    total_wait += poll_interval
                    poll_interval = min(poll_interval * 1.5, max_poll_interval)
                    logger.debug(f"No plan response yet, next poll in {poll_interval:.0f}s")

                if response is None:
                    raise RuntimeError("Plan review timed out after 24 hours")

                response_action = response.get("action", "")
                response_text = response.get("text", "")

                if response_action == "approve" or response_action.lower() in ["approve", "lgtm", "looks good"]:
                    logger.info(f"Plan approved via {approval_source}! Waiting for user to start implementation...")
                    if approval_source in ("github", "github_reaction"):
                        await add_issue_comment_step(
                            task.repo_url, issue_number,
                            "✅ Got it! Plan approved."
                        )
                    break
                elif response_action == "cancel":
                    logger.info("Plan cancelled by user")
                    await update_worker_task_status(task_id, TaskStatus.CANCELLED)
                    await add_issue_comment_step(
                        task.repo_url, issue_number,
                        "❌ Plan cancelled by user."
                    )
                    await update_github_issue_step(
                        task.repo_url, issue_number, state="closed"
                    )
                    notify_main_thread(
                        task.user_id, task_id, "worker_result",
                        {"status": "cancelled", "result": {"message": "Plan cancelled by user"}},
                    )
                    return {"status": "cancelled", "message": "Plan cancelled by user"}
                else:
                    # User provided feedback - re-run plan with feedback
                    feedback = response_text or response_action
                    logger.info(f"User requested plan revision via {approval_source}: {feedback}")
                    if approval_source == "github":
                        await add_issue_comment_step(
                            task.repo_url, issue_number,
                            f"📝 Got your feedback. Regenerating plan..."
                        )
                    else:
                        await add_issue_comment_step(
                            task.repo_url, issue_number,
                            f"📝 **Revision requested:**\n> {feedback}\n\nRegenerating plan..."
                        )
                    plan_text = feedback  # Pass feedback to next iteration
                    user_answers = {}  # Clear answers for new iteration
                    continue

            # Plan approved - update issue and pause for implementation trigger
            await add_issue_comment_step(
                task.repo_url, issue_number,
                "✅ **Plan approved!** Ready to start implementation when triggered."
            )

            # Update issue labels
            await update_github_issue_step(
                task.repo_url, issue_number,
                labels=["mainloop-plan", "approved"],
            )

            # Generate intelligent branch name
            branch_name = await generate_branch_name_step(
                issue_number, task.description[:50], task.task_type
            )
            logger.info(f"Generated branch name: {branch_name}")

            # Set status to ready_to_implement and PAUSE
            await update_worker_task_status(
                task_id, TaskStatus.READY_TO_IMPLEMENT,
                issue_url=issue_url, issue_number=issue_number,
                branch_name=branch_name, plan_text=plan_text,
            )

            # Notify that plan is approved and waiting for implementation trigger
            notify_main_thread(
                task.user_id,
                task_id,
                "worker_result",
                {
                    "status": "ready_to_implement",
                    "result": {
                        "issue_url": issue_url,
                        "plan_text": plan_text,
                        "message": "Plan approved. Click 'Start Implementation' when ready.",
                    },
                },
            )

            # Wait for user to trigger implementation (24 hour timeout)
            logger.info("Waiting for user to trigger implementation...")
            impl_response = await DBOS.recv_async(
                topic=TOPIC_START_IMPLEMENTATION,
                timeout_seconds=PLAN_REVIEW_TIMEOUT_SECONDS,
            )

            if impl_response is None:
                raise RuntimeError("Implementation trigger timed out after 24 hours")

            if impl_response.get("action") == "cancel":
                logger.info("Implementation cancelled by user")
                await update_worker_task_status(task_id, TaskStatus.CANCELLED)
                await add_issue_comment_step(
                    task.repo_url, issue_number,
                    "❌ Implementation cancelled by user."
                )
                await update_github_issue_step(
                    task.repo_url, issue_number, state="closed"
                )
                notify_main_thread(
                    task.user_id, task_id, "worker_result",
                    {"status": "cancelled", "result": {"message": "Cancelled by user"}},
                )
                return {"status": "cancelled", "message": "Cancelled by user"}

            logger.info("Implementation triggered! Proceeding to Phase 2...")
            await add_issue_comment_step(
                task.repo_url, issue_number,
                "🚀 **Starting implementation...**"
            )

        # ============================================================
        # PHASE 2: IMPLEMENTATION
        # Creates a PR that references the plan issue
        # ============================================================
        await update_worker_task_status(task_id, TaskStatus.IMPLEMENTING)

        if task.skip_plan:
            # Skip plan mode: create PR directly with implementation
            impl_result = await run_job_with_retry(
                lambda: spawn_implement_job(task_id, namespace, task),
                "implement job (skip plan)",
            )
        else:
            # Normal mode: implement the approved plan
            impl_result = await run_job_with_retry(
                lambda: spawn_implement_job(task_id, namespace, task, issue_number=issue_number, branch_name=branch_name),
                f"implement job (issue #{issue_number})",
            )

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
                await run_job_with_retry(
                    lambda: spawn_fix_job(task_id, namespace, task, pr_number, failure_logs, branch_name, ci_iteration),
                    f"fix job (CI iteration {ci_iteration})",
                )

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

                    # Set status to implementing while Claude works
                    await update_worker_task_status(task_id, TaskStatus.IMPLEMENTING)

                    await run_job_with_retry(
                        lambda: spawn_feedback_job(task_id, namespace, task, pr_number, feedback, branch_name, feedback_iteration),
                        f"feedback job (iteration {feedback_iteration})",
                    )

                    # Set status back to under_review after job completes
                    await update_worker_task_status(task_id, TaskStatus.UNDER_REVIEW, pr_url=pr_url, pr_number=pr_number)
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
