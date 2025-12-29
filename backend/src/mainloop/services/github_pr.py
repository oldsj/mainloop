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


async def format_feedback_for_agent(
    repo_url: str,
    pr_number: int,
    since: datetime | None = None,
) -> str:
    """Format PR feedback as context for the agent.

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

    # Add review feedback
    for review in reviews:
        if review.state in ("CHANGES_REQUESTED", "COMMENTED") and review.body:
            parts.append(f"## Review from @{review.user} ({review.state})\n{review.body}")

    # Add comments
    for comment in comments:
        comment_type = "Code comment" if comment.is_review_comment else "Comment"
        parts.append(f"## {comment_type} from @{comment.user}\n{comment.body}")

    return "\n\n---\n\n".join(parts) if parts else ""
