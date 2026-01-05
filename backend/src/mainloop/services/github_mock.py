"""Mock GitHub service for testing without real GitHub API.

This module provides mock implementations that simulate GitHub operations
and maintain state so tests can verify behavior.
"""

import logging
from datetime import datetime, timezone
from typing import Literal

from pydantic import BaseModel

logger = logging.getLogger(__name__)


# ==================== Mock State ====================
# These store mock data so tests can verify state changes


class MockIssue(BaseModel):
    """Mock GitHub issue state."""

    number: int
    title: str
    body: str
    state: Literal["open", "closed"] = "open"
    labels: list[str] = []
    created_at: datetime = datetime.now(timezone.utc)
    updated_at: datetime = datetime.now(timezone.utc)


class MockComment(BaseModel):
    """Mock GitHub comment."""

    id: int
    issue_number: int
    body: str
    user: str = "test-user"
    created_at: datetime = datetime.now(timezone.utc)
    updated_at: datetime = datetime.now(timezone.utc)
    reactions: list[str] = []


class MockPR(BaseModel):
    """Mock GitHub PR state."""

    number: int
    title: str
    body: str | None
    state: Literal["open", "closed"] = "open"
    merged: bool = False
    head_branch: str = "feature/test"
    head_sha: str = "abc1234"
    base_branch: str = "main"
    created_at: datetime = datetime.now(timezone.utc)
    updated_at: datetime = datetime.now(timezone.utc)


class MockCheckRun(BaseModel):
    """Mock GitHub Actions check run."""

    name: str
    status: Literal["queued", "in_progress", "completed"]
    conclusion: (
        Literal["success", "failure", "neutral", "cancelled", "skipped", "timed_out"]
        | None
    ) = None


class MockGitHubState:
    """Global mock state for GitHub operations.

    Call reset() between tests to clear state.
    """

    def __init__(self):
        self.reset()

    def reset(self):
        """Reset all mock state."""
        self._issues: dict[int, MockIssue] = {}
        self._comments: dict[int, MockComment] = {}
        self._prs: dict[int, MockPR] = {}
        self._check_runs: dict[int, list[MockCheckRun]] = {}  # PR number -> check runs
        self._next_issue_number = 1
        self._next_comment_id = 1
        self._next_pr_number = 1
        logger.info("Mock GitHub state reset")

    def add_issue(self, issue: MockIssue):
        """Add an issue to mock state."""
        self._issues[issue.number] = issue

    def add_pr(self, pr: MockPR):
        """Add a PR to mock state."""
        self._prs[pr.number] = pr

    def set_check_runs(self, pr_number: int, runs: list[MockCheckRun]):
        """Set check runs for a PR."""
        self._check_runs[pr_number] = runs

    def get_issue(self, number: int) -> MockIssue | None:
        """Get issue by number."""
        return self._issues.get(number)

    def get_pr(self, number: int) -> MockPR | None:
        """Get PR by number."""
        return self._prs.get(number)

    def get_comments(self, issue_number: int) -> list[MockComment]:
        """Get comments for an issue."""
        return [c for c in self._comments.values() if c.issue_number == issue_number]

    def next_issue_number(self) -> int:
        """Get next issue number and increment."""
        num = self._next_issue_number
        self._next_issue_number += 1
        return num

    def next_comment_id(self) -> int:
        """Get next comment ID and increment."""
        cid = self._next_comment_id
        self._next_comment_id += 1
        return cid

    def next_pr_number(self) -> int:
        """Get next PR number and increment."""
        num = self._next_pr_number
        self._next_pr_number += 1
        return num


# Global mock state
mock_state = MockGitHubState()


# ==================== Import Types ====================
# Re-export types from github_pr for compatibility

from mainloop.services.github_pr import (  # noqa: E402
    CheckRunStatus,
    CombinedCheckStatus,
    ConditionalResponse,
    CreatedIssue,
    PRComment,
    PRReview,
    PRStatus,
)

# ==================== Mock Implementations ====================


async def create_github_issue(
    repo_url: str,
    title: str,
    body: str,
    labels: list[str] | None = None,
) -> CreatedIssue | None:
    """Mock create_github_issue - creates issue in mock state."""
    number = mock_state.next_issue_number()

    issue = MockIssue(
        number=number,
        title=title,
        body=body,
        labels=labels or [],
    )
    mock_state.add_issue(issue)

    owner, repo = _parse_repo(repo_url)
    url = f"https://github.com/{owner}/{repo}/issues/{number}"

    logger.info(f"[mock] Created issue #{number}: {title}")

    return CreatedIssue(
        number=number,
        url=url,
        title=title,
    )


async def update_github_issue(
    repo_url: str,
    issue_number: int,
    title: str | None = None,
    body: str | None = None,
    state: str | None = None,
    labels: list[str] | None = None,
) -> bool:
    """Mock update_github_issue - updates issue in mock state."""
    issue = mock_state.get_issue(issue_number)
    if not issue:
        logger.warning(f"[mock] Issue #{issue_number} not found")
        return False

    if title is not None:
        issue.title = title
    if body is not None:
        issue.body = body
    if state is not None:
        issue.state = state
    if labels is not None:
        issue.labels = labels
    issue.updated_at = datetime.now(timezone.utc)

    logger.info(f"[mock] Updated issue #{issue_number}")
    return True


async def add_issue_comment(
    repo_url: str,
    issue_number: int,
    body: str,
    return_id: bool = False,
) -> bool | int:
    """Mock add_issue_comment - adds comment to mock state."""
    comment_id = mock_state.next_comment_id()

    comment = MockComment(
        id=comment_id,
        issue_number=issue_number,
        body=body,
    )
    mock_state._comments[comment_id] = comment

    logger.info(f"[mock] Added comment {comment_id} to issue #{issue_number}")

    if return_id:
        return comment_id
    return True


async def get_issue_status(
    repo_url: str,
    issue_number: int,
    etag: str | None = None,
    if_modified_since: datetime | None = None,
) -> ConditionalResponse:
    """Mock get_issue_status - returns issue from mock state."""
    issue = mock_state.get_issue(issue_number)
    if not issue:
        return ConditionalResponse(data={"state": "not_found"})

    owner, repo = _parse_repo(repo_url)

    return ConditionalResponse(
        data={
            "number": issue.number,
            "state": issue.state,
            "title": issue.title,
            "body": issue.body,
            "url": f"https://github.com/{owner}/{repo}/issues/{issue.number}",
            "created_at": issue.created_at.isoformat(),
            "updated_at": issue.updated_at.isoformat(),
            "labels": issue.labels,
        },
        etag=f"etag-{issue_number}-{issue.updated_at.timestamp()}",
    )


async def get_issue_comments(
    repo_url: str,
    issue_number: int,
    since: datetime | None = None,
    etag: str | None = None,
) -> ConditionalResponse:
    """Mock get_issue_comments - returns comments from mock state."""
    comments = mock_state.get_comments(issue_number)

    if since:
        comments = [c for c in comments if c.created_at > since]

    return ConditionalResponse(
        data=[
            {
                "id": c.id,
                "body": c.body,
                "user": c.user,
                "created_at": c.created_at.isoformat(),
                "updated_at": c.updated_at.isoformat(),
                "url": f"https://github.com/test/repo/issues/{issue_number}#issuecomment-{c.id}",
            }
            for c in comments
        ],
        etag=f"etag-comments-{issue_number}",
    )


async def get_comment_reactions(
    repo_url: str,
    comment_id: int,
) -> list[str]:
    """Mock get_comment_reactions - returns reactions from mock state."""
    comment = mock_state._comments.get(comment_id)
    if not comment:
        return []
    return comment.reactions


async def get_pr_status(repo_url: str, pr_number: int) -> PRStatus | None:
    """Mock get_pr_status - returns PR from mock state."""
    pr = mock_state.get_pr(pr_number)
    if not pr:
        return None

    owner, repo = _parse_repo(repo_url)

    return PRStatus(
        number=pr.number,
        state=pr.state,
        merged=pr.merged,
        title=pr.title,
        body=pr.body,
        head_branch=pr.head_branch,
        head_sha=pr.head_sha,
        base_branch=pr.base_branch,
        url=f"https://github.com/{owner}/{repo}/pull/{pr.number}",
        created_at=pr.created_at,
        updated_at=pr.updated_at,
        mergeable=True,
        review_decision=None,
    )


async def get_pr_comments(
    repo_url: str,
    pr_number: int,
    since: datetime | None = None,
) -> list[PRComment]:
    """Mock get_pr_comments - returns empty list (no feedback by default)."""
    # In mock mode, we don't simulate PR review comments
    # Plan approval comes via DBOS message, not GitHub polling
    return []


async def get_pr_reviews(
    repo_url: str,
    pr_number: int,
) -> list[PRReview]:
    """Mock get_pr_reviews - returns empty list."""
    return []


async def get_check_status(repo_url: str, pr_number: int) -> CombinedCheckStatus:
    """Mock get_check_status - returns success by default."""
    # Check if we have configured check runs for this PR
    runs = mock_state._check_runs.get(pr_number)

    if not runs:
        # Default: all checks pass
        return CombinedCheckStatus(
            status="success",
            total_count=1,
            check_runs=[
                CheckRunStatus(
                    name="CI",
                    status="completed",
                    conclusion="success",
                    details_url=None,
                )
            ],
            failed_runs=[],
        )

    # Convert mock runs to CheckRunStatus
    check_runs = [
        CheckRunStatus(
            name=r.name,
            status=r.status,
            conclusion=r.conclusion,
            details_url=None,
        )
        for r in runs
    ]

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
    """Mock get_check_failure_logs - returns empty string."""
    return ""


async def is_pr_merged(repo_url: str, pr_number: int) -> bool:
    """Mock is_pr_merged."""
    pr = mock_state.get_pr(pr_number)
    return pr.merged if pr else False


async def is_pr_approved(repo_url: str, pr_number: int) -> bool:
    """Mock is_pr_approved - returns False by default."""
    return False


async def add_reaction_to_comment(
    repo_url: str,
    comment_id: int,
    is_review_comment: bool = False,
    reaction: str = "eyes",
) -> bool:
    """Mock add_reaction_to_comment - adds reaction to mock state."""
    comment = mock_state._comments.get(comment_id)
    if comment:
        comment.reactions.append(reaction)
        logger.info(f"[mock] Added {reaction} reaction to comment {comment_id}")
        return True
    return False


async def acknowledge_comments(
    repo_url: str,
    comments: list[PRComment],
) -> None:
    """Mock acknowledge_comments."""
    for comment in comments:
        await add_reaction_to_comment(
            repo_url,
            comment.id,
            is_review_comment=comment.is_review_comment,
            reaction="eyes",
        )


async def format_feedback_for_agent(
    repo_url: str,
    pr_number: int,
    since: datetime | None = None,
) -> str:
    """Mock format_feedback_for_agent - returns empty string."""
    return ""


async def format_issue_feedback_for_agent(
    repo_url: str,
    issue_number: int,
    since: datetime | None = None,
) -> str:
    """Mock format_issue_feedback_for_agent - returns empty string."""
    return ""


# ==================== Utility Functions ====================


def _parse_repo(repo_url: str) -> tuple[str, str]:
    """Parse owner and repo from a GitHub URL."""
    url = repo_url.rstrip("/")
    if url.endswith(".git"):
        url = url[:-4]
    parts = url.split("/")
    return parts[-2], parts[-1]


# Functions that don't need mocking (pure logic)
