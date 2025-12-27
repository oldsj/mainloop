#!/bin/bash
set -e

echo "Starting Claude Code HTTP API server..."
echo "Workspace: /workspace"
echo "Listening on port 8001"

# Check for OAuth token (from setup-token, good for 1 year)
if [ -n "$CLAUDE_CODE_OAUTH_TOKEN" ]; then
    echo "✓ Using CLAUDE_CODE_OAUTH_TOKEN"
    export CLAUDE_CODE_OAUTH_TOKEN
# Check for API key
elif [ -n "$ANTHROPIC_API_KEY" ]; then
    echo "✓ Using ANTHROPIC_API_KEY"
# Fall back to OAuth credentials file
elif [ -f /home/claude/.claude/.credentials.json ]; then
    echo "✓ Using OAuth credentials (volume)"
else
    echo "⚠ No credentials found!"
    echo "  Set CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY env var"
fi

# Run the HTTP API server
exec python3 /home/claude/server.py
