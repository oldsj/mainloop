"""Claude agent orchestration."""

import httpx
from models import AgentTask, AgentResponse
from mainloop.config import settings


class ClaudeAgent:
    """Client for interacting with Claude Code CLI container."""

    def __init__(self, base_url: str | None = None):
        """Initialize Claude agent client."""
        self.base_url = base_url or settings.agent_controller_url
        self.client = httpx.AsyncClient(timeout=300.0)  # 5 minute timeout

    async def execute_task(self, task: AgentTask) -> AgentResponse:
        """Execute a task via Claude Code CLI container."""
        try:
            response = await self.client.post(
                f"{self.base_url}/execute",
                json={
                    "prompt": task.prompt,
                    "workspace": "/workspace"
                }
            )
            response.raise_for_status()
            data = response.json()

            # Check for errors from Claude execution
            if data.get("error"):
                return AgentResponse(
                    task_id=task.id,
                    content=f"Claude execution error: {data['error']}"
                )

            return AgentResponse(
                task_id=task.id,
                content=data.get("output", "")
            )
        except httpx.HTTPError as e:
            return AgentResponse(
                task_id=task.id,
                content=f"Failed to communicate with Claude agent: {str(e)}"
            )

    async def stream_task(self, task: AgentTask):
        """Stream task execution results."""
        # TODO: Implement SSE streaming from Claude agent
        yield "Mock streaming response"


# Global agent instance
claude_agent = ClaudeAgent()
