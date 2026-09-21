#!/usr/bin/env bash
# ShellCheck: these status checks and the single-quoted bash -c body are intentional.
# shellcheck disable=SC2016,SC2310,SC2312
# Run this worktree's frontend (Vite, hot reload) against the shared Kind backend.
#
#   scripts/dev-ui.sh up [--port N] [--clean]   start (idempotent) and print the URL
#   scripts/dev-ui.sh status                    every running dev UI plus the backend forward
#   scripts/dev-ui.sh logs                      follow this worktree's Vite log
#   scripts/dev-ui.sh down [--all]              stop this worktree's UI (--all: all UIs and the forward)
#
# Safe with several worktrees at once:
# - Each worktree gets its own stable Vite port (from its path; the next free one if taken).
# - All worktrees share the one in-cluster backend, so one port-forward serves them all. It is
#   started only if nothing already answers on the backend port, and `down` leaves it running for
#   the other worktrees (`down --all` stops it).
# - The browser only ever talks to its Vite port: Vite proxies /api to the backend, so a remote
#   browser needs just that one port forwarded (VS Code forwards it automatically).
# - kubectl is always given an explicit kubeconfig and context, never your current context.
#
# Environment: KIND_CLUSTER_NAME (mainloop-test), MAINLOOP_NAMESPACE (mainloop),
# MAINLOOP_DEV_BACKEND_PORT (8081), MAINLOOP_KUBECONFIG (use this kubeconfig instead of asking
# `kind`; needed when reading it requires privileges you do not have here).
set -euo pipefail

CLUSTER_NAME="${KIND_CLUSTER_NAME:-mainloop-test}"
CONTEXT="kind-${CLUSTER_NAME}"
NAMESPACE="${MAINLOOP_NAMESPACE:-mainloop}"
BACKEND_PORT="${MAINLOOP_DEV_BACKEND_PORT:-8081}"
PORT_BASE=5180
PORT_SPAN=100

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FRONTEND="${ROOT}/frontend"
STATE="${XDG_STATE_HOME:-${HOME}/.local/state}/mainloop-dev-ui"
mkdir -p "${STATE}"

path_hash="$(printf '%s' "${ROOT}" | cksum | cut -d' ' -f1)"
SLUG="$(basename "${ROOT}")-${path_hash}"
UI_PID="${STATE}/${SLUG}.pid"
UI_PORT="${STATE}/${SLUG}.port"
UI_LOG="${STATE}/${SLUG}.log"
FWD_PID="${STATE}/backend-forward.pid"
FWD_LOG="${STATE}/backend-forward.log"

die() {
  echo "dev-ui: $*" >&2
  exit 1
}
say() { echo "dev-ui: $*"; }

alive() { [[ -f $1 ]] && kill -0 "$(cat "$1")" 2>/dev/null; }
listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
backend_healthy() { curl -fsS -m 3 -o /dev/null "http://localhost:${BACKEND_PORT}/health" 2>/dev/null; }

stop_group() { # <pidfile>: stop the process and its children (it was started with setsid)
  local pid
  alive "$1" || {
    rm -f "$1"
    return 0
  }
  pid="$(cat "$1")"
  kill -- "-${pid}" 2>/dev/null || kill "${pid}" 2>/dev/null || true
  rm -f "$1"
}

wait_for() { # <seconds> <description> <command...>
  local secs="$1" what="$2"
  shift 2
  for ((i = 0; i < secs * 2; i++)); do
    "$@" && return 0
    sleep 0.5
  done
  die "timed out waiting for ${what}"
}

kubeconfig_path() {
  if [[ -n ${MAINLOOP_KUBECONFIG-} ]]; then
    echo "${MAINLOOP_KUBECONFIG}"
  else
    echo "${STATE}/kubeconfig-${CLUSTER_NAME}"
  fi
}

refresh_kubeconfig() { # Kind's API port changes when the cluster restarts, so re-read it each start
  [[ -n ${MAINLOOP_KUBECONFIG-} ]] && return 0
  local out
  if out="$(kind get kubeconfig --name "${CLUSTER_NAME}" 2>/dev/null)"; then
    :
  elif out="$(sudo -n "$(command -v kind)" get kubeconfig --name "${CLUSTER_NAME}" 2>/dev/null)"; then
    :
  else
    die "cannot read the kubeconfig for Kind cluster '${CLUSTER_NAME}' (is it running? if kind needs privileges, set MAINLOOP_KUBECONFIG)"
  fi
  (umask 077 && printf '%s\n' "${out}" >"$(kubeconfig_path)")
}

backend_up() {
  if backend_healthy; then
    say "backend already answering on :${BACKEND_PORT}"
    return 0
  fi
  # A forward that exists but is not answering is stale (cluster restarted): replace it.
  stop_group "${FWD_PID}"
  listening "${BACKEND_PORT}" && die "port ${BACKEND_PORT} is in use by something that is not the Mainloop backend (set MAINLOOP_DEV_BACKEND_PORT)"
  refresh_kubeconfig
  say "forwarding svc/mainloop-backend (${CONTEXT}/${NAMESPACE}) to :${BACKEND_PORT}"
  # kubectl port-forward exits when the backend pod is replaced; keep it coming back.
  setsid nohup bash -c '
    while true; do
      kubectl --kubeconfig "$1" --context "$2" -n "$3" port-forward --address 127.0.0.1 svc/mainloop-backend "$4:8000"
      sleep 1
    done' _ "$(kubeconfig_path)" "${CONTEXT}" "${NAMESPACE}" "${BACKEND_PORT}" >"${FWD_LOG}" 2>&1 &
  echo $! >"${FWD_PID}"
  wait_for 30 "the backend on :${BACKEND_PORT} (see ${FWD_LOG})" backend_healthy
}

claimed_by_other_ui() { # <port>: is it the port of another worktree's running UI?
  local f
  for f in "${STATE}"/*.port; do
    [[ -e ${f} && ${f} != "${UI_PORT}" ]] || continue
    [[ "$(cat "${f}")" == "$1" ]] && alive "${f%.port}.pid" && return 0
  done
  return 1
}

pick_port() {
  local start=$((PORT_BASE + path_hash % PORT_SPAN)) i p
  for ((i = 0; i < PORT_SPAN; i++)); do
    p=$((PORT_BASE + (start - PORT_BASE + i) % PORT_SPAN))
    listening "${p}" || claimed_by_other_ui "${p}" || {
      echo "${p}"
      return 0
    }
  done
  die "no free port in ${PORT_BASE}-$((PORT_BASE + PORT_SPAN - 1))"
}

ui_url() { echo "http://localhost:$(cat "${UI_PORT}")/"; }

ui_up() { # <port|""> <clean>
  local port="$1" clean="$2"
  [[ -x "${FRONTEND}/node_modules/.bin/vite" ]] || die "frontend dependencies missing: run 'pnpm install' in ${ROOT}"
  if alive "${UI_PID}"; then
    say "already running for this worktree: $(ui_url)"
    return 0
  fi
  rm -f "${UI_PID}"
  [[ -n ${port} ]] || port="$(pick_port)"
  listening "${port}" && die "port ${port} is already in use"
  [[ ${clean} == 1 ]] && rm -rf "${FRONTEND}/node_modules/.vite"
  echo "${port}" >"${UI_PORT}"
  (
    cd "${FRONTEND}"
    VITE_API_URL=/api MAINLOOP_API_PROXY="http://localhost:${BACKEND_PORT}" \
      setsid nohup node_modules/.bin/vite dev --host 127.0.0.1 --port "${port}" --strictPort >"${UI_LOG}" 2>&1 &
    echo $! >"${UI_PID}"
  )
  wait_for 40 "Vite on :${port} (see ${UI_LOG})" listening "${port}"
  say "ready: http://localhost:${port}/  (forward port ${port} to browse from another machine)"
}

cmd_status() {
  local f slug
  if backend_healthy; then say "backend: up on :${BACKEND_PORT}"; else say "backend: not answering on :${BACKEND_PORT}"; fi
  for f in "${STATE}"/*.pid; do
    [[ -e ${f} && ${f} != "${FWD_PID}" ]] || continue
    slug="$(basename "${f}" .pid)"
    if alive "${f}"; then
      say "ui: ${slug}  http://localhost:$(cat "${STATE}/${slug}.port")/  (pid $(cat "${f}"))$([[ ${slug} == "${SLUG}" ]] && echo '  <- this worktree')"
    fi
  done
}

cmd_down() {
  local f
  if [[ ${1-} == --all ]]; then
    for f in "${STATE}"/*.pid; do
      [[ -e ${f} && ${f} != "${FWD_PID}" ]] && stop_group "${f}"
    done
    stop_group "${FWD_PID}"
    say "stopped every dev UI and the backend forward"
    return 0
  fi
  stop_group "${UI_PID}"
  say "stopped this worktree's UI (the backend forward keeps running for other worktrees; 'down --all' stops it)"
}

main() {
  local cmd="${1:-up}" port="" clean=0
  [[ $# -gt 0 ]] && shift
  case "${cmd}" in
  up)
    while [[ $# -gt 0 ]]; do
      case "$1" in
      --port)
        port="${2:?--port needs a value}"
        shift 2
        ;;
      --clean)
        clean=1
        shift
        ;;
      *) die "unknown option: $1" ;;
      esac
    done
    # One at a time, so two worktrees starting together do not both start the forward.
    if command -v flock >/dev/null; then
      (
        flock 9
        backend_up
      ) 9>"${STATE}/lock"
    else
      backend_up
    fi
    ui_up "${port}" "${clean}"
    ;;
  status) cmd_status ;;
  logs)
    [[ -f ${UI_LOG} ]] || die "no log yet: run 'up' first"
    exec tail -n 50 -f "${UI_LOG}"
    ;;
  down) cmd_down "$@" ;;
  -h | --help | help) sed -n '2,20p' "$0" ;;
  *) die "unknown command: ${cmd} (up | status | logs | down)" ;;
  esac
}

main "$@"
