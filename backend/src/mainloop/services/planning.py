"""In-thread planning service.

Handles synchronous planning sessions in the main conversation thread.
Planning uses Claude Agent SDK with codebase tools pointed at a cached repo.
"""

import logging
from datetime import datetime, timezone

from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    ResultMessage,
    SystemMessage,
    TextBlock,
    query,
)
from dbos import SetWorkflowID
from mainloop.db import db
from mainloop.services.github_pr import create_github_issue
from mainloop.services.repo_cache import get_repo_cache
from mainloop.services.task_router import extract_keywords
from mainloop.workflows.dbos_config import worker_queue

from models import (
    PlanningSession,
    PlanningSessionStatus,
    TaskStatus,
    WorkerTask,
)

logger = logging.getLogger(__name__)


def build_planning_system_prompt(repo_url: str, task_description: str) -> str:
    """Build the system prompt for planning mode."""
    return f"""You are helping plan an implementation for a coding task.

## Task Description
{task_description}

## Repository
{repo_url}

## Your Role
You are in planning mode. Your job is to:
1. Explore the codebase to understand the existing architecture and patterns
2. Design an implementation approach for the task
3. Create a clear, actionable plan

## Tools Available
You have read-only access to the codebase via:
- Read: Read file contents
- Glob: Find files by pattern (e.g., "**/*.py", "src/**/*.ts")
- Grep: Search file contents with regex
- LS: List directory contents

## Output Format
When you've finished exploring and are ready to present your plan, clearly state:
1. **Summary**: Brief overview of the approach
2. **Files to Modify**: List of files that need changes
3. **Implementation Steps**: Numbered steps to complete the task
4. **Considerations**: Any trade-offs, risks, or alternatives

After presenting the plan, ask the user if they want to:
- Approve the plan and create a GitHub issue
- Request changes to the plan
- Cancel planning

Do NOT start implementing - just create the plan. The actual implementation will happen after approval.
"""


async def start_planning_session(
    user_id: str,
    main_thread_id: str,
    conversation_id: str,
    repo_url: str,
    task_description: str,
) -> tuple[PlanningSession, str]:
    """Start a new planning session.

    Args:
        user_id: User ID
        main_thread_id: Main thread ID
        conversation_id: Conversation ID
        repo_url: GitHub repository URL
        task_description: What the user wants to accomplish

    Returns:
        Tuple of (PlanningSession, initial_message)

    """
    # Validate repo URL
    if not repo_url.startswith("https://github.com/"):
        raise ValueError(f"Invalid GitHub repo URL: {repo_url}")

    # Create planning session
    session = PlanningSession(
        user_id=user_id,
        conversation_id=conversation_id,
        main_thread_id=main_thread_id,
        repo_url=repo_url,
        task_description=task_description,
    )
    session = await db.create_planning_session(session)

    # Cache the repo
    repo_cache = get_repo_cache()
    try:
        repo_path = await repo_cache.ensure_fresh(repo_url)
        logger.info(f"Repo cached at {repo_path} for planning session {session.id}")
    except Exception as e:
        logger.error(f"Failed to cache repo {repo_url}: {e}")
        # Update session to cancelled
        await db.update_planning_session(
            session.id,
            status=PlanningSessionStatus.CANCELLED,
            completed_at=datetime.now(timezone.utc),
        )
        raise ValueError(f"Failed to clone repository: {e}") from e

    # Record this repo as recently used
    await db.add_recent_repo(main_thread_id, repo_url)

    # Get initial directory listing for context
    dir_listing = ""
    try:
        import os

        files = []
        for root, dirs, filenames in os.walk(repo_path):
            # Skip hidden directories
            dirs[:] = [d for d in dirs if not d.startswith(".")]
            for f in filenames:
                if not f.startswith("."):
                    rel_path = os.path.relpath(os.path.join(root, f), repo_path)
                    files.append(rel_path)
            # Limit depth to avoid huge listings
            if root.count(os.sep) - str(repo_path).count(os.sep) >= 3:
                dirs.clear()

        if files:
            # Sort and limit to top 50 files
            files.sort()
            dir_listing = "\n".join(files[:50])
            if len(files) > 50:
                dir_listing += f"\n... and {len(files) - 50} more files"
    except Exception as e:
        logger.warning(f"Failed to get directory listing: {e}")

    initial_message = (
        f"Starting planning for: **{task_description}**\n\n"
        f"Repository: {repo_url}\n\n"
    )

    if dir_listing:
        initial_message += f"## Repository Structure\n```\n{dir_listing}\n```\n\n"

    initial_message += (
        "You now have read-only access to explore this codebase using Read, Glob, Grep, and LS tools. "
        "Start by examining relevant files to understand the architecture, then create a plan."
    )

    return session, initial_message


async def run_planning_query(
    session: PlanningSession,
    user_message: str,
) -> str:
    """Run a planning query with codebase access.

    Args:
        session: Active planning session
        user_message: User's message/question

    Returns:
        Claude's response text

    """
    # Get repo path
    repo_cache = get_repo_cache()
    repo_path = repo_cache.get_repo_path(session.repo_url)

    if not repo_path.exists():
        # Re-cache if somehow missing
        repo_path = await repo_cache.ensure_fresh(session.repo_url)

    # Build system prompt
    system_prompt = build_planning_system_prompt(
        session.repo_url, session.task_description
    )

    # Build Claude options with codebase tools
    options = ClaudeAgentOptions(
        model="sonnet",
        permission_mode="plan",  # Read-only filesystem access
        cwd=str(repo_path),
        system_prompt=system_prompt,
        allowed_tools=["Read", "Glob", "Grep", "LS", "WebSearch"],
        # Resume from previous session if exists
        resume=session.claude_session_id,
    )

    logger.info(f"Running planning query for session {session.id} with cwd={repo_path}")

    collected_text: list[str] = []
    new_session_id: str | None = None

    try:
        async for msg in query(prompt=user_message, options=options):
            if isinstance(msg, AssistantMessage):
                for block in msg.content:
                    if isinstance(block, TextBlock):
                        collected_text.append(block.text)
            elif isinstance(msg, ResultMessage):
                if msg.is_error:
                    logger.error(f"Planning query error: {msg.result}")
                    return f"Error during planning: {msg.result}"
                # Extract session ID for resumption
                if hasattr(msg, "session_id"):
                    new_session_id = msg.session_id
            elif isinstance(msg, SystemMessage):
                # Track session ID from init message
                if msg.subtype == "init" and msg.data:
                    new_session_id = msg.data.get("session_id")

        # Update session with Claude session ID for resumption
        if new_session_id and new_session_id != session.claude_session_id:
            await db.update_planning_session(
                session.id, claude_session_id=new_session_id
            )

        return "\n".join(collected_text) if collected_text else "No response generated."

    except Exception as e:
        logger.error(f"Planning query failed: {e}")
        return f"Error during planning: {str(e)}"


async def approve_plan(
    session: PlanningSession,
    plan_text: str,
) -> tuple[WorkerTask, str]:
    """Approve a plan and create GitHub issue + WorkerTask.

    Args:
        session: Active planning session
        plan_text: The approved plan text

    Returns:
        Tuple of (WorkerTask, result_message)

    """
    # Parse owner/repo from URL
    repo_url_clean = session.repo_url.replace("https://github.com/", "").replace(
        ".git", ""
    )
    parts = repo_url_clean.split("/")
    if len(parts) < 2:
        raise ValueError(f"Invalid GitHub URL: {session.repo_url}")
    owner, repo = parts[0], parts[1]

    # Create GitHub issue with the plan
    issue_title = f"[Mainloop] {session.task_description[:80]}"
    issue_body = f"""## Task
{session.task_description}

## Implementation Plan
{plan_text}

---
*Created by [Mainloop](https://github.com/oldsj/mainloop)*
"""

    try:
        issue_result = await create_github_issue(
            owner=owner,
            repo=repo,
            title=issue_title,
            body=issue_body,
            labels=["mainloop-plan"],
        )
        issue_url = issue_result.get("html_url", "")
        issue_number = issue_result.get("number")
        logger.info(f"Created GitHub issue: {issue_url}")
    except Exception as e:
        logger.error(f"Failed to create GitHub issue: {e}")
        raise ValueError(f"Failed to create GitHub issue: {e}") from e

    # Create project if needed
    project = await db.get_or_create_project_from_url(session.user_id, session.repo_url)

    # Extract keywords for task routing
    keywords = extract_keywords(session.task_description)

    # Create WorkerTask with skip_plan=True (already have plan)
    task = WorkerTask(
        main_thread_id=session.main_thread_id,
        user_id=session.user_id,
        task_type="feature",
        description=session.task_description,
        prompt=session.task_description,
        repo_url=session.repo_url,
        project_id=project.id,
        status=TaskStatus.READY_TO_IMPLEMENT,  # Skip planning phase
        conversation_id=session.conversation_id,
        keywords=keywords,
        skip_plan=True,
        plan_text=plan_text,
        issue_url=issue_url,
        issue_number=issue_number,
    )
    task = await db.create_worker_task(task)
    logger.info(f"Created WorkerTask {task.id} from planning session {session.id}")

    # Update planning session
    await db.update_planning_session(
        session.id,
        status=PlanningSessionStatus.APPROVED,
        plan_text=plan_text,
        worker_task_id=task.id,
        completed_at=datetime.now(timezone.utc),
    )

    # Enqueue the worker task workflow
    from mainloop.workflows.worker import worker_task_workflow

    with SetWorkflowID(task.id):
        worker_queue.enqueue(worker_task_workflow, task.id)
    logger.info(f"Enqueued worker workflow for task {task.id}")

    result_message = (
        f"Plan approved and GitHub issue created: {issue_url}\n\n"
        f"Worker task spawned (ID: {task.id[:8]}). "
        f"The agent will start implementing when you trigger it from the Tasks panel."
    )

    return task, result_message


async def cancel_planning(session: PlanningSession) -> str:
    """Cancel an active planning session.

    Args:
        session: Active planning session

    Returns:
        Cancellation message

    """
    await db.update_planning_session(
        session.id,
        status=PlanningSessionStatus.CANCELLED,
        completed_at=datetime.now(timezone.utc),
    )
    logger.info(f"Cancelled planning session {session.id}")

    return "Planning cancelled. No GitHub issue was created."
