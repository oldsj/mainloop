"""Client for the Claude Agent container HTTP API."""

import logging
from typing import AsyncGenerator

import httpx
from mainloop.config import settings
from pydantic import BaseModel

logger = logging.getLogger(__name__)


class ExecuteResponse(BaseModel):
    """Response from Claude Agent execution."""

    output: str
    session_id: str | None = None
    cost_usd: float | None = None
    error: str | None = None


class ClaudeAgentClient:
    """HTTP client for the Claude Agent container."""

    def __init__(self, base_url: str | None = None):
        self.base_url = (
            base_url or settings.claude_agent_url or "http://claude-agent:8001"
        )

    async def execute(
        self,
        prompt: str,
        model: str = "sonnet",
        timeout: float = 300.0,
    ) -> ExecuteResponse:
        """
        Execute a prompt using Claude Agent SDK.

        Claude-agent manages its own isolated workspace internally.

        Args:
            prompt: The prompt to execute
            model: Model to use (haiku, sonnet, opus)
            timeout: Request timeout in seconds

        Returns:
            ExecuteResponse with output or error

        """
        async with httpx.AsyncClient(timeout=timeout) as client:
            try:
                response = await client.post(
                    f"{self.base_url}/execute",
                    json={
                        "prompt": prompt,
                        "model": model,
                    },
                )
                response.raise_for_status()
                return ExecuteResponse(**response.json())
            except httpx.HTTPStatusError as e:
                logger.error(f"Claude agent HTTP error: {e}")
                return ExecuteResponse(
                    output="",
                    error=f"HTTP {e.response.status_code}: {e.response.text}",
                )
            except httpx.RequestError as e:
                logger.error(f"Claude agent request error: {e}")
                return ExecuteResponse(
                    output="",
                    error=f"Request failed: {str(e)}",
                )

    async def execute_stream(
        self,
        prompt: str,
        model: str = "sonnet",
        timeout: float = 300.0,
    ) -> AsyncGenerator[dict, None]:
        """
        Stream execution results from Claude Agent SDK.

        Yields:
            Dict events with type: 'text', 'result', or 'error'

        """
        import json

        async with httpx.AsyncClient(timeout=timeout) as client:
            try:
                async with client.stream(
                    "POST",
                    f"{self.base_url}/execute/stream",
                    json={
                        "prompt": prompt,
                        "model": model,
                    },
                ) as response:
                    response.raise_for_status()
                    async for line in response.aiter_lines():
                        if line.startswith("data: "):
                            data = line[6:]
                            if data == "[DONE]":
                                break
                            try:
                                yield json.loads(data)
                            except json.JSONDecodeError:
                                continue
            except httpx.HTTPStatusError as e:
                yield {"type": "error", "error": f"HTTP {e.response.status_code}"}
            except httpx.RequestError as e:
                yield {"type": "error", "error": str(e)}

    async def health_check(self) -> dict:
        """Check if the Claude Agent service is healthy."""
        async with httpx.AsyncClient(timeout=5.0) as client:
            try:
                response = await client.get(f"{self.base_url}/health")
                response.raise_for_status()
                return response.json()
            except Exception as e:
                return {"status": "error", "error": str(e)}


# Singleton client instance
_client: ClaudeAgentClient | None = None


def get_claude_agent_client() -> ClaudeAgentClient:
    """Get the singleton Claude Agent client."""
    global _client
    if _client is None:
        _client = ClaudeAgentClient()
    return _client
