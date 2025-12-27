"""Worker task workflow - executes tasks using Claude with durable execution."""

import logging
import subprocess
import tempfile
import shutil
from typing import Any

from dbos import DBOS
from pydantic_ai import Agent
from pydantic_ai.durable_exec.dbos import DBOSAgent

from models import WorkerTask, TaskStatus
from mainloop.db import db
from mainloop.config import settings

logger = logging.getLogger(__name__)

# Topic for receiving human input during task execution
TOPIC_HUMAN_RESPONSE = "human_response"


# Lazy-loaded agent to avoid requiring ANTHROPIC_API_KEY at import time
_code_agent: Agent | None = None
_dbos_code_agent: DBOSAgent | None = None


def get_code_agent() -> DBOSAgent:
    """Get or create the durable code agent."""
    global _code_agent, _dbos_code_agent

    if _dbos_code_agent is None:
        _code_agent = Agent(
            "anthropic:claude-sonnet-4-20250514",
            instructions="""You are a skilled software engineer. You help users by:
1. Understanding their task requirements
2. Making code changes to repositories
3. Creating pull requests with your changes

When working on a task:
- First understand what needs to be done
- Make minimal, focused changes
- Write clear commit messages
- Create descriptive PR titles and descriptions

You have access to the repository via the workspace directory.
Use git and gh CLI for version control and GitHub operations.
""",
            name="code-worker",
        )
        _dbos_code_agent = DBOSAgent(_code_agent)

    return _dbos_code_agent


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
def setup_workspace(repo_url: str | None, base_branch: str) -> str:
    """Set up a temporary workspace for the task."""
    workspace = tempfile.mkdtemp(prefix="worker-")

    if repo_url:
        # Clone the repository using gh CLI
        try:
            subprocess.run(
                ["gh", "repo", "clone", repo_url, workspace, "--", "-b", base_branch],
                check=True,
                capture_output=True,
                text=True,
            )
            logger.info(f"Cloned {repo_url} to {workspace}")
        except subprocess.CalledProcessError as e:
            logger.error(f"Failed to clone repo: {e.stderr}")
            raise RuntimeError(f"Failed to clone repository: {e.stderr}")

    return workspace


@DBOS.step()
def cleanup_workspace(workspace: str) -> None:
    """Clean up temporary workspace."""
    if workspace and workspace.startswith(tempfile.gettempdir()):
        shutil.rmtree(workspace, ignore_errors=True)
        logger.info(f"Cleaned up workspace: {workspace}")


@DBOS.step()
def create_branch(workspace: str, branch_name: str) -> None:
    """Create a new git branch."""
    subprocess.run(
        ["git", "checkout", "-b", branch_name],
        cwd=workspace,
        check=True,
        capture_output=True,
    )
    logger.info(f"Created branch: {branch_name}")


@DBOS.step()
def commit_and_push(workspace: str, message: str, branch_name: str) -> str | None:
    """Commit changes and push to remote. Returns commit SHA."""
    # Check if there are changes
    result = subprocess.run(
        ["git", "status", "--porcelain"],
        cwd=workspace,
        capture_output=True,
        text=True,
    )

    if not result.stdout.strip():
        logger.info("No changes to commit")
        return None

    # Add all changes
    subprocess.run(["git", "add", "-A"], cwd=workspace, check=True)

    # Commit with the provided message
    subprocess.run(
        ["git", "commit", "-m", message],
        cwd=workspace,
        check=True,
        capture_output=True,
    )

    # Get the commit SHA
    result = subprocess.run(
        ["git", "rev-parse", "HEAD"],
        cwd=workspace,
        capture_output=True,
        text=True,
        check=True,
    )
    commit_sha = result.stdout.strip()

    # Push the branch
    subprocess.run(
        ["git", "push", "-u", "origin", branch_name],
        cwd=workspace,
        check=True,
        capture_output=True,
    )

    logger.info(f"Pushed commit {commit_sha} to {branch_name}")
    return commit_sha


@DBOS.step()
def create_pull_request(
    workspace: str,
    title: str,
    body: str,
    base_branch: str,
) -> str:
    """Create a pull request and return its URL."""
    result = subprocess.run(
        [
            "gh", "pr", "create",
            "--title", title,
            "--body", body,
            "--base", base_branch,
        ],
        cwd=workspace,
        capture_output=True,
        text=True,
        check=True,
    )

    pr_url = result.stdout.strip()
    logger.info(f"Created PR: {pr_url}")
    return pr_url


@DBOS.workflow()
async def worker_task_workflow(task_id: str) -> dict[str, Any]:
    """
    Worker workflow that executes a task.

    This workflow:
    1. Loads the task from the database
    2. Sets up a workspace (optionally clones a repo)
    3. Executes the task using Claude via DBOSAgent
    4. Commits changes and creates a PR if applicable
    5. Reports results back to the main thread
    """
    logger.info(f"Starting worker for task: {task_id}")

    # Load the task
    task = await load_worker_task(task_id)
    if not task:
        return {"status": "failed", "error": "Task not found"}

    # Mark as running
    await update_task_running(task_id)

    workspace = None
    try:
        # Set up workspace
        workspace = setup_workspace(task.repo_url, task.base_branch)

        # Create feature branch if repo is involved
        branch_name = None
        if task.repo_url:
            branch_name = task.branch_name or f"mainloop/{task_id[:8]}"
            create_branch(workspace, branch_name)

        # Build the prompt for Claude
        prompt = f"""
Task: {task.description}

{f"Repository: {task.repo_url}" if task.repo_url else ""}
{f"Working in: {workspace}" if workspace else ""}

Please complete this task. If you need to make code changes, use the workspace directory.
When done, summarize what you did.

Additional context:
{task.prompt}
"""

        # Execute the task with Claude using durable execution
        # DBOSAgent automatically wraps this for durability
        agent = get_code_agent()
        result = await agent.run(prompt)

        # Extract the result
        agent_output = result.output

        # If there's a repo, commit and create PR
        pr_url = None
        commit_sha = None

        if task.repo_url and branch_name:
            # Commit any changes
            commit_message = f"feat: {task.description[:50]}"
            commit_sha = commit_and_push(workspace, commit_message, branch_name)

            if commit_sha:
                # Create PR
                pr_body = f"""
## Summary
{agent_output}

## Task
{task.description}

---
*Generated by Mainloop*
"""
                pr_url = create_pull_request(
                    workspace,
                    task.description[:100],
                    pr_body,
                    task.base_branch,
                )

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
                        "pr_url": pr_url,
                        "commit_sha": commit_sha,
                    },
                },
            },
        )

        return {
            "status": "completed",
            "summary": agent_output,
            "pr_url": pr_url,
            "commit_sha": commit_sha,
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

    finally:
        # Clean up workspace
        if workspace:
            cleanup_workspace(workspace)


@DBOS.workflow()
async def interactive_worker_task(task_id: str) -> dict[str, Any]:
    """
    Worker workflow that can ask for human input during execution.

    This workflow uses DBOS.recv() to wait for human responses when needed.
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
    response = DBOS.recv(topic=TOPIC_HUMAN_RESPONSE, timeout_seconds=3600)

    if response is None:
        return {"status": "failed", "error": "Timed out waiting for human input"}

    human_input = response.get("response", "")
    logger.info(f"Received human input: {human_input}")

    # Continue with the task using the human input
    # ... (same as worker_task_workflow but with human_input incorporated)

    return {"status": "completed", "summary": f"Completed with input: {human_input}"}
