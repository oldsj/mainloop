#!/usr/bin/env bash
# shellcheck disable=SC2312,SC2310,SC2311,SC2249  # pedantic optional checks; spike scripts
# Live-agent-gate actor entrypoint (gate 5 in .tasknotes/plan.md, bounded live proof): a real
# Herdr server plus real Claude Code / Codex CLIs. Substrate has no Kubernetes-Secret-equivalent
# volume/env mechanism for an actor (see docs/spikes/substrate-workspace-adapter.md,
# "Credential-injection gap").
#
# Credential-free by construction: this entrypoint never fetches a credential and never
# requires network access to reach a running state. The template controller uses the
# ActorTemplate's `/healthz` readiness check before accepting the golden actor. A boot path
# that depends on a credential fetch succeeding can fail golden creation when its relay is
# denied or unreachable, which is what happened. Credential delivery is deferred to a
# reviewed boundary and is never performed during golden-actor warmup or from this entrypoint.
set -eu
mkdir -p "${HOME}" "${HOME}/.claude" "${CODEX_HOME}"

# Claude Code: onboarding done, workspace trusted, bypass-permissions warning accepted.
if [[ ! -s "${HOME}/.claude.json" ]]; then
  jq -n --arg p "${WORKSPACE_PATH}" '{
    hasCompletedOnboarding: true,
    numStartups: 1,
    theme: "dark",
    projects: {($p): {hasTrustDialogAccepted: true, hasCompletedProjectOnboarding: true, allowedTools: []}}
  }' >"${HOME}/.claude.json"
fi
[[ -s "${HOME}/.claude/settings.json" ]] || echo '{"skipDangerousModePermissionPrompt": true}' >"${HOME}/.claude/settings.json"

# Codex: trust the workspace.
if [[ ! -s "${CODEX_HOME}/config.toml" ]]; then
  printf '[projects."%s"]\ntrust_level = "trusted"\n' "${WORKSPACE_PATH}" >"${CODEX_HOME}/config.toml"
fi
grep -q '^\[notice\]' "${CODEX_HOME}/config.toml" || printf '\n[notice]\nhide_rate_limit_model_nudge = true\n' >>"${CODEX_HOME}/config.toml"

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
