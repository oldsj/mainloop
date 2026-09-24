#!/usr/bin/env bash
# Start only the actor-local shim. Native CLIs run headlessly once per delivered turn.
set -eu

AGENT_UID=10001
AGENT_GID=10001
CURRENT_UID="$(id -u)"
export HOME=/home/agent
WORKSPACE_PATH="${WORKSPACE_PATH:-/work/repo}"
EXEC_SHIM_STATE_DIR="${EXEC_SHIM_STATE_DIR:-${WORKSPACE_PATH}/.mainloop/exec-shim}"
export WORKSPACE_PATH EXEC_SHIM_STATE_DIR

if [[ ${CURRENT_UID} -eq 0 && ${1-} != "--runtime-user" ]]; then
  if ! command -v setpriv >/dev/null 2>&1; then
    echo 'setpriv is required to run the actor shim without root' >&2
    exit 1
  fi
  EXEC_SHIM_STATE_DIR="$(realpath -m "${EXEC_SHIM_STATE_DIR}")"
  case "${EXEC_SHIM_STATE_DIR}" in
  /work/*) ;;
  *)
    echo 'EXEC_SHIM_STATE_DIR must be under /work' >&2
    exit 1
    ;;
  esac
  export EXEC_SHIM_STATE_DIR
  mkdir -p "${HOME}" /work "${EXEC_SHIM_STATE_DIR}"
  chown -R "${AGENT_UID}:${AGENT_GID}" "${HOME}" /work
  exec setpriv --reuid "${AGENT_UID}" --regid "${AGENT_GID}" --init-groups \
    --bounding-set=-all --no-new-privs -- "$0" --runtime-user
fi

if [[ ${1-} == "--runtime-user" ]]; then
  shift
fi
if [[ ${CURRENT_UID} -eq 0 ]]; then
  echo 'entrypoint refused to continue as UID 0' >&2
  exit 1
fi

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
