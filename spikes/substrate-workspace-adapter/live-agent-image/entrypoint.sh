#!/usr/bin/env bash
# Start only the actor-local shim. Native CLIs run headlessly once per delivered turn.
set -eu

node /usr/local/bin/prepare-native-agent-config.cjs
mkdir -p "${WORKSPACE_PATH}"
[[ -d "${WORKSPACE_PATH}/.git" ]] || git -C "${WORKSPACE_PATH}" init -q

# Credential-free marker retained for the bounded Gate 5 restore/suspend-resume probe.
[[ -f "${WORKSPACE_PATH}/gate5-counter" ]] || echo 0 >"${WORKSPACE_PATH}/gate5-counter"

node "${EXEC_SHIM}" &
SHIM_PID=$!
trap 'kill "${SHIM_PID}" 2>/dev/null || true' EXIT INT TERM

ready=0
for _ in $(seq 1 60); do
  if curl --fail --silent http://127.0.0.1:"${EXEC_SHIM_PORT:-8090}"/healthz >/dev/null; then
    ready=1
    break
  fi
  if ! kill -0 "${SHIM_PID}" 2>/dev/null; then
    wait "${SHIM_PID}" || true
    echo 'CONTROL_SERVICE_READINESS_FAILED: shim exited before healthz passed' >&2
    exit 1
  fi
  sleep 0.5
done
if [[ ${ready} -ne 1 ]]; then
  echo 'CONTROL_SERVICE_READINESS_TIMEOUT: shim/workspace not ready within 30s' >&2
  kill "${SHIM_PID}" 2>/dev/null || true
  wait "${SHIM_PID}" || true
  exit 1
fi

echo "CONTROL_SERVICE_READY workspace=${WORKSPACE_PATH} port=${EXEC_SHIM_PORT:-8090}"
wait "${SHIM_PID}"
