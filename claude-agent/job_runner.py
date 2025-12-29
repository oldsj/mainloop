#!/usr/bin/env python3
"""
Job runner for K8s worker Jobs.

This is the entry point when the claude-agent container runs as a K8s Job.
It reads configuration from environment variables, executes the task using
Claude Agent SDK, and POSTs the result back to the backend.

Modes:
  - initial: Clone repo, implement feature, create PR
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
MODE = os.environ.get("MODE", "initial")
CLAUDE_MODEL = os.environ.get("CLAUDE_MODEL", "sonnet")
REPO_URL = os.environ.get("REPO_URL", "")
PR_NUMBER = os.environ.get("PR_NUMBER", "")
FEEDBACK_CONTEXT = os.environ.get("FEEDBACK_CONTEXT", "")

WORKSPACE = "/workspace"


def build_prompt() -> str:
    """Build the prompt based on mode and context."""
    if MODE == "initial":
        return build_initial_prompt()
    elif MODE == "feedback":
        return build_feedback_prompt()
    else:
        raise ValueError(f"Unknown mode: {MODE}")


def build_initial_prompt() -> str:
    """Build prompt for initial task (create PR)."""
    parts = [
        f"Task ID: {TASK_ID[:8]}",
        f"Task: {TASK_PROMPT}",
        "",
    ]

    if REPO_URL:
        parts.extend([
            f"Repository: {REPO_URL}",
            f"Branch name: mainloop/{TASK_ID[:8]}",
            "",
            "Instructions:",
            "1. Clone the repository",
            "2. Create a new feature branch",
            "3. Implement the task described above",
            "4. Commit your changes with clear commit messages",
            "5. Push the branch and create a pull request",
            "6. Include a clear PR description explaining the changes",
            "",
        ])
    else:
        parts.extend([
            "Instructions:",
            "1. Complete the task described above",
            "2. Create any necessary files in /workspace",
            "3. Summarize what you accomplished",
            "",
        ])

    return "\n".join(parts)


def build_feedback_prompt() -> str:
    """Build prompt for addressing PR feedback."""
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
            f"Branch: mainloop/{TASK_ID[:8]}",
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

    # Try to extract PR URL from output
    pr_url = extract_pr_url(output)

    return {
        "output": output,
        "session_id": session_id,
        "cost_usd": cost_usd,
        "pr_url": pr_url,
    }


def extract_pr_url(output: str) -> str | None:
    """Try to extract a GitHub PR URL from the output."""
    import re

    # Match GitHub PR URLs
    pr_pattern = r"https://github\.com/[^/]+/[^/]+/pull/\d+"
    match = re.search(pr_pattern, output)
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
