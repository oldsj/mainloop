#!/usr/bin/env bash
# Start only the actor-local shim. Native CLIs run headlessly once per delivered turn.
set -eu

CURRENT_UID="$(id -u)"
export HOME="${HOME:-/work/.home}"
WORKSPACE_PATH="${WORKSPACE_PATH:-/work/repo}"
EXEC_SHIM_STATE_DIR="${EXEC_SHIM_STATE_DIR:-${WORKSPACE_PATH}/.mainloop/exec-shim}"
export WORKSPACE_PATH EXEC_SHIM_STATE_DIR

if [[ ${CURRENT_UID} -eq 0 ]]; then
  echo 'entrypoint refused to continue as UID 0' >&2
  exit 1
fi

if ! mkdir -p "${WORKSPACE_PATH}" || [[ ! -w ${WORKSPACE_PATH} ]]; then
  printf 'entrypoint requires a writable workspace directory for UID %s: %s\n' \
    "${CURRENT_UID}" "${WORKSPACE_PATH}" >&2
  exit 1
fi

node /usr/local/bin/prepare-native-agent-config.cjs
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
