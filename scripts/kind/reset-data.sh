#!/usr/bin/env bash
# Reset data between runs
#
# Usage:
#   ./reset-data.sh        # Reset test data only (preserves dev user data)
#   ./reset-data.sh --all  # Reset ALL data (dev + test)
#
# The API cleans up both database and K8s namespaces.
set -euo pipefail

API_URL="${API_URL:-http://localhost:8081}"
RESET_ALL="${1-}"

if [[ ${RESET_ALL} == "--all" ]]; then
  echo "=== Resetting ALL data (dev + test) ==="
  QUERY="?all=true"
else
  echo "=== Resetting test data only (preserving dev data) ==="
  QUERY=""
fi

echo "Calling API to reset data..."
if curl -sf "${API_URL}/health" >/dev/null 2>&1; then
  response=$(curl -sf -X POST "${API_URL}/internal/test/reset${QUERY}" 2>&1) || {
    echo "Warning: API reset failed - backend may not be in test mode"
    echo "Response: ${response:-none}"
    exit 1
  }
  echo "API response: ${response}"
else
  echo "Error: Backend not reachable at ${API_URL}"
  exit 1
fi

echo "=== Reset complete ==="
