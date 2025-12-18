#!/bin/bash
set -e

echo "Starting Claude Code HTTP API server..."
echo "Claude CLI config mounted from host: /home/claude/.claude"
echo "Workspace: /workspace"
echo "Listening on port 8001"

# Run the HTTP API server
exec python3 /home/claude/server.py
