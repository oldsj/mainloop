"""Claude agent orchestration."""

import httpx
from models import AgentTask, AgentResponse


class ClaudeAgent:
    """Client for interacting with Claude Code CLI container."""

    def __init__(self, base_url: str = "http://claude-agent:8080"):
        """Initialize Claude agent client."""
        self.base_url = base_url
        self.client = httpx.AsyncClient()

    async def execute_task(self, task: AgentTask) -> AgentResponse:
        """Execute a task via Claude Code CLI container."""
        # TODO: Implement actual communication with Claude agent container
        # For now, return a mock response
        return AgentResponse(
            task_id=task.id,
            content="Mock response from Claude agent"
        )

    async def stream_task(self, task: AgentTask):
        """Stream task execution results."""
        # TODO: Implement SSE streaming from Claude agent
        yield "Mock streaming response"


# Global agent instance
claude_agent = ClaudeAgent()
