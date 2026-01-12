#!/bin/bash
set -e

echo "Starting Mainloop Backend API..."

# Verify /repos is writable (fail fast if volume permissions are wrong)
if [[ -d /repos ]]; then
  if touch /repos/.write-test 2>/dev/null; then
    rm -f /repos/.write-test
    echo "✓ Repo cache directory writable"
  else
    echo "✗ ERROR: /repos is not writable by user $(whoami || true)"
    echo "  Fix with: docker exec -u root mainloop-backend chown -R claude:claude /repos"
    exit 1
  fi
fi

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

# Execute the main command
exec "$@"
