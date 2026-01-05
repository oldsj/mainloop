"""Mock Claude Agent SDK for fast testing without API calls.

This module provides canned responses that simulate Claude's behavior,
including spawn_task tool calls when appropriate.

When spawn_task_tool is provided, the mock will actually call it
to create real tasks, enabling full E2E testing without Claude API costs.
"""

import logging
import re
from dataclasses import dataclass
from typing import Any, AsyncIterator, Callable

logger = logging.getLogger(__name__)


@dataclass
class MockTextBlock:
    """Mock TextBlock matching claude_agent_sdk.TextBlock interface."""

    text: str
    type: str = "text"


@dataclass
class MockToolUseBlock:
    """Mock ToolUseBlock for tool calls."""

    id: str
    name: str
    input: dict[str, Any]
    type: str = "tool_use"


@dataclass
class MockAssistantMessage:
    """Mock AssistantMessage matching claude_agent_sdk interface."""

    content: list[MockTextBlock | MockToolUseBlock]
    role: str = "assistant"


@dataclass
class MockResultMessage:
    """Mock ResultMessage matching claude_agent_sdk interface."""

    result: str | None
    is_error: bool = False


# Pattern detection for different intents
SPAWN_PATTERNS = [
    r"\b(implement|create|build|add|fix|update|refactor|spawn|start)\b.*\b(feature|bug|task|agent|worker)\b",
    r"\b(spawn|start|create)\b.*\b(task|agent|worker)\b",
    r"\byes\b.*\b(spawn|proceed|confirm|go ahead)\b",
    r"\b(go ahead|confirm|yes|proceed)\b",
]

GREETING_PATTERNS = [
    r"^(hi|hello|hey|greetings|good\s+(morning|afternoon|evening))[\s!.,]*$",
]


class MockClaudeResponse:
    """Provides predictable mock responses for testing."""

    @staticmethod
    def _detect_intent(prompt: str) -> str:
        """Detect user intent from prompt."""
        lower_prompt = prompt.lower()

        # Check for greetings
        for pattern in GREETING_PATTERNS:
            if re.search(pattern, lower_prompt, re.IGNORECASE):
                return "greeting"

        # Check for spawn-related requests
        for pattern in SPAWN_PATTERNS:
            if re.search(pattern, lower_prompt, re.IGNORECASE):
                return "spawn"

        return "general"

    @staticmethod
    def _extract_task_details(prompt: str) -> tuple[str, str | None]:
        """Extract task description and repo URL from prompt."""
        # Look for GitHub URLs
        url_match = re.search(r"https://github\.com/[\w-]+/[\w-]+", prompt)
        repo_url = url_match.group(0) if url_match else None

        # Use the prompt as task description
        task_desc = prompt[:200] if len(prompt) > 200 else prompt

        return task_desc, repo_url

    @classmethod
    async def query_mock(
        cls,
        prompt: str,
        spawn_task_tool: Callable | None = None,
        **kwargs,
    ) -> AsyncIterator[MockAssistantMessage | MockResultMessage]:
        """Mock query() that returns canned responses.

        Simulates Claude behavior including:
        - Greeting responses
        - Task spawning (actually calls spawn_task_tool if provided)
        - General conversation

        Args:
            prompt: The user's message
            spawn_task_tool: Optional spawn_task function to actually create tasks
            **kwargs: Additional arguments (ignored, for API compatibility)

        """
        intent = cls._detect_intent(prompt)
        logger.info(f"Mock Claude: detected intent '{intent}' from prompt")

        if intent == "greeting":
            text = "Hello! I'm here to help. I can spawn autonomous coding agents to work on your projects. What would you like to accomplish today?"
            yield MockAssistantMessage(content=[MockTextBlock(text=text)])
            yield MockResultMessage(result=text, is_error=False)
            return

        if intent == "spawn":
            task_desc, repo_url = cls._extract_task_details(prompt)

            if not repo_url:
                # Ask for repo URL
                text = (
                    "I'd be happy to help with that! To spawn a worker agent, "
                    "I'll need the GitHub repository URL. Could you provide it?"
                )
                yield MockAssistantMessage(content=[MockTextBlock(text=text)])
                yield MockResultMessage(result=text, is_error=False)
                return

            # Actually call spawn_task_tool if available
            if spawn_task_tool:
                logger.info(f"Mock Claude: calling spawn_task_tool for {repo_url}")

                # Call the real spawn_task tool
                try:
                    result = await spawn_task_tool(
                        {
                            "task_description": task_desc,
                            "repo_url": repo_url,
                            "skip_planning": False,
                        }
                    )

                    # Check if tool call succeeded
                    if result.get("is_error"):
                        error_text = result.get("content", [{}])[0].get(
                            "text", "Unknown error"
                        )
                        text = f"I tried to spawn a task but encountered an error: {error_text}"
                        yield MockAssistantMessage(content=[MockTextBlock(text=text)])
                        yield MockResultMessage(result=text, is_error=False)
                        return

                    # Success - extract task info from result
                    result_text = result.get("content", [{}])[0].get("text", "")
                    text = (
                        f"I've spawned a worker agent to work on this.\n\n{result_text}"
                    )
                    yield MockAssistantMessage(content=[MockTextBlock(text=text)])
                    yield MockResultMessage(result=text, is_error=False)
                    return

                except Exception as e:
                    logger.error(f"Mock Claude: spawn_task_tool error: {e}")
                    text = f"I tried to spawn a task but encountered an error: {e}"
                    yield MockAssistantMessage(content=[MockTextBlock(text=text)])
                    yield MockResultMessage(result=text, is_error=False)
                    return
            else:
                # No tool available - just acknowledge
                text = f"I understood your request about: {task_desc[:100]}... However, I don't have the spawn_task tool available in this context."
                yield MockAssistantMessage(content=[MockTextBlock(text=text)])
                yield MockResultMessage(result=text, is_error=False)
            return

        # General response
        # Truncate prompt for readability
        truncated = prompt[:100] + "..." if len(prompt) > 100 else prompt
        text = f"I understood your message. Here's my response to: {truncated}"
        yield MockAssistantMessage(content=[MockTextBlock(text=text)])
        yield MockResultMessage(result=text, is_error=False)
