#!/usr/bin/env bash
# shellcheck disable=SC2312,SC2310,SC2311,SC2249  # pedantic optional checks; spike scripts
# Live-agent-gate actor entrypoint (gate 5 in .tasknotes/plan.md, bounded live proof): a real
# Herdr server plus real Claude Code / Codex CLIs. Substrate has no Kubernetes-Secret-equivalent
# volume/env mechanism for an actor (see docs/spikes/substrate-workspace-adapter.md,
# "Credential-injection gap"), so credentials are fetched over the network from a small in-cluster
# server, reachable only because this actor's narrow EgressPolicy allows exactly that server's
# ClusterIP -- the same CIDR-scoped access-control mechanism gate 4 (dev-service) proved actually
# enforces (a non-allowed destination gets a clean 403), reused here as the auth boundary rather
# than inventing a new one. Never echoed, never written to a template, never logged.
set -eu
mkdir -p "${HOME}" "${HOME}/.claude" "${CODEX_HOME}"

if [[ -n ${CRED_SERVER-} ]]; then
  curl -fsS "http://${CRED_SERVER}/claude-token" -o "${HOME}/.claude-oauth-token"
  chmod 600 "${HOME}/.claude-oauth-token"
  CLAUDE_CODE_OAUTH_TOKEN="$(tr -d ' \r\n' <"${HOME}/.claude-oauth-token")"
  export CLAUDE_CODE_OAUTH_TOKEN
  curl -fsS "http://${CRED_SERVER}/codex-auth.json" -o "${CODEX_HOME}/auth.json"
  chmod 600 "${CODEX_HOME}/auth.json"
fi

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

echo "herdr $(herdr --version) server starting (HOME=${HOME} session=${HERDR_SESSION})"
herdr --session "${HERDR_SESSION}" server &
HERDR_PID=$!

for _ in $(seq 1 60); do
  herdr --session "${HERDR_SESSION}" status server >/dev/null 2>&1 && break
  sleep 0.5
done

shell_ws=$(herdr --session "${HERDR_SESSION}" workspace create --label shell --cwd "${WORKSPACE_PATH}")
shell_pane=$(echo "${shell_ws}" | jq -r '.result.root_pane.pane_id')

EXEC_SHIM_PANE_ID="${shell_pane}" HERDR_SESSION="${HERDR_SESSION}" node "${EXEC_SHIM}" &

wait "${HERDR_PID}"
