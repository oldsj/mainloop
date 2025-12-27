"""Claude agent orchestration using the Agent SDK."""

from claude_agent_sdk import (
    query,
    ClaudeAgentOptions,
    AssistantMessage,
    ResultMessage,
    TextBlock,
)
from models import AgentTask, AgentResponse
from mainloop.config import settings


class ClaudeAgent:
    """Client for interacting with Claude Code via the Agent SDK."""

    def __init__(self):
        """Initialize Claude agent client."""
        pass

    async def execute_task(self, task: AgentTask) -> AgentResponse:
        """Execute a task via Claude Code Agent SDK."""
        try:
            options = ClaudeAgentOptions(
                model=settings.claude_model,
                permission_mode="bypassPermissions",
                cwd=settings.claude_workspace,
            )

            collected_text: list[str] = []

            async for message in query(prompt=task.prompt, options=options):
                if isinstance(message, AssistantMessage):
                    for block in message.content:
                        if isinstance(block, TextBlock):
                            collected_text.append(block.text)
                elif isinstance(message, ResultMessage):
                    if message.is_error:
                        return AgentResponse(
                            task_id=task.id,
                            content=f"Error: {message.result or 'Unknown error'}"
                        )

            return AgentResponse(
                task_id=task.id,
                content="\n".join(collected_text) if collected_text else "No response"
            )

        except Exception as e:
            return AgentResponse(
                task_id=task.id,
                content=f"Claude SDK error: {str(e)}"
            )

    async def stream_task(self, task: AgentTask):
        """Stream task execution results."""
        options = ClaudeAgentOptions(
            model=settings.claude_model,
            permission_mode="bypassPermissions",
            cwd=settings.claude_workspace,
        )

        async for message in query(prompt=task.prompt, options=options):
            if isinstance(message, AssistantMessage):
                for block in message.content:
                    if isinstance(block, TextBlock):
                        yield block.text


# Global agent instance
claude_agent = ClaudeAgent()
