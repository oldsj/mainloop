"""Task routing service - matches user messages to active tasks."""

import re
import logging
from dataclasses import dataclass

from models import WorkerTask, TaskStatus
from mainloop.db import db

logger = logging.getLogger(__name__)


@dataclass
class RouteMatch:
    """A potential task match for routing."""

    task: WorkerTask
    confidence: float  # 0.0 to 1.0
    match_reasons: list[str]


def extract_keywords(message: str) -> list[str]:
    """Extract routing keywords from a user message.

    Looks for:
    - Domain names (understanding.news, example.com)
    - Repository names (owner/repo)
    - Technical terms (background, header, button, etc.)
    - Color terms
    """
    keywords: list[str] = []
    message_lower = message.lower()

    # Extract domain-like patterns
    domain_pattern = r"\b([a-z0-9-]+\.(?:com|org|net|io|dev|news|app|co))\b"
    keywords.extend(re.findall(domain_pattern, message_lower))

    # Extract GitHub repo patterns (owner/repo)
    repo_pattern = r"\b([a-z0-9_-]+/[a-z0-9_-]+)\b"
    keywords.extend(re.findall(repo_pattern, message_lower))

    # Extract common UI/code terms
    ui_terms = [
        "background",
        "header",
        "footer",
        "button",
        "color",
        "style",
        "layout",
        "font",
        "image",
        "icon",
        "nav",
        "navbar",
        "sidebar",
        "menu",
        "form",
        "input",
        "modal",
        "dialog",
        "card",
        "table",
        "list",
        "api",
        "endpoint",
        "route",
        "auth",
        "login",
        "signup",
        "database",
        "schema",
        "test",
        "bug",
        "fix",
        "feature",
    ]
    for term in ui_terms:
        if term in message_lower:
            keywords.append(term)

    # Extract color terms
    colors = [
        "red",
        "blue",
        "green",
        "yellow",
        "pink",
        "grey",
        "gray",
        "white",
        "black",
        "purple",
        "orange",
        "cyan",
        "magenta",
        "brown",
        "dark",
        "light",
    ]
    for color in colors:
        if color in message_lower:
            keywords.append(color)

    # Deduplicate
    return list(set(keywords))


async def find_matching_tasks(
    user_id: str,
    message: str,
    min_confidence: float = 0.3,
) -> list[RouteMatch]:
    """Find active tasks that might match a user message.

    Matching criteria:
    1. Exact repo_url match in message
    2. Keywords overlap (description, keywords array)
    3. Only considers PLANNING, WAITING_PLAN_REVIEW, IMPLEMENTING, or UNDER_REVIEW tasks
    """
    # Get active tasks
    active_statuses = [
        TaskStatus.PLANNING.value,
        TaskStatus.WAITING_PLAN_REVIEW.value,
        TaskStatus.IMPLEMENTING.value,
        TaskStatus.UNDER_REVIEW.value,
    ]

    all_tasks: list[WorkerTask] = []
    for status in active_statuses:
        tasks = await db.list_worker_tasks(user_id=user_id, status=status)
        all_tasks.extend(tasks)

    if not all_tasks:
        return []

    # Extract keywords from incoming message
    message_keywords = set(extract_keywords(message))
    message_lower = message.lower()

    matches: list[RouteMatch] = []

    for task in all_tasks:
        confidence = 0.0
        reasons: list[str] = []

        # Check repo URL match
        if task.repo_url:
            # Extract repo name from URL
            repo_name = task.repo_url.rstrip("/").split("/")[-1].replace(".git", "")
            if repo_name.lower() in message_lower:
                confidence += 0.4
                reasons.append(f"Repo '{repo_name}' mentioned")

            # Also check for domain if it's in the repo URL
            # e.g., "understanding.news" from github.com/user/understanding.news
            if "." in repo_name and repo_name.lower() in message_lower:
                confidence += 0.2
                reasons.append(f"Domain '{repo_name}' mentioned")

        # Check keyword overlap
        task_keywords = set(task.keywords or [])
        # Also extract keywords from task description
        task_keywords.update(extract_keywords(task.description))

        overlap = message_keywords & task_keywords
        if overlap:
            # Score based on how many keywords overlap
            overlap_score = len(overlap) / max(len(message_keywords), 1)
            confidence += 0.4 * overlap_score
            reasons.append(f"Keywords: {', '.join(sorted(overlap))}")

        # Check PR URL in message
        if task.pr_url:
            pr_num_str = str(task.pr_number) if task.pr_number else ""
            if pr_num_str and f"#{pr_num_str}" in message:
                confidence += 0.3
                reasons.append(f"PR #{pr_num_str} mentioned")

        # Only include if above minimum confidence
        if confidence >= min_confidence:
            matches.append(
                RouteMatch(
                    task=task,
                    confidence=min(confidence, 1.0),
                    match_reasons=reasons,
                )
            )

    # Sort by confidence descending
    matches.sort(key=lambda m: m.confidence, reverse=True)

    logger.info(
        f"Found {len(matches)} matching tasks for message: {message[:50]}..."
        if len(message) > 50
        else f"Found {len(matches)} matching tasks for message: {message}"
    )

    return matches


def should_skip_plan(message: str) -> bool:
    """Check if user wants to skip plan phase.

    Detected phrases: "just do it", "skip plan", "no plan needed", "go ahead"
    """
    message_lower = message.lower()
    skip_phrases = [
        "just do it",
        "skip plan",
        "no plan",
        "go ahead",
        "don't plan",
        "dont plan",
        "skip planning",
        "no planning",
        "straight to",
        "directly",
    ]
    return any(phrase in message_lower for phrase in skip_phrases)
