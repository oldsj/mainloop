#!/usr/bin/env python3
"""
Claude Agent API with OAuth authentication support.
Uses subprocess to call Claude CLI for reliability.
"""
import asyncio
import json
import os
import subprocess
import re
from pathlib import Path

from fastapi import FastAPI, HTTPException
from fastapi.responses import StreamingResponse
from pydantic import BaseModel


app = FastAPI(title="Claude Agent API")

CREDENTIALS_PATH = Path.home() / ".claude" / ".credentials.json"


class ExecuteRequest(BaseModel):
    """Request to execute Claude Code."""
    prompt: str
    workspace: str = "/workspace"
    model: str = "haiku"


class ExecuteResponse(BaseModel):
    """Response from Claude Code execution."""
    output: str
    session_id: str | None = None
    error: str | None = None


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
        "service": "claude-agent-api",
        "authenticated": auth.authenticated,
    }


@app.get("/auth/status", response_model=AuthStatus)
async def auth_status():
    """Check authentication status."""
    return check_auth()


@app.post("/auth/login")
async def auth_login():
    """
    Initiate OAuth login flow.
    Returns a URL for the user to open in their browser.
    """
    try:
        # Start claude login process and capture its output
        # We use script to fake a TTY
        result = subprocess.run(
            ["script", "-q", "/dev/null", "claude", "login"],
            capture_output=True,
            text=True,
            timeout=10,
            env={**os.environ, "TERM": "dumb"},
        )

        output = result.stdout + result.stderr

        # Look for URL in output
        url_match = re.search(r'https://[^\s\]]+', output)
        if url_match:
            return {
                "status": "pending",
                "message": "Open this URL to authenticate",
                "url": url_match.group(0),
            }

        # If no URL found, return the raw output for debugging
        return {
            "status": "error",
            "message": "Could not extract auth URL",
            "output": output[:500],
        }

    except subprocess.TimeoutExpired:
        return {
            "status": "error",
            "message": "Login command timed out",
        }
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@app.post("/auth/token")
async def auth_set_token(token: str):
    """
    Set authentication token directly.
    Useful for setting an API key.
    """
    # For API key, set environment variable
    os.environ["ANTHROPIC_API_KEY"] = token

    return {"status": "ok", "message": "Token set"}


@app.post("/execute", response_model=ExecuteResponse)
async def execute_claude(request: ExecuteRequest):
    """
    Execute a prompt using Claude CLI.
    """
    try:
        # Run claude with -p flag for non-interactive output
        result = subprocess.run(
            [
                "claude",
                "-p",
                "--dangerously-skip-permissions",
                "--no-session-persistence",
                "--model", request.model,
                request.prompt,
            ],
            cwd=request.workspace,
            capture_output=True,
            text=True,
            timeout=300,  # 5 minute timeout
        )

        if result.returncode != 0 and "401" in result.stdout:
            return ExecuteResponse(
                output="",
                error="Authentication expired. Please re-authenticate via /auth/login",
            )

        return ExecuteResponse(
            output=result.stdout,
            error=result.stderr if result.returncode != 0 else None,
        )

    except subprocess.TimeoutExpired:
        raise HTTPException(status_code=504, detail="Claude execution timed out")
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@app.post("/restart")
async def restart():
    """Restart placeholder - no persistent state in subprocess mode."""
    return {"status": "ok"}


if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="0.0.0.0", port=8001)
