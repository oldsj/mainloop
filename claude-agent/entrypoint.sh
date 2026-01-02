#!/bin/bash
set -e

echo "Starting Claude Agent Worker API..."
echo "Workspace: /workspace"
echo "Listening on port 8001"

# Create Claude credentials file from env var if token is provided
# This is required for the Claude Agent SDK to authenticate
if [[ -n ${CLAUDE_CODE_OAUTH_TOKEN} ]]; then
  mkdir -p ~/.claude
  cat >~/.claude/.credentials.json <<EOF
{
  "claudeAiOauth": {
    "accessToken": "${CLAUDE_CODE_OAUTH_TOKEN}",
    "expiresAt": 9999999999999,
    "subscriptionType": "max"
  }
}
EOF
  chmod 600 ~/.claude/.credentials.json
  echo "✓ Claude credentials configured from CLAUDE_CODE_OAUTH_TOKEN"
elif [[ -n ${ANTHROPIC_API_KEY} ]]; then
  echo "✓ Using ANTHROPIC_API_KEY for Claude SDK"
elif [[ -f ~/.claude/.credentials.json ]]; then
  echo "✓ Using existing Claude credentials file"
else
  echo "⚠ No Claude credentials found!"
  echo "  Set CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY env var"
fi

# Configure GitHub CLI authentication
if [[ -n ${GH_TOKEN} ]]; then
  echo "✓ GitHub CLI configured with GH_TOKEN"
  git config --global user.name "Mainloop Bot"
  git config --global user.email "bot@mainloop.ai"
else
  echo "⚠ GH_TOKEN not set - GitHub operations will fail"
fi

# Run the HTTP API server
cd /app && exec uvicorn server:app --host 0.0.0.0 --port 8001
