#!/usr/bin/env python3
"""
Job runner for K8s session Jobs.

This is the entry point when the claude-agent container runs as a K8s Job.
It reads configuration from environment variables, executes the task using
Claude Agent SDK, and POSTs the result back to the backend.

A session IS a job - no modes, just run Claude with a prompt and return the result.
"""

import asyncio
import os
import sys
from datetime import datetime, timezone
from pathlib import Path

import httpx
from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    ResultMessage,
    TextBlock,
    query,
)

# Environment variables
SESSION_ID = os.environ.get("SESSION_ID", "")
TASK_PROMPT = os.environ.get("TASK_PROMPT", "")
CALLBACK_URL = os.environ.get("CALLBACK_URL", "")
CLAUDE_MODEL = os.environ.get("CLAUDE_MODEL", "sonnet")

WORKSPACE = "/workspace"


async def execute_task() -> dict:
    """Execute the task using Claude Agent SDK."""
    print(f"[job_runner] Model: {CLAUDE_MODEL}")
    print(f"[job_runner] Prompt:\n{TASK_PROMPT[:500]}...")

    options = ClaudeAgentOptions(
        model=CLAUDE_MODEL,
        permission_mode="bypassPermissions",
        cwd=WORKSPACE,
    )

    collected_text: list[str] = []
    session_id: str | None = None
    cost_usd: float | None = None

    async for message in query(prompt=TASK_PROMPT, options=options):
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

    output = "\n".join(collected_text) if collected_text else "No response generated."

    return {
        "output": output,
        "session_id": session_id,
        "cost_usd": cost_usd,
    }


async def send_result(
    status: str, result: dict | None = None, error: str | None = None
):
    """Send the result back to the backend via HTTP callback."""
    if not CALLBACK_URL:
        print("[job_runner] No callback URL, skipping result POST")
        return

    payload = {
        "session_id": SESSION_ID,
        "status": status,
        "result": result,
        "error": error,
        "completed_at": datetime.now(timezone.utc).isoformat(),
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
                print("[job_runner] Result sent successfully")
                return
            except httpx.RequestError as e:
                print(f"[job_runner] Attempt {attempt + 1} failed: {e}")
                if attempt < 2:
                    await asyncio.sleep(2**attempt)
                else:
                    print("[job_runner] Failed to send result after 3 attempts")
                    raise


async def main():
    """Execute the job runner workflow."""
    print(f"[job_runner] Starting job for session {SESSION_ID}")
    print(f"[job_runner] Working directory: {WORKSPACE}")

    # Validate required env vars
    if not SESSION_ID:
        print("[job_runner] ERROR: SESSION_ID not set")
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
