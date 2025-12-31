"""GitHub PR monitoring service."""

import logging
from datetime import datetime
from typing import Literal

import httpx
from pydantic import BaseModel

from mainloop.config import settings

logger = logging.getLogger(__name__)

GITHUB_API_BASE = "https://api.github.com"


class PRComment(BaseModel):
    """A comment on a PR."""

    id: int
    body: str
    user: str
    created_at: datetime
    updated_at: datetime
    url: str
    is_review_comment: bool = False  # True if it's a review comment on code


class PRReview(BaseModel):
    """A review on a PR."""

    id: int
    user: str
    state: Literal["APPROVED", "CHANGES_REQUESTED", "COMMENTED", "PENDING", "DISMISSED"]
    body: str | None
    submitted_at: datetime


class PRStatus(BaseModel):
    """Status of a pull request."""

    number: int
    state: Literal["open", "closed"]
    merged: bool
    title: str
    body: str | None
    head_branch: str
    head_sha: str
    base_branch: str
    url: str
    created_at: datetime
    updated_at: datetime
    mergeable: bool | None
    review_decision: str | None  # APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED, etc.


def _get_headers() -> dict:
    """Get headers for GitHub API requests."""
    headers = {
        "Accept": "application/vnd.github.v3+json",
        "X-GitHub-Api-Version": "2022-11-28",
    }
    if settings.github_token:
        headers["Authorization"] = f"Bearer {settings.github_token}"
    return headers


def _parse_repo(repo_url: str) -> tuple[str, str]:
    """Parse owner and repo from a GitHub URL.

    Args:
        repo_url: URL like https://github.com/owner/repo

    Returns:
        Tuple of (owner, repo)
    """
    # Handle various GitHub URL formats
    url = repo_url.rstrip("/")
    if url.endswith(".git"):
        url = url[:-4]

    parts = url.split("/")
    return parts[-2], parts[-1]


async def get_pr_status(repo_url: str, pr_number: int) -> PRStatus | None:
    """Get the current status of a PR.

    Args:
        repo_url: GitHub repository URL
        pr_number: PR number

    Returns:
        PRStatus or None if not found
    """
    owner, repo = _parse_repo(repo_url)

    async with httpx.AsyncClient() as client:
        response = await client.get(
            f"{GITHUB_API_BASE}/repos/{owner}/{repo}/pulls/{pr_number}",
            headers=_get_headers(),
        )

        if response.status_code == 404:
            return None

        response.raise_for_status()
        data = response.json()

        return PRStatus(
            number=data["number"],
            state=data["state"],
            merged=data.get("merged", False),
            title=data["title"],
            body=data.get("body"),
            head_branch=data["head"]["ref"],
            head_sha=data["head"]["sha"],
            base_branch=data["base"]["ref"],
            url=data["html_url"],
            created_at=datetime.fromisoformat(data["created_at"].replace("Z", "+00:00")),
            updated_at=datetime.fromisoformat(data["updated_at"].replace("Z", "+00:00")),
            mergeable=data.get("mergeable"),
            review_decision=data.get("review_decision"),
        )


async def get_pr_comments(
    repo_url: str,
    pr_number: int,
    since: datetime | None = None,
) -> list[PRComment]:
    """Get comments on a PR.

    Args:
        repo_url: GitHub repository URL
        pr_number: PR number
        since: Only return comments after this timestamp

    Returns:
        List of comments
    """
    owner, repo = _parse_repo(repo_url)
    comments = []

    async with httpx.AsyncClient() as client:
        # Get issue comments (general PR comments)
        params = {}
        if since:
            params["since"] = since.isoformat()

        response = await client.get(
            f"{GITHUB_API_BASE}/repos/{owner}/{repo}/issues/{pr_number}/comments",
            headers=_get_headers(),
            params=params,
        )
        response.raise_for_status()

        for c in response.json():
            comments.append(
                PRComment(
                    id=c["id"],
                    body=c["body"],
                    user=c["user"]["login"],
                    created_at=datetime.fromisoformat(c["created_at"].replace("Z", "+00:00")),
                    updated_at=datetime.fromisoformat(c["updated_at"].replace("Z", "+00:00")),
                    url=c["html_url"],
                    is_review_comment=False,
                )
            )

        # Get review comments (inline code comments)
        response = await client.get(
            f"{GITHUB_API_BASE}/repos/{owner}/{repo}/pulls/{pr_number}/comments",
            headers=_get_headers(),
            params=params,
        )
        response.raise_for_status()

        for c in response.json():
            comments.append(
                PRComment(
                    id=c["id"],
                    body=c["body"],
                    user=c["user"]["login"],
                    created_at=datetime.fromisoformat(c["created_at"].replace("Z", "+00:00")),
                    updated_at=datetime.fromisoformat(c["updated_at"].replace("Z", "+00:00")),
                    url=c["html_url"],
                    is_review_comment=True,
                )
            )

    # Sort by created_at
    comments.sort(key=lambda c: c.created_at)

    # Filter by since if provided
    if since:
        comments = [c for c in comments if c.created_at > since]

    return comments


async def get_pr_reviews(
    repo_url: str,
    pr_number: int,
) -> list[PRReview]:
    """Get reviews on a PR.

    Args:
        repo_url: GitHub repository URL
        pr_number: PR number

    Returns:
        List of reviews
    """
    owner, repo = _parse_repo(repo_url)

    async with httpx.AsyncClient() as client:
        response = await client.get(
            f"{GITHUB_API_BASE}/repos/{owner}/{repo}/pulls/{pr_number}/reviews",
            headers=_get_headers(),
        )
        response.raise_for_status()

        reviews = []
        for r in response.json():
            reviews.append(
                PRReview(
                    id=r["id"],
                    user=r["user"]["login"],
                    state=r["state"],
                    body=r.get("body"),
                    submitted_at=datetime.fromisoformat(r["submitted_at"].replace("Z", "+00:00")),
                )
            )

        return reviews


async def is_pr_merged(repo_url: str, pr_number: int) -> bool:
    """Check if a PR has been merged.

    Args:
        repo_url: GitHub repository URL
        pr_number: PR number

    Returns:
        True if merged, False otherwise
    """
    status = await get_pr_status(repo_url, pr_number)
    return status.merged if status else False


async def is_pr_approved(repo_url: str, pr_number: int) -> bool:
    """Check if a PR has been approved.

    Args:
        repo_url: GitHub repository URL
        pr_number: PR number

    Returns:
        True if approved, False otherwise
    """
    reviews = await get_pr_reviews(repo_url, pr_number)

    # Find the most recent review from each user
    user_reviews: dict[str, PRReview] = {}
    for review in reviews:
        existing = user_reviews.get(review.user)
        if not existing or review.submitted_at > existing.submitted_at:
            user_reviews[review.user] = review

    # Check if any user has approved
    return any(r.state == "APPROVED" for r in user_reviews.values())


class CheckRunStatus(BaseModel):
    """Status of a GitHub Actions check run."""

    name: str
    status: Literal["queued", "in_progress", "completed"]
    conclusion: Literal[
        "success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required"
    ] | None
    details_url: str | None
    output_title: str | None = None
    output_summary: str | None = None


class CombinedCheckStatus(BaseModel):
    """Combined status of all check runs for a commit."""

    status: Literal["pending", "success", "failure"]
    total_count: int
    check_runs: list[CheckRunStatus]
    failed_runs: list[CheckRunStatus]


async def get_check_status(repo_url: str, pr_number: int) -> CombinedCheckStatus:
    """Get combined status of GitHub Actions checks for a PR's head commit.

    Args:
        repo_url: GitHub repository URL
        pr_number: PR number

    Returns:
        CombinedCheckStatus with overall status and individual check runs
    """
    owner, repo = _parse_repo(repo_url)

    # First get the PR to find the head SHA
    pr_status = await get_pr_status(repo_url, pr_number)
    if not pr_status:
        return CombinedCheckStatus(
            status="pending",
            total_count=0,
            check_runs=[],
            failed_runs=[],
        )

    head_sha = pr_status.head_sha

    async with httpx.AsyncClient() as client:
        response = await client.get(
            f"{GITHUB_API_BASE}/repos/{owner}/{repo}/commits/{head_sha}/check-runs",
            headers=_get_headers(),
        )
        response.raise_for_status()
        data = response.json()

    check_runs = []
    for run in data.get("check_runs", []):
        check_runs.append(
            CheckRunStatus(
                name=run["name"],
                status=run["status"],
                conclusion=run.get("conclusion"),
                details_url=run.get("details_url"),
                output_title=run.get("output", {}).get("title"),
                output_summary=run.get("output", {}).get("summary"),
            )
        )

    failed = [r for r in check_runs if r.conclusion == "failure"]
    pending = [r for r in check_runs if r.status != "completed"]

    if pending:
        status: Literal["pending", "success", "failure"] = "pending"
    elif failed:
        status = "failure"
    else:
        status = "success"

    return CombinedCheckStatus(
        status=status,
        total_count=len(check_runs),
        check_runs=check_runs,
        failed_runs=failed,
    )


async def get_check_failure_logs(repo_url: str, pr_number: int) -> str:
    """Get formatted failure context from failed check runs.

    This retrieves the output summary from each failed check run,
    which typically contains the error details.

    Args:
        repo_url: GitHub repository URL
        pr_number: PR number

    Returns:
        Formatted string of failure context for the agent
    """
    check_status = await get_check_status(repo_url, pr_number)

    if not check_status.failed_runs:
        return ""

    parts = []
    for run in check_status.failed_runs:
        lines = [f"## Failed: {run.name}"]
        if run.output_title:
            lines.append(f"**{run.output_title}**")
        if run.output_summary:
            # Truncate long summaries
            summary = run.output_summary[:2000]
            if len(run.output_summary) > 2000:
                summary += "\n... (truncated)"
            lines.append(summary)
        if run.details_url:
            lines.append(f"Details: {run.details_url}")
        parts.append("\n".join(lines))

    return "\n\n---\n\n".join(parts)


# Tag that triggers the agent to act on a comment
AGENT_MENTION_TAG = "@mainloop"


def _should_agent_act_on_comment(comment: PRComment) -> bool:
    """Check if the agent should act on a comment.

    Returns True if:
    - Comment mentions @mainloop
    - Comment is a code review comment (inline on code)
    - Comment starts with /revise command
    """
    if AGENT_MENTION_TAG.lower() in comment.body.lower():
        return True
    if comment.is_review_comment:
        return True
    # Check for /revise command
    command, _ = parse_issue_command(comment.body)
    if command == "revise":
        return True
    return False


def _should_agent_act_on_review(review: PRReview) -> bool:
    """Check if the agent should act on a review.

    Returns True if:
    - Review requests changes (explicit feedback)
    - Review body mentions @mainloop
    """
    if review.state == "CHANGES_REQUESTED":
        return True
    if review.body and AGENT_MENTION_TAG.lower() in review.body.lower():
        return True
    return False


async def format_feedback_for_agent(
    repo_url: str,
    pr_number: int,
    since: datetime | None = None,
) -> str:
    """Format PR feedback as context for the agent.

    Only includes feedback that the agent should act on:
    - Comments mentioning @mainloop
    - Code review comments (inline on specific lines)
    - Reviews requesting changes

    General discussion comments without @mainloop are ignored.

    Args:
        repo_url: GitHub repository URL
        pr_number: PR number
        since: Only include comments after this timestamp

    Returns:
        Formatted string of feedback
    """
    comments = await get_pr_comments(repo_url, pr_number, since=since)
    reviews = await get_pr_reviews(repo_url, pr_number)

    # Filter reviews to only recent ones if since is provided
    if since:
        reviews = [r for r in reviews if r.submitted_at > since]

    parts = []

    # Add review feedback (only if agent should act on it)
    for review in reviews:
        if _should_agent_act_on_review(review) and review.body:
            parts.append(f"## Review from @{review.user} ({review.state})\n{review.body}")

    # Add comments (only if agent should act on them)
    for comment in comments:
        if _should_agent_act_on_comment(comment):
            comment_type = "Code comment" if comment.is_review_comment else "Comment"
            body = comment.body

            # Extract feedback from /revise command if present
            command, feedback = parse_issue_command(body)
            if command == "revise" and feedback:
                body = feedback  # Use just the feedback text, not the /revise prefix

            parts.append(f"## {comment_type} from @{comment.user}\n{body}")

    return "\n\n---\n\n".join(parts) if parts else ""


async def add_reaction_to_comment(
    repo_url: str,
    comment_id: int,
    is_review_comment: bool = False,
    reaction: str = "eyes",  # 👀 - indicates "looking at this"
) -> bool:
    """Add a reaction to acknowledge a PR comment.

    Args:
        repo_url: GitHub repository URL
        comment_id: The comment ID
        is_review_comment: True if it's a code review comment
        reaction: Reaction type (eyes, +1, heart, rocket, etc.)

    Returns:
        True if successful, False otherwise
    """
    owner, repo = _parse_repo(repo_url)

    # Different endpoints for issue comments vs review comments
    if is_review_comment:
        url = f"{GITHUB_API_BASE}/repos/{owner}/{repo}/pulls/comments/{comment_id}/reactions"
    else:
        url = f"{GITHUB_API_BASE}/repos/{owner}/{repo}/issues/comments/{comment_id}/reactions"

    async with httpx.AsyncClient() as client:
        try:
            response = await client.post(
                url,
                headers=_get_headers(),
                json={"content": reaction},
            )
            # 201 = created, 200 = already exists
            return response.status_code in (200, 201)
        except Exception as e:
            logger.warning(f"Failed to add reaction to comment {comment_id}: {e}")
            return False


async def acknowledge_comments(
    repo_url: str,
    comments: list[PRComment],
) -> None:
    """Add 👀 reaction to comments to indicate they've been seen.

    Args:
        repo_url: GitHub repository URL
        comments: List of comments to acknowledge
    """
    for comment in comments:
        await add_reaction_to_comment(
            repo_url,
            comment.id,
            is_review_comment=comment.is_review_comment,
            reaction="eyes",
        )


# ============= GitHub Issue Support =============

class IssueStatus(BaseModel):
    """Status of a GitHub issue."""

    number: int
    state: Literal["open", "closed"]
    title: str
    body: str | None
    url: str
    created_at: datetime
    updated_at: datetime
    labels: list[str]


class IssueComment(BaseModel):
    """A comment on a GitHub issue."""

    id: int
    body: str
    user: str
    created_at: datetime
    updated_at: datetime
    url: str


class ConditionalResponse(BaseModel):
    """Response with conditional request metadata for rate limiting."""

    data: dict | list | None = None
    etag: str | None = None
    last_modified: datetime | None = None
    not_modified: bool = False  # True if 304 response (content unchanged)


class IssueCommand(BaseModel):
    """A parsed slash command from an issue comment."""

    command: Literal["implement", "revise", "none"]
    feedback: str | None = None  # Feedback text for /revise command
    comment_id: int | None = None  # ID of the comment containing the command
    user: str | None = None  # User who issued the command


def parse_issue_command(comment_body: str) -> tuple[Literal["implement", "revise", "none"], str | None]:
    """Parse a slash command from an issue comment.

    Supported commands:
        /implement - Approve the plan and start implementation
        /revise <feedback> - Request changes to the plan

    Args:
        comment_body: The comment text to parse

    Returns:
        Tuple of (command_type, feedback_text)
        - ("implement", None) for /implement
        - ("revise", "feedback text") for /revise
        - ("none", None) for no recognized command
    """
    import re

    # Normalize whitespace
    body = comment_body.strip()

    # Check for /implement command (case insensitive)
    if re.match(r"^/implement\s*$", body, re.IGNORECASE):
        return ("implement", None)

    # Also accept /lgtm as alias for implement
    if re.match(r"^/lgtm\s*$", body, re.IGNORECASE):
        return ("implement", None)

    # Check for /revise command with feedback
    revise_match = re.match(r"^/revise\s+(.+)$", body, re.IGNORECASE | re.DOTALL)
    if revise_match:
        feedback = revise_match.group(1).strip()
        return ("revise", feedback)

    return ("none", None)


def parse_comments_for_command(comments: list[dict]) -> IssueCommand:
    """Parse a list of issue comments to find the most recent command.

    Scans comments in reverse chronological order to find the latest command.

    Args:
        comments: List of comment dicts (from get_issue_comments response.data)

    Returns:
        IssueCommand with the most recent command found, or command="none" if no command
    """
    # Sort by created_at descending (most recent first)
    sorted_comments = sorted(
        comments,
        key=lambda c: c.get("created_at", ""),
        reverse=True,
    )

    for comment in sorted_comments:
        command, feedback = parse_issue_command(comment.get("body", ""))
        if command != "none":
            return IssueCommand(
                command=command,
                feedback=feedback,
                comment_id=comment.get("id"),
                user=comment.get("user"),
            )

    return IssueCommand(command="none")


def _parse_last_modified(header: str | None) -> datetime | None:
    """Parse Last-Modified header to datetime."""
    if not header:
        return None
    try:
        from email.utils import parsedate_to_datetime
        return parsedate_to_datetime(header)
    except (ValueError, TypeError):
        return None


async def get_issue_status(
    repo_url: str,
    issue_number: int,
    etag: str | None = None,
    if_modified_since: datetime | None = None,
) -> ConditionalResponse:
    """Get the current status of an issue with conditional request support.

    Uses ETags/If-Modified-Since for rate-limit friendly polling.
    304 Not Modified responses don't count against GitHub's rate limit.

    Args:
        repo_url: GitHub repository URL
        issue_number: Issue number
        etag: ETag from previous request (If-None-Match header)
        if_modified_since: Datetime from previous request (If-Modified-Since header)

    Returns:
        ConditionalResponse with issue data or not_modified=True
    """
    owner, repo = _parse_repo(repo_url)
    headers = _get_headers()

    if etag:
        headers["If-None-Match"] = etag
    if if_modified_since:
        headers["If-Modified-Since"] = if_modified_since.strftime(
            "%a, %d %b %Y %H:%M:%S GMT"
        )

    async with httpx.AsyncClient() as client:
        response = await client.get(
            f"{GITHUB_API_BASE}/repos/{owner}/{repo}/issues/{issue_number}",
            headers=headers,
        )

        # 304 = Not Modified (content unchanged, doesn't count against rate limit)
        if response.status_code == 304:
            return ConditionalResponse(
                not_modified=True,
                etag=response.headers.get("ETag"),
                last_modified=_parse_last_modified(response.headers.get("Last-Modified")),
            )

        if response.status_code == 404:
            return ConditionalResponse(data={"state": "not_found"})

        response.raise_for_status()
        data = response.json()

        issue = IssueStatus(
            number=data["number"],
            state=data["state"],
            title=data["title"],
            body=data.get("body"),
            url=data["html_url"],
            created_at=datetime.fromisoformat(data["created_at"].replace("Z", "+00:00")),
            updated_at=datetime.fromisoformat(data["updated_at"].replace("Z", "+00:00")),
            labels=[label["name"] for label in data.get("labels", [])],
        )

        return ConditionalResponse(
            data=issue.model_dump(),
            etag=response.headers.get("ETag"),
            last_modified=_parse_last_modified(response.headers.get("Last-Modified")),
        )


async def get_issue_comments(
    repo_url: str,
    issue_number: int,
    since: datetime | None = None,
    etag: str | None = None,
) -> ConditionalResponse:
    """Get comments on an issue with conditional request support.

    Args:
        repo_url: GitHub repository URL
        issue_number: Issue number
        since: Only return comments after this timestamp
        etag: ETag from previous request

    Returns:
        ConditionalResponse with list of IssueComment data or not_modified=True
    """
    owner, repo = _parse_repo(repo_url)
    headers = _get_headers()

    if etag:
        headers["If-None-Match"] = etag

    params = {}
    if since:
        params["since"] = since.isoformat()

    async with httpx.AsyncClient() as client:
        response = await client.get(
            f"{GITHUB_API_BASE}/repos/{owner}/{repo}/issues/{issue_number}/comments",
            headers=headers,
            params=params,
        )

        if response.status_code == 304:
            return ConditionalResponse(
                not_modified=True,
                etag=response.headers.get("ETag"),
            )

        response.raise_for_status()
        data = response.json()

        comments = []
        for c in data:
            comments.append(
                IssueComment(
                    id=c["id"],
                    body=c["body"],
                    user=c["user"]["login"],
                    created_at=datetime.fromisoformat(c["created_at"].replace("Z", "+00:00")),
                    updated_at=datetime.fromisoformat(c["updated_at"].replace("Z", "+00:00")),
                    url=c["html_url"],
                ).model_dump()
            )

        return ConditionalResponse(
            data=comments,
            etag=response.headers.get("ETag"),
        )


def generate_branch_name(
    issue_number: int,
    title: str,
    task_type: str = "feature",
) -> str:
    """Generate an intelligent branch name from issue metadata.

    Examples:
        - issue_number=42, title="Add dark mode toggle", type="feature"
          -> "feature/42-add-dark-mode-toggle"
        - issue_number=123, title="Fix login crash on iOS", type="bugfix"
          -> "fix/123-login-crash-ios"

    Args:
        issue_number: GitHub issue number
        title: Issue title
        task_type: Type of task (feature, bugfix, bug, fix, refactor, docs, chore, test)

    Returns:
        Branch name like "feature/42-add-dark-mode"
    """
    import re

    # Map task types to branch prefixes
    prefix_map = {
        "feature": "feature",
        "bugfix": "fix",
        "bug": "fix",
        "fix": "fix",
        "refactor": "refactor",
        "docs": "docs",
        "test": "test",
        "chore": "chore",
    }
    prefix = prefix_map.get(task_type.lower(), "feature")

    # Slugify title
    slug = title.lower()
    slug = re.sub(r"[^\w\s-]", "", slug)  # Remove special chars
    slug = re.sub(r"[\s_]+", "-", slug)   # Spaces/underscores to hyphens
    slug = re.sub(r"-+", "-", slug)        # Collapse multiple hyphens
    slug = slug.strip("-")

    # Remove stop words
    stop_words = {"the", "a", "an", "and", "or", "but", "in", "on", "at", "to", "for", "of", "with", "by", "is", "are", "was", "were"}
    words = [w for w in slug.split("-") if w and w not in stop_words]
    slug = "-".join(words[:8])  # Max 8 words

    # Trim to reasonable length
    if len(slug) > 50:
        slug = slug[:50].rsplit("-", 1)[0]

    return f"{prefix}/{issue_number}-{slug}"


async def format_issue_feedback_for_agent(
    repo_url: str,
    issue_number: int,
    since: datetime | None = None,
) -> str:
    """Format issue comments as feedback context for the agent.

    Args:
        repo_url: GitHub repository URL
        issue_number: Issue number
        since: Only include comments after this timestamp

    Returns:
        Formatted string of feedback
    """
    response = await get_issue_comments(repo_url, issue_number, since=since)

    if response.not_modified or not response.data:
        return ""

    parts = []
    for comment in response.data:
        body = comment['body']

        # Extract feedback from /revise command if present
        command, feedback = parse_issue_command(body)
        if command == "revise" and feedback:
            body = feedback  # Use just the feedback text, not the /revise prefix

        parts.append(f"## Comment from @{comment['user']}\n{body}")

    return "\n\n---\n\n".join(parts) if parts else ""


class CreatedIssue(BaseModel):
    """A newly created GitHub issue."""

    number: int
    url: str
    title: str


async def create_github_issue(
    repo_url: str,
    title: str,
    body: str,
    labels: list[str] | None = None,
) -> CreatedIssue | None:
    """Create a GitHub issue.

    Args:
        repo_url: GitHub repository URL
        title: Issue title
        body: Issue body (markdown)
        labels: Optional list of labels to apply

    Returns:
        CreatedIssue with number and URL, or None if creation failed
    """
    owner, repo = _parse_repo(repo_url)

    payload = {
        "title": title,
        "body": body,
    }
    if labels:
        payload["labels"] = labels

    async with httpx.AsyncClient() as client:
        try:
            response = await client.post(
                f"{GITHUB_API_BASE}/repos/{owner}/{repo}/issues",
                headers=_get_headers(),
                json=payload,
            )
            response.raise_for_status()
            data = response.json()

            return CreatedIssue(
                number=data["number"],
                url=data["html_url"],
                title=data["title"],
            )
        except Exception as e:
            logger.error(f"Failed to create GitHub issue: {e}")
            return None


async def update_github_issue(
    repo_url: str,
    issue_number: int,
    title: str | None = None,
    body: str | None = None,
    state: str | None = None,
    labels: list[str] | None = None,
) -> bool:
    """Update a GitHub issue.

    Args:
        repo_url: GitHub repository URL
        issue_number: Issue number to update
        title: New title (optional)
        body: New body (optional)
        state: New state - 'open' or 'closed' (optional)
        labels: New labels (optional, replaces existing)

    Returns:
        True if update succeeded, False otherwise
    """
    owner, repo = _parse_repo(repo_url)

    payload = {}
    if title is not None:
        payload["title"] = title
    if body is not None:
        payload["body"] = body
    if state is not None:
        payload["state"] = state
    if labels is not None:
        payload["labels"] = labels

    if not payload:
        return True  # Nothing to update

    async with httpx.AsyncClient() as client:
        try:
            response = await client.patch(
                f"{GITHUB_API_BASE}/repos/{owner}/{repo}/issues/{issue_number}",
                headers=_get_headers(),
                json=payload,
            )
            response.raise_for_status()
            logger.info(f"Updated issue #{issue_number} in {owner}/{repo}")
            return True
        except Exception as e:
            logger.error(f"Failed to update GitHub issue #{issue_number}: {e}")
            return False


async def add_issue_comment(
    repo_url: str,
    issue_number: int,
    body: str,
) -> bool:
    """Add a comment to a GitHub issue.

    Args:
        repo_url: GitHub repository URL
        issue_number: Issue number
        body: Comment body (markdown)

    Returns:
        True if comment was added, False otherwise
    """
    owner, repo = _parse_repo(repo_url)

    async with httpx.AsyncClient() as client:
        try:
            response = await client.post(
                f"{GITHUB_API_BASE}/repos/{owner}/{repo}/issues/{issue_number}/comments",
                headers=_get_headers(),
                json={"body": body},
            )
            response.raise_for_status()
            logger.info(f"Added comment to issue #{issue_number} in {owner}/{repo}")
            return True
        except Exception as e:
            logger.error(f"Failed to add comment to issue #{issue_number}: {e}")
            return False
