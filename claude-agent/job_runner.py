#!/usr/bin/env python3
"""
Job runner for K8s worker Jobs.

This is the entry point when the claude-agent container runs as a K8s Job.
It reads configuration from environment variables, executes the task using
Claude Agent SDK, and POSTs the result back to the backend.

Modes:
  - plan: Clone repo, analyze task, create DRAFT PR with implementation plan (no code)
  - implement: Checkout existing branch, implement code per approved plan, mark PR ready
  - feedback: Address PR comments, push new commits
"""

import asyncio
import json
import os
import sys
from datetime import datetime
from pathlib import Path

import httpx
from claude_agent_sdk import (
    query,
    ClaudeAgentOptions,
    AssistantMessage,
    ResultMessage,
    TextBlock,
)


# Environment variables
TASK_ID = os.environ.get("TASK_ID", "")
TASK_PROMPT = os.environ.get("TASK_PROMPT", "")
CALLBACK_URL = os.environ.get("CALLBACK_URL", "")
MODE = os.environ.get("MODE", "plan")
CLAUDE_MODEL = os.environ.get("CLAUDE_MODEL", "sonnet")
REPO_URL = os.environ.get("REPO_URL", "")
PR_NUMBER = os.environ.get("PR_NUMBER", "")
ISSUE_NUMBER = os.environ.get("ISSUE_NUMBER", "")
BRANCH_NAME = os.environ.get("BRANCH_NAME", "")
FEEDBACK_CONTEXT = os.environ.get("FEEDBACK_CONTEXT", "")

WORKSPACE = "/workspace"


def build_prompt() -> str:
    """Build the prompt based on mode and context."""
    if MODE == "plan":
        return build_plan_prompt()
    elif MODE == "implement":
        return build_implement_prompt()
    elif MODE == "feedback":
        return build_feedback_prompt()
    elif MODE == "fix":
        return build_fix_prompt()
    else:
        raise ValueError(f"Unknown mode: {MODE}")


def build_plan_prompt() -> str:
    """Build prompt for planning phase (create GitHub ISSUE with plan, no code)."""
    parts = [
        f"Task ID: {TASK_ID[:8]}",
        f"Task: {TASK_PROMPT}",
        "",
    ]

    if REPO_URL:
        parts.extend([
            f"Repository: {REPO_URL}",
            "",
            "Instructions:",
            "1. Clone the repository and explore the codebase to understand the structure",
            "2. Analyze what changes are needed to complete this task",
            "3. Create a GitHub ISSUE (not a PR) with your implementation plan",
            "",
            "The issue should have:",
            "- A clear, concise title summarizing the task",
            "- Body with these sections:",
            "",
            "## Approach",
            "Describe your implementation strategy in 2-3 sentences.",
            "",
            "## Files to Modify",
            "List each file you plan to change and briefly describe the changes.",
            "",
            "## Files to Create",
            "List any new files you need to create and their purpose.",
            "",
            "## Considerations",
            "Note any risks, edge cases, or decisions that need confirmation.",
            "",
            "---",
            "",
            "## Commands",
            "Reply with:",
            "- `/implement` - Approve this plan and start implementation",
            "- `/revise <feedback>` - Request changes to the plan",
            "",
            "4. Use `gh issue create --title \"...\" --body \"...\"` to create the issue",
            "5. Add the label `mainloop-plan` to the issue: `gh issue edit <number> --add-label mainloop-plan`",
            "6. Do NOT write any implementation code yet - only the plan",
            "7. Do NOT create a branch or PR yet - that happens after approval",
            "",
        ])
    else:
        parts.extend([
            "Instructions:",
            "1. Analyze the task and create a plan",
            "2. Document what you would do to complete the task",
            "3. Do NOT implement yet - only plan",
            "",
        ])

    # Add feedback context if this is a plan revision
    if FEEDBACK_CONTEXT:
        parts.extend([
            "Feedback on your previous plan:",
            "---",
            FEEDBACK_CONTEXT,
            "---",
            "",
            "Update your plan to address this feedback. Edit the issue body with `gh issue edit <number> --body \"...\"`",
        ])

    return "\n".join(parts)


def build_implement_prompt() -> str:
    """Build prompt for implementation phase (write code, create PR)."""
    # Use provided branch name or fall back to default
    branch_name = BRANCH_NAME or f"mainloop/{TASK_ID[:8]}"

    parts = [
        f"Task ID: {TASK_ID[:8]}",
        f"Original Task: {TASK_PROMPT}",
        "",
    ]

    if REPO_URL:
        parts.extend([
            f"Repository: {REPO_URL}",
            f"Branch to create: {branch_name}",
        ])
        if ISSUE_NUMBER:
            parts.append(f"Plan issue: #{ISSUE_NUMBER}")
        parts.extend([
            "",
            "Your implementation plan has been approved. Now implement it:",
            "",
            "Instructions:",
            "1. Clone the repository",
            f"2. Create and checkout a new branch: `git checkout -b {branch_name}`",
        ])
        if ISSUE_NUMBER:
            parts.append(f"3. Read your approved plan from issue #{ISSUE_NUMBER}")
        parts.extend([
            "4. Implement the code according to your approved plan",
            "5. Commit your changes with clear commit messages",
            "6. Push the branch",
        ])
        if ISSUE_NUMBER:
            parts.extend([
                f"7. Create a pull request that links to and auto-closes the plan issue:",
                f"   The PR body MUST start with 'Closes #{ISSUE_NUMBER}' - this auto-closes the issue on merge.",
                "",
                "   Example:",
                f'   `gh pr create --title "Add feature X" --body "Closes #{ISSUE_NUMBER}',
                "",
                "   ## Summary",
                "   Brief description of what was implemented.",
                "",
                "   ## Changes",
                '   - Change 1',
                '   - Change 2"`,
            ])
        else:
            parts.extend([
                "7. Create a pull request: `gh pr create --title \"...\" --body \"...\"`",
            ])
        parts.extend([
            "",
            "Follow your plan carefully. If you discover issues during implementation,",
            "add a comment to the PR explaining any deviations from the plan.",
            "",
        ])
    else:
        parts.extend([
            "Instructions:",
            "1. Implement the task as planned",
            "2. Create any necessary files in /workspace",
            "3. Summarize what you accomplished",
            "",
        ])

    return "\n".join(parts)


def build_feedback_prompt() -> str:
    """Build prompt for addressing PR feedback."""
    branch_name = BRANCH_NAME or f"mainloop/{TASK_ID[:8]}"

    parts = [
        f"Task ID: {TASK_ID[:8]}",
        f"Original Task: {TASK_PROMPT}",
        "",
        f"You are addressing feedback on PR #{PR_NUMBER}",
        "",
    ]

    if REPO_URL:
        parts.extend([
            f"Repository: {REPO_URL}",
            f"Branch: {branch_name}",
            "",
        ])

    if FEEDBACK_CONTEXT:
        parts.extend([
            "Feedback to address:",
            "---",
            FEEDBACK_CONTEXT,
            "---",
            "",
        ])

    parts.extend([
        "Instructions:",
        "1. Clone the repository and checkout the existing branch",
        "2. Review the feedback above",
        "3. Make the necessary changes to address the feedback",
        "4. Commit your changes with a clear message referencing the feedback",
        "5. Push the updated branch",
        "",
        "Be thorough in addressing all points raised in the feedback.",
    ])

    return "\n".join(parts)


def build_fix_prompt() -> str:
    """Build prompt for fixing CI failures."""
    branch_name = BRANCH_NAME or f"mainloop/{TASK_ID[:8]}"

    parts = [
        f"Task ID: {TASK_ID[:8]}",
        f"Original Task: {TASK_PROMPT}",
        "",
        f"You are fixing CI failures on PR #{PR_NUMBER}",
        "",
    ]

    if REPO_URL:
        parts.extend([
            f"Repository: {REPO_URL}",
            f"Branch: {branch_name}",
            "",
        ])

    parts.extend([
        "GitHub Actions checks have FAILED. You must fix them.",
        "",
    ])

    if FEEDBACK_CONTEXT:
        parts.extend([
            "Failed checks and logs:",
            "---",
            FEEDBACK_CONTEXT,
            "---",
            "",
        ])

    parts.extend([
        "Instructions:",
        "1. Clone the repository and checkout the existing branch",
        "2. Analyze the failure logs above carefully",
        "3. Identify the root cause of each failure",
        "4. Fix the issues (lint errors, test failures, type errors, build errors)",
        "5. Run `trunk check` locally if available to verify before pushing",
        "6. Commit and push your fix with a clear message",
        "",
        "IMPORTANT:",
        "- Focus ONLY on fixing the specific failures shown above",
        "- Do NOT refactor or change unrelated code",
        "- Do NOT mark the PR ready - the workflow will re-check Actions after you push",
    ])

    return "\n".join(parts)


async def execute_task() -> dict:
    """Execute the task using Claude Agent SDK."""
    prompt = build_prompt()
    print(f"[job_runner] Mode: {MODE}")
    print(f"[job_runner] Model: {CLAUDE_MODEL}")
    print(f"[job_runner] Prompt:\n{prompt[:500]}...")

    options = ClaudeAgentOptions(
        model=CLAUDE_MODEL,
        permission_mode="bypassPermissions",
        cwd=WORKSPACE,
    )

    collected_text: list[str] = []
    session_id: str | None = None
    cost_usd: float | None = None

    async for message in query(prompt=prompt, options=options):
        if isinstance(message, AssistantMessage):
            for block in message.content:
                if isinstance(block, TextBlock):
                    print(f"[claude] {block.text[:200]}...")
                    collected_text.append(block.text)
        elif isinstance(message, ResultMessage):
            session_id = message.session_id
            cost_usd = message.total_cost_usd
            if message.is_error:
                raise RuntimeError(message.result or "Claude execution failed")

    output = "\n".join(collected_text)

    # Try to extract URLs from output
    pr_url = extract_pr_url(output)
    issue_url = extract_issue_url(output)

    return {
        "output": output,
        "session_id": session_id,
        "cost_usd": cost_usd,
        "pr_url": pr_url,
        "issue_url": issue_url,
    }


def extract_pr_url(output: str) -> str | None:
    """Try to extract a GitHub PR URL from the output."""
    import re

    # Match GitHub PR URLs
    pr_pattern = r"https://github\.com/[^/]+/[^/]+/pull/\d+"
    match = re.search(pr_pattern, output)
    return match.group(0) if match else None


def extract_issue_url(output: str) -> str | None:
    """Try to extract a GitHub issue URL from the output."""
    import re

    # Match GitHub issue URLs
    issue_pattern = r"https://github\.com/[^/]+/[^/]+/issues/\d+"
    match = re.search(issue_pattern, output)
    return match.group(0) if match else None


async def send_result(status: str, result: dict | None = None, error: str | None = None):
    """Send the result back to the backend via HTTP callback."""
    if not CALLBACK_URL:
        print("[job_runner] No callback URL, skipping result POST")
        return

    payload = {
        "task_id": TASK_ID,
        "status": status,
        "result": result,
        "error": error,
        "completed_at": datetime.utcnow().isoformat(),
    }

    print(f"[job_runner] Sending result to {CALLBACK_URL}")
    print(f"[job_runner] Status: {status}")

    async with httpx.AsyncClient(timeout=30.0) as client:
        for attempt in range(3):
            try:
                response = await client.post(
                    CALLBACK_URL,
                    json=payload,
                    headers={"Content-Type": "application/json"},
                )
                response.raise_for_status()
                print(f"[job_runner] Result sent successfully")
                return
            except httpx.RequestError as e:
                print(f"[job_runner] Attempt {attempt + 1} failed: {e}")
                if attempt < 2:
                    await asyncio.sleep(2 ** attempt)
                else:
                    print(f"[job_runner] Failed to send result after 3 attempts")
                    raise


async def main():
    """Main entry point."""
    print(f"[job_runner] Starting job for task {TASK_ID}")
    print(f"[job_runner] Working directory: {WORKSPACE}")

    # Validate required env vars
    if not TASK_ID:
        print("[job_runner] ERROR: TASK_ID not set")
        sys.exit(1)
    if not TASK_PROMPT:
        print("[job_runner] ERROR: TASK_PROMPT not set")
        sys.exit(1)

    # Ensure workspace exists
    Path(WORKSPACE).mkdir(parents=True, exist_ok=True)
    os.chdir(WORKSPACE)

    try:
        result = await execute_task()
        await send_result(
            status="completed",
            result=result,
        )
        print("[job_runner] Job completed successfully")

    except Exception as e:
        print(f"[job_runner] ERROR: {e}")
        await send_result(
            status="failed",
            error=str(e),
        )
        sys.exit(1)


if __name__ == "__main__":
    asyncio.run(main())
