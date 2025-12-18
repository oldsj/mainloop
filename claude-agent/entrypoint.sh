#!/bin/bash
set -e

echo "Starting Claude Code HTTP API server..."
echo "Claude CLI config mounted from host: /home/claude/.claude"
echo "Workspace: /workspace"
echo "Listening on port 8001"

# Create credentials file if CLAUDE_CREDENTIALS is set
if [ -n "$CLAUDE_CREDENTIALS" ]; then
    echo "Setting up Claude credentials..."
    echo "$CLAUDE_CREDENTIALS" > /home/claude/.claude/.credentials.json
    chmod 600 /home/claude/.claude/.credentials.json
fi

# Run the HTTP API server
exec python3 /home/claude/server.py
