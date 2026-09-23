#!/usr/bin/env bash
# shellcheck disable=SC2312,SC2310,SC2311,SC2249  # pedantic optional checks; spike scripts
# Live-agent-gate actor entrypoint (gate 5 in .tasknotes/plan.md, bounded live proof): a real
# Herdr server plus real Claude Code / Codex CLIs. Substrate has no Kubernetes-Secret-equivalent
# volume/env mechanism for an actor (see docs/spikes/substrate-workspace-adapter.md,
# "Credential-injection gap").
#
# Credential-free by construction: this entrypoint never fetches a credential and never
# requires network access to reach a running state. Mainloop installs a per-actor shim token
# and provider credentials only after the final actor is RUNNING. The golden actor stays clean.
# The template controller checks `/healthz` before accepting the golden actor.
set -eu
# Seed supported first-run defaults before either CLI is started. Native sessions start only
# through start-native-agent, after the final actor receives its credential.
node /usr/local/bin/prepare-native-agent-config.cjs

mkdir -p "${WORKSPACE_PATH}"
[[ -d "${WORKSPACE_PATH}/.git" ]] || git -C "${WORKSPACE_PATH}" init -q
# Credential-free identity/counter marker for the fake-payload proof (recovery plan step 2):
# a plain file the exec shim can read/increment to verify golden restore and suspend/resume
# without any real agent session or credential.
[[ -f "${WORKSPACE_PATH}/gate5-counter" ]] || echo 0 >"${WORKSPACE_PATH}/gate5-counter"

echo "herdr $(herdr --version) starting (HOME=${HOME} session=${HERDR_SESSION})"
herdr --session "${HERDR_SESSION}" server &
HERDR_PID=$!

# The persistent control service's readiness check: an explicit, confirmed status call,
# not a fixed sleep or a pre-confirmation log line. The ActorTemplate's `/healthz` probe
# checks the Herdr server and this shell pane before the controller captures its snapshot.
ready=0
for _ in $(seq 1 60); do
  if herdr --session "${HERDR_SESSION}" status server >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.5
done
if [[ ${ready} -ne 1 ]]; then
  echo "CONTROL_SERVICE_READINESS_TIMEOUT: herdr server did not report ready within 30s" >&2
  kill "${HERDR_PID}" 2>/dev/null || true
  exit 1
fi
shell_ws=$(herdr --session "${HERDR_SESSION}" workspace create --label shell --cwd "${WORKSPACE_PATH}")
shell_pane=$(echo "${shell_ws}" | jq -r '.result.root_pane.pane_id')

EXEC_SHIM_PANE_ID="${shell_pane}" HERDR_SESSION="${HERDR_SESSION}" node "${EXEC_SHIM}" &
echo "CONTROL_SERVICE_READY session=${HERDR_SESSION} pane=${shell_pane}"

wait "${HERDR_PID}"
