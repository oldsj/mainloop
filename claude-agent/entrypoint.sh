#!/bin/bash
set -e

echo "Starting Claude Code HTTP API server..."
echo "Workspace: /workspace"
echo "Listening on port 8001"

# Credentials priority:
# 1. Already in volume (from: make setup-claude-creds)
# 2. CLAUDE_CREDENTIALS env var (from: k8s secret)
if [ -f /home/claude/.claude/.credentials.json ]; then
    echo "✓ Claude credentials found (volume)"
elif [ -n "$CLAUDE_CREDENTIALS" ]; then
    echo "Writing credentials from env var..."
    mkdir -p /home/claude/.claude
    echo "$CLAUDE_CREDENTIALS" > /home/claude/.claude/.credentials.json
    chmod 600 /home/claude/.claude/.credentials.json
    echo "✓ Claude credentials configured (env)"
else
    echo "⚠ No credentials found!"
    echo "  Local: run 'make setup-claude-creds'"
    echo "  K8s: run 'make setup-claude-creds-k8s'"
fi

# Run the HTTP API server
exec python3 /home/claude/server.py
