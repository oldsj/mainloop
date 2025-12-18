#!/usr/bin/env python3
"""
Simple HTTP API wrapper for Claude Code CLI.
Exposes Claude Code functionality over HTTP for Docker networking.
"""
import asyncio
import json
import subprocess
import tempfile
from pathlib import Path

from fastapi import FastAPI, HTTPException
from fastapi.responses import StreamingResponse
from pydantic import BaseModel


app = FastAPI(title="Claude Code API")


class ExecuteRequest(BaseModel):
    """Request to execute Claude Code."""
    prompt: str
    workspace: str = "/workspace"


class ExecuteResponse(BaseModel):
    """Response from Claude Code execution."""
    output: str
    error: str | None = None


@app.get("/health")
async def health():
    """Health check endpoint."""
    return {"status": "ok", "service": "claude-code-api"}


@app.post("/execute", response_model=ExecuteResponse)
async def execute_claude(request: ExecuteRequest):
    """
    Execute Claude Code CLI with the given prompt.

    Runs claude in one-shot mode with permissions bypassed.
    """
    try:
        # Create temp file for prompt
        with tempfile.NamedTemporaryFile(mode='w', suffix='.txt', delete=False) as f:
            f.write(request.prompt)
            prompt_file = f.name

        try:
            # Run claude with permissions bypassed
            result = subprocess.run(
                [
                    'claude',
                    '--dangerously-skip-permissions',
                    '--cwd', request.workspace,
                    prompt_file
                ],
                capture_output=True,
                text=True,
                timeout=300  # 5 minute timeout
            )

            return ExecuteResponse(
                output=result.stdout,
                error=result.stderr if result.returncode != 0 else None
            )

        finally:
            # Clean up temp file
            Path(prompt_file).unlink(missing_ok=True)

    except subprocess.TimeoutExpired:
        raise HTTPException(status_code=504, detail="Claude execution timed out")
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="0.0.0.0", port=8001)
