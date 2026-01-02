#!/usr/bin/env python3
"""
Job runner for K8s worker Jobs.

This is the entry point when the claude-agent container runs as a K8s Job.
It reads configuration from environment variables, executes the task using
Claude Agent SDK, and POSTs the result back to the backend.

Modes:
  - plan: Clone repo, analyze task, return implementation plan (no GitHub issue - reviewed in inbox)
  - implement: Checkout existing branch, implement code per approved plan, create PR
  - feedback: Address PR comments, push new commits
"""

import asyncio
import os
import sys
import uuid
from datetime import datetime, timezone
from pathlib import Path

import httpx
from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    ResultMessage,
    TextBlock,
    ToolUseBlock,
    query,
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
CLAUDE_PLANS_DIR = Path.home() / ".claude" / "plans"


def find_plan_file() -> Path | None:
    """Find the most recently modified plan file in Claude's plans directory."""
    if not CLAUDE_PLANS_DIR.exists():
        print(f"[job_runner] Plans directory not found: {CLAUDE_PLANS_DIR}")
        return None

    plan_files = list(CLAUDE_PLANS_DIR.glob("*.md"))
    if not plan_files:
        print(f"[job_runner] No plan files found in {CLAUDE_PLANS_DIR}")
        return None

    # Return the most recently modified plan file
    latest = max(plan_files, key=lambda p: p.stat().st_mtime)
    print(f"[job_runner] Found plan file: {latest}")
    return latest


def read_plan_file(path: Path) -> str:
    """Read the content of a plan file."""
    try:
        content = path.read_text()
        return content
    except Exception as e:
        print(f"[job_runner] Error reading plan file {path}: {e}")
        return ""


def pre_clone_repo() -> str | None:
    """Pre-clone the repo before Claude starts.

    Returns the path to the cloned repo, or None if no repo URL.
    Uses shallow clone (depth=1) for plan mode for speed.
    """
    from urllib.parse import urlparse

    import git

    if not REPO_URL:
        return None

    # Parse repo URL to get repo name for target dir
    parsed = urlparse(REPO_URL)
    path_parts = parsed.path.strip("/").split("/")
    if len(path_parts) >= 2:
        repo_name = path_parts[-1].replace(".git", "")
    else:
        repo_name = "repo"

    target_dir = f"{WORKSPACE}/{repo_name}"

    # Build authenticated URL if GH_TOKEN is available
    clone_url = REPO_URL
    gh_token = os.environ.get("GH_TOKEN")
    if gh_token and "github.com" in REPO_URL:
        clone_url = REPO_URL.replace(
            "https://github.com", f"https://x-access-token:{gh_token}@github.com"
        )
        print("[job_runner] Using authenticated clone URL")

    try:
        # Clone - shallow for plan mode, full for others
        if MODE == "plan":
            print("[job_runner] Shallow cloning for plan mode...")
            repo = git.Repo.clone_from(clone_url, target_dir, depth=1)
        else:
            print(f"[job_runner] Full cloning for {MODE} mode...")
            repo = git.Repo.clone_from(clone_url, target_dir)
            # Fetch all refs for branch checkout
            repo.remotes.origin.fetch()

        print(f"[job_runner] Repo cloned to {target_dir}")
        return target_dir

    except git.GitCommandError as e:
        print(f"[job_runner] Clone failed: {e}")
        return None
    except Exception as e:
        print(f"[job_runner] Clone error: {e}")
        return None


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
    """Build prompt for planning phase - explores codebase and outputs a plan."""
    parts = [
        f"Task ID: {TASK_ID[:8]}",
        f"Task: {TASK_PROMPT}",
        "",
    ]

    if REPO_URL:
        parts.append(f"Repository: {REPO_URL}")
        parts.append("")
        parts.extend(
            [
                "The repository is already cloned and you are in its directory.",
                "Explore the codebase to understand the structure and patterns used.",
                "",
                "Create a detailed implementation plan that includes:",
                "- **Summary**: Brief overview of the approach",
                "- **Files to modify**: List each file and describe the changes",
                "- **Files to create**: Any new files needed and their purpose",
                "- **Considerations**: Risks, edge cases, or decisions needing confirmation",
                "",
                "If there are multiple valid approaches, present them as options:",
                "- **Option A**: [approach name] - [brief description]",
                "- **Option B**: [approach name] - [brief description]",
                "And recommend which option you prefer.",
                "",
                "IMPORTANT: Your final output should be ONLY the plan in clean markdown.",
                "The plan will be shown to the user for approval before implementation.",
                "",
            ]
        )
    else:
        parts.extend(
            [
                "Create an implementation plan for this task.",
                "If there are multiple approaches, present them as options.",
                "Output your plan as clean markdown.",
                "",
            ]
        )

    # Add feedback context if this is a plan revision
    if FEEDBACK_CONTEXT:
        parts.extend(
            [
                "Feedback on your previous plan:",
                "---",
                FEEDBACK_CONTEXT,
                "---",
                "",
                "Update your plan to address this feedback.",
            ]
        )

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
        parts.extend(
            [
                f"Repository: {REPO_URL}",
                f"Branch to create: {branch_name}",
            ]
        )
        if ISSUE_NUMBER:
            parts.append(f"Plan issue: #{ISSUE_NUMBER}")
        parts.extend(
            [
                "",
                "Your implementation plan has been approved. Now implement it:",
                "",
                "Instructions:",
                "1. The repository is already cloned and you are in its directory",
                f"2. Create and checkout a new branch: `git checkout -b {branch_name}`",
            ]
        )
        if ISSUE_NUMBER:
            parts.append(f"3. Read your approved plan from issue #{ISSUE_NUMBER}")
        parts.extend(
            [
                "4. Implement the code according to your approved plan",
                "5. Commit your changes with clear commit messages",
                "6. Push the branch",
            ]
        )
        if ISSUE_NUMBER:
            parts.extend(
                [
                    "7. Create a pull request that links to and auto-closes the plan issue:",
                    f"   The PR body MUST start with 'Closes #{ISSUE_NUMBER}' - this auto-closes the issue on merge.",
                    f'   Example: gh pr create --title "..." --body "Closes #{ISSUE_NUMBER} - <summary of changes>"',
                ]
            )
        else:
            parts.extend(
                [
                    '7. Create a pull request: `gh pr create --title "..." --body "..."`',
                ]
            )
        parts.extend(
            [
                "",
                "Follow your plan carefully. If you discover issues during implementation,",
                "add a comment to the PR explaining any deviations from the plan.",
                "",
            ]
        )
    else:
        parts.extend(
            [
                "Instructions:",
                "1. Implement the task as planned",
                "2. Create any necessary files in /workspace",
                "3. Summarize what you accomplished",
                "",
            ]
        )

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
        parts.extend(
            [
                f"Repository: {REPO_URL}",
                f"Branch: {branch_name}",
                "",
            ]
        )

    if FEEDBACK_CONTEXT:
        parts.extend(
            [
                "Feedback to address:",
                "---",
                FEEDBACK_CONTEXT,
                "---",
                "",
            ]
        )

    parts.extend(
        [
            "Instructions:",
            f"1. The repository is already cloned. Checkout the branch: `git checkout {branch_name}`",
            "2. Review the feedback above",
            "3. Make the necessary changes to address the feedback",
            "4. Commit your changes with a clear message referencing the feedback",
            "5. Push the updated branch",
            "",
            "Be thorough in addressing all points raised in the feedback.",
        ]
    )

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
        parts.extend(
            [
                f"Repository: {REPO_URL}",
                f"Branch: {branch_name}",
                "",
            ]
        )

    parts.extend(
        [
            "GitHub Actions checks have FAILED. You must fix them.",
            "",
        ]
    )

    if FEEDBACK_CONTEXT:
        parts.extend(
            [
                "Failed checks and logs:",
                "---",
                FEEDBACK_CONTEXT,
                "---",
                "",
            ]
        )

    parts.extend(
        [
            "Instructions:",
            f"1. The repository is already cloned. Checkout the branch: `git checkout {branch_name}`",
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
        ]
    )

    return "\n".join(parts)


async def execute_task() -> dict:
    """Execute the task using Claude Agent SDK."""
    prompt = build_prompt()
    print(f"[job_runner] Mode: {MODE}")
    print(f"[job_runner] Model: {CLAUDE_MODEL}")
    print(f"[job_runner] Prompt:\n{prompt[:500]}...")

    # Use native plan mode for planning (allows reads, blocks writes)
    # Use bypassPermissions for implement/feedback/fix (needs full access)
    perm_mode = "plan" if MODE == "plan" else "bypassPermissions"
    print(f"[job_runner] Permission mode: {perm_mode}")

    options = ClaudeAgentOptions(
        model=CLAUDE_MODEL,
        permission_mode=perm_mode,
        cwd=WORKSPACE,
    )

    collected_text: list[str] = []
    collected_questions: list[dict] = []  # Questions from AskUserQuestion tool
    plan_content: str | None = None
    session_id: str | None = None
    cost_usd: float | None = None
    should_stop_for_questions = False  # Flag to break cleanly

    async for message in query(prompt=prompt, options=options):
        if isinstance(message, AssistantMessage):
            for block in message.content:
                if isinstance(block, TextBlock):
                    print(f"[claude] {block.text[:200]}...")
                    collected_text.append(block.text)
                elif isinstance(block, ToolUseBlock):
                    # Capture plan from ExitPlanMode tool call
                    if block.name == "ExitPlanMode":
                        # ExitPlanMode doesn't pass content - find and read the plan file
                        plan_file_path = find_plan_file()
                        if plan_file_path:
                            plan_content = read_plan_file(plan_file_path)
                            print(
                                f"[claude] ExitPlanMode - read plan from {plan_file_path} ({len(plan_content)} chars)"
                            )
                        else:
                            print("[claude] ExitPlanMode called but no plan file found")

                    # Capture questions from AskUserQuestion tool call
                    elif block.name == "AskUserQuestion":
                        questions = block.input.get("questions", [])
                        print(
                            f"[claude] AskUserQuestion called with {len(questions)} question(s)"
                        )
                        # Deduplicate by header to avoid repeats from multiple AskUserQuestion calls
                        existing_headers = {q["header"] for q in collected_questions}
                        for q in questions:
                            header = q.get("header", "")
                            if header and header in existing_headers:
                                print(f"[claude] Skipping duplicate question: {header}")
                                continue
                            existing_headers.add(header)
                            collected_questions.append(
                                {
                                    "id": str(uuid.uuid4()),
                                    "header": header,
                                    "question": q.get("question", ""),
                                    "options": [
                                        {
                                            "label": opt.get("label", ""),
                                            "description": opt.get("description"),
                                        }
                                        for opt in q.get("options", [])
                                    ],
                                    "multi_select": q.get("multiSelect", False),
                                }
                            )
                        # In plan mode, flag to stop after this message
                        if MODE == "plan" and collected_questions:
                            print(
                                f"[job_runner] Will stop after this message to get user answers for {len(collected_questions)} question(s)"
                            )
                            should_stop_for_questions = True
        elif isinstance(message, ResultMessage):
            session_id = message.session_id
            cost_usd = message.total_cost_usd
            if message.is_error:
                raise RuntimeError(message.result or "Claude execution failed")

    # If we collected questions in plan mode, return early with questions
    if should_stop_for_questions and collected_questions:
        print(
            f"[job_runner] Returning {len(collected_questions)} question(s) for user answers"
        )
        plan_text = (
            "\n".join(collected_text)
            if collected_text
            else "Plan in progress - answering questions first"
        )
        return {
            "output": plan_text,
            "plan_text": plan_text,
            "questions": collected_questions,
            "suggested_options": [],
            "session_id": session_id,
            "cost_usd": cost_usd,
        }

    # Use plan content if available (from plan mode), otherwise use collected text
    output = plan_content if plan_content else "\n".join(collected_text)

    # Try to extract URLs from output
    pr_url = extract_pr_url(output)
    issue_url = extract_issue_url(output)

    # For plan mode: return plan content and questions for inbox review
    if MODE == "plan" and output:
        # Prefer plan_content from ExitPlanMode (actual plan file), fall back to collected text
        if plan_content:
            plan_text = plan_content
            print(f"[job_runner] Using plan from ExitPlanMode ({len(plan_text)} chars)")
        else:
            # Fall back to last substantial text block
            plan_text = None
            for text in reversed(collected_text):
                if len(text) > 200:  # Substantial text block
                    plan_text = text
                    break
            plan_text = plan_text or output
            print(f"[job_runner] Using collected text as plan ({len(plan_text)} chars)")
        print(f"[job_runner] Collected {len(collected_questions)} question(s)")

        # Extract suggested options from the plan
        suggested_options = extract_plan_options(plan_text)
        print(f"[job_runner] Extracted {len(suggested_options)} options from plan")

        return {
            "output": output,
            "plan_text": plan_text,
            "questions": collected_questions,  # Questions from AskUserQuestion
            "suggested_options": suggested_options,
            "session_id": session_id,
            "cost_usd": cost_usd,
        }

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


def extract_plan_options(plan_text: str) -> list[str]:
    """Extract suggested options from a plan for user selection.

    Looks for patterns like:
    - **Option A**: ...
    - **Option 1**: ...
    - **Approach A**: ...
    - Bullet points starting with "Option"
    """
    import re

    options = ["Approve"]  # Always include approve option

    # Match patterns like "**Option A**:", "**Approach 1**:", "- Option A:"
    option_patterns = [
        r"\*\*Option\s+([A-Za-z0-9]+)\*\*[:\s]+([^\n]+)",  # **Option A**: description
        r"\*\*Approach\s+([A-Za-z0-9]+)\*\*[:\s]+([^\n]+)",  # **Approach 1**: description
        r"[-*]\s*Option\s+([A-Za-z0-9]+)[:\s]+([^\n]+)",  # - Option A: description
        r"[-*]\s*Approach\s+([A-Za-z0-9]+)[:\s]+([^\n]+)",  # - Approach 1: description
    ]

    for pattern in option_patterns:
        matches = re.findall(pattern, plan_text, re.IGNORECASE)
        for match in matches:
            label, desc = match
            # Create a concise option label
            option = f"Use {label.strip()}"
            if option not in options:
                options.append(option)

    # If no options found, add some defaults
    if len(options) == 1:
        options.append("Request changes")

    return options


def create_github_issue_from_plan(plan_content: str) -> str | None:
    """Create a GitHub issue from plan content using githubkit."""
    from githubkit import GitHub

    if not REPO_URL:
        print("[job_runner] No REPO_URL, cannot create issue")
        return None

    gh_token = os.environ.get("GH_TOKEN")
    if not gh_token:
        print("[job_runner] No GH_TOKEN, cannot create issue")
        return None

    # Parse owner/repo from URL
    parts = REPO_URL.rstrip("/").replace(".git", "").split("/")
    owner, repo_name = parts[-2], parts[-1]

    title = (
        f"Plan: {TASK_PROMPT[:80]}" if len(TASK_PROMPT) > 80 else f"Plan: {TASK_PROMPT}"
    )

    body = f"""{plan_content}

---

## Commands
Reply with:
- `/implement` - Approve this plan and start implementation
- `/revise <feedback>` - Request changes to the plan
"""

    try:
        gh = GitHub(gh_token)
        # Create issue
        response = gh.rest.issues.create(
            owner=owner,
            repo=repo_name,
            title=title,
            body=body,
            labels=["mainloop-plan"],
        )
        issue_url = response.parsed_data.html_url
        print(f"[job_runner] Created issue: {issue_url}")
        return issue_url
    except Exception as e:
        print(f"[job_runner] Error creating issue: {e}")
        return None


async def send_result(
    status: str, result: dict | None = None, error: str | None = None
):
    """Send the result back to the backend via HTTP callback."""
    if not CALLBACK_URL:
        print("[job_runner] No callback URL, skipping result POST")
        return

    payload = {
        "task_id": TASK_ID,
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

    # Pre-clone repo if URL provided
    repo_dir = pre_clone_repo()
    if REPO_URL and not repo_dir:
        # Clone was required but failed - abort the job
        error_msg = "Failed to clone repository - check GH_TOKEN and repo URL"
        print(f"[job_runner] ERROR: {error_msg}")
        await send_result(status="failed", error=error_msg)
        sys.exit(1)
    if repo_dir:
        # Change to cloned repo directory so Claude can work on it
        os.chdir(repo_dir)
        print(f"[job_runner] Working in repo: {repo_dir}")

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
