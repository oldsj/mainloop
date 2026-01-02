#!/usr/bin/env python3
"""
Claude Agent Worker API using the Agent SDK.
Each worker maintains its own independent session with context compaction support.
"""
import json
import logging
from pathlib import Path

from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    ResultMessage,
    SystemMessage,
    TextBlock,
    query,
)
from fastapi import FastAPI, HTTPException
from fastapi.responses import StreamingResponse
from pydantic import BaseModel

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)

app = FastAPI(title="Claude Agent Worker API")

CREDENTIALS_PATH = Path.home() / ".claude" / ".credentials.json"


class ExecuteRequest(BaseModel):
    """Request to execute Claude Code."""

    prompt: str
    workspace: str = "/workspace"
    model: str = "haiku"
    session_id: str | None = None  # Resume from existing session
    max_turns: int | None = None  # Limit agent turns


class ExecuteResponse(BaseModel):
    """Response from Claude Code execution."""

    output: str
    session_id: str | None = None
    cost_usd: float | None = None
    error: str | None = None
    compacted: bool = False  # Whether context was compacted during execution
    compaction_count: int = 0  # Number of compactions that occurred


class AuthStatus(BaseModel):
    """Authentication status."""

    authenticated: bool
    expires_at: int | None = None
    subscription_type: str | None = None
    error: str | None = None


def get_credentials() -> dict | None:
    """Read credentials from file."""
    if CREDENTIALS_PATH.exists():
        try:
            return json.loads(CREDENTIALS_PATH.read_text())
        except Exception:
            return None
    return None


def check_auth() -> AuthStatus:
    """Check if we have valid authentication."""
    creds = get_credentials()
    if not creds:
        return AuthStatus(authenticated=False, error="No credentials found")

    oauth = creds.get("claudeAiOauth", {})
    if not oauth.get("accessToken"):
        return AuthStatus(authenticated=False, error="No access token")

    return AuthStatus(
        authenticated=True,
        expires_at=oauth.get("expiresAt"),
        subscription_type=oauth.get("subscriptionType"),
    )


@app.get("/health")
async def health():
    """Health check endpoint."""
    auth = check_auth()
    return {
        "status": "ok",
        "service": "claude-agent-worker",
        "authenticated": auth.authenticated,
    }


@app.get("/auth/status", response_model=AuthStatus)
async def auth_status():
    """Check authentication status."""
    return check_auth()


@app.post("/execute", response_model=ExecuteResponse)
async def execute_claude(request: ExecuteRequest):
    """Execute a prompt using Claude Agent SDK with session and compaction support."""
    try:
        options = ClaudeAgentOptions(
            model=request.model,
            permission_mode="bypassPermissions",
            cwd=request.workspace,
            resume=request.session_id,  # Resume existing session if provided
            max_turns=request.max_turns,  # Limit agent turns
        )

        collected_text: list[str] = []
        session_id: str | None = None
        cost_usd: float | None = None
        compaction_count: int = 0

        async for message in query(prompt=request.prompt, options=options):
            if isinstance(message, AssistantMessage):
                for block in message.content:
                    if isinstance(block, TextBlock):
                        collected_text.append(block.text)
            elif isinstance(message, ResultMessage):
                session_id = message.session_id
                cost_usd = message.total_cost_usd
                if message.is_error:
                    return ExecuteResponse(
                        output="",
                        session_id=session_id,
                        cost_usd=cost_usd,
                        error=message.result or "Unknown error",
                        compacted=compaction_count > 0,
                        compaction_count=compaction_count,
                    )
            elif isinstance(message, SystemMessage):
                # Track compaction events (context was automatically summarized)
                if message.subtype == "compact_boundary":
                    compaction_count += 1
                    data = message.data or {}
                    pre_tokens = data.get("pre_tokens", 0)
                    trigger = data.get("trigger", "unknown")
                    logger.info(
                        f"Context compacted ({trigger}): {pre_tokens} tokens summarized"
                    )

        return ExecuteResponse(
            output="\n".join(collected_text) if collected_text else "",
            session_id=session_id,
            cost_usd=cost_usd,
            compacted=compaction_count > 0,
            compaction_count=compaction_count,
        )

    except Exception as e:
        logger.exception(f"Execute failed: {e}")
        raise HTTPException(status_code=500, detail=str(e))


@app.post("/execute/stream")
async def execute_claude_stream(request: ExecuteRequest):
    """Stream execution results using Claude Agent SDK."""

    async def generate():
        try:
            options = ClaudeAgentOptions(
                model=request.model,
                permission_mode="bypassPermissions",
                cwd=request.workspace,
            )

            async for message in query(prompt=request.prompt, options=options):
                if isinstance(message, AssistantMessage):
                    for block in message.content:
                        if isinstance(block, TextBlock):
                            yield f"data: {json.dumps({'type': 'text', 'content': block.text})}\n\n"
                elif isinstance(message, ResultMessage):
                    yield f"data: {json.dumps({'type': 'result', 'session_id': message.session_id, 'cost_usd': message.total_cost_usd, 'is_error': message.is_error})}\n\n"

            yield "data: [DONE]\n\n"
        except Exception as e:
            yield f"data: {json.dumps({'type': 'error', 'error': str(e)})}\n\n"

    return StreamingResponse(
        generate(),
        media_type="text/event-stream",
        headers={
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
        },
    )


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host="0.0.0.0", port=8001)
