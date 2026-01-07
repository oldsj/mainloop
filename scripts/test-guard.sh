#!/usr/bin/env bash
# Lesson #8: Fail fast if tests are already running
# Prevents port conflicts and orphaned processes
set -euo pipefail

LOCKFILE="/tmp/mainloop-test.lock"

# Check if lockfile exists and process is still running
if [[ -f ${LOCKFILE} ]]; then
  PID=$(cat "${LOCKFILE}")
  if ps -p "${PID}" >/dev/null 2>&1; then
    echo "Error: Tests already running (PID ${PID})"
    echo "Run 'make test-stop' to stop the existing test session"
    exit 1
  else
    echo "Removing stale lockfile (PID ${PID} not running)"
    rm -f "${LOCKFILE}"
  fi
fi

# Create lockfile with our PID
echo $$ >"${LOCKFILE}"

# Cleanup on exit
trap 'rm -f ${LOCKFILE}' EXIT
