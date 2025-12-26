#!/usr/bin/env python3
"""
Persistent Claude Code CLI wrapper with streaming JSON.
Keeps Claude process alive for fast responses and session memory.
"""
import asyncio
import json
import subprocess
import threading
from collections.abc import AsyncGenerator
from queue import Queue, Empty

from fastapi import FastAPI, HTTPException
from fastapi.responses import StreamingResponse
from pydantic import BaseModel


app = FastAPI(title="Claude Code API")


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


class ClaudeSession:
    """Persistent Claude CLI session using stream-json mode."""

    def __init__(self, model: str = "haiku", workspace: str = "/workspace"):
        self.model = model
        self.workspace = workspace
        self.process: subprocess.Popen | None = None
        self.session_id: str | None = None
        self.response_queue: Queue = Queue()
        self.reader_thread: threading.Thread | None = None
        self.lock = threading.Lock()
        self._start_process()

    def _start_process(self):
        """Start the Claude CLI process."""
        self.process = subprocess.Popen(
            [
                'claude', '-p', '--verbose',
                '--input-format', 'stream-json',
                '--output-format', 'stream-json',
                '--dangerously-skip-permissions',
                '--model', self.model,
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            cwd=self.workspace,
            bufsize=1,  # Line buffered
        )

        # Start reader thread
        self.reader_thread = threading.Thread(target=self._read_output, daemon=True)
        self.reader_thread.start()

    def _read_output(self):
        """Read output from Claude process in background thread."""
        try:
            for line in self.process.stdout:
                line = line.strip()
                if line:
                    try:
                        msg = json.loads(line)
                        self.response_queue.put(msg)
                    except json.JSONDecodeError:
                        pass
        except Exception:
            pass

    def is_alive(self) -> bool:
        """Check if the Claude process is still running."""
        return self.process is not None and self.process.poll() is None

    def restart(self):
        """Restart the Claude process if it died."""
        with self.lock:
            if self.process:
                self.process.kill()
                self.process.wait()
            self._start_process()
            self.session_id = None

    def send_message(self, prompt: str) -> str:
        """Send a message and wait for the result."""
        with self.lock:
            if not self.is_alive():
                self.restart()

            # Clear any pending messages
            while not self.response_queue.empty():
                try:
                    self.response_queue.get_nowait()
                except Empty:
                    break

            # Send the message
            msg = {
                "type": "user",
                "message": {"role": "user", "content": prompt}
            }
            self.process.stdin.write(json.dumps(msg) + "\n")
            self.process.stdin.flush()

            # Wait for result
            result_text = ""
            timeout = 300  # 5 minutes

            while True:
                try:
                    response = self.response_queue.get(timeout=timeout)

                    # Capture session ID from init message
                    if response.get("type") == "system" and response.get("subtype") == "init":
                        self.session_id = response.get("session_id")

                    # Check for result (final message)
                    elif response.get("type") == "result":
                        result_text = response.get("result", "")
                        if response.get("is_error"):
                            raise Exception(result_text)
                        break

                except Empty:
                    raise TimeoutError("Claude response timed out")

            return result_text


# Global session instance
_session: ClaudeSession | None = None
_session_lock = threading.Lock()


def get_session(model: str = "haiku", workspace: str = "/workspace") -> ClaudeSession:
    """Get or create the global Claude session."""
    global _session
    with _session_lock:
        if _session is None or not _session.is_alive():
            _session = ClaudeSession(model=model, workspace=workspace)
        return _session


@app.get("/health")
async def health():
    """Health check endpoint."""
    session = get_session()
    return {
        "status": "ok",
        "service": "claude-code-api",
        "session_alive": session.is_alive(),
        "session_id": session.session_id,
    }


@app.post("/execute", response_model=ExecuteResponse)
async def execute_claude(request: ExecuteRequest):
    """
    Execute Claude Code CLI with the given prompt.
    Uses persistent session for fast responses.
    """
    try:
        session = get_session(model=request.model, workspace=request.workspace)

        # Run in thread pool to not block
        loop = asyncio.get_event_loop()
        result = await loop.run_in_executor(
            None,
            session.send_message,
            request.prompt
        )

        return ExecuteResponse(
            output=result,
            session_id=session.session_id,
        )

    except TimeoutError:
        raise HTTPException(status_code=504, detail="Claude execution timed out")
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@app.post("/restart")
async def restart_session():
    """Force restart the Claude session."""
    global _session
    with _session_lock:
        if _session:
            _session.restart()
    return {"status": "restarted"}


if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="0.0.0.0", port=8001)
