#!/usr/bin/env bash
# shellcheck disable=SC2312,SC2310,SC2311,SC2249  # pedantic optional checks; spike scripts
# Pod entrypoint: real Herdr headless server, with all state on the PVC.
# Seeds agent trust/onboarding state so agents start without a human at a dialog, and copies
# the read-only Codex auth Secret to a writable CODEX_HOME on the PVC. Never prints credentials.
set -eu
mkdir -p "${HOME}" "${WORKSPACE_PATH}" "${STANDIN_STATE_DIR}" "${CODEX_HOME}" "${HOME}/.claude"
[[ -d "${WORKSPACE_PATH}/.git" ]] || git -C "${WORKSPACE_PATH}" init -q

# Claude Code: onboarding done, workspace trusted, bypass-permissions warning accepted.
# CLAUDE_CODE_OAUTH_TOKEN comes from a Secret-backed env var (subscription token).
if [[ ! -s "${HOME}/.claude.json" ]]; then
  jq -n --arg p "${WORKSPACE_PATH}" '{
    hasCompletedOnboarding: true,
    numStartups: 1,
    theme: "dark",
    projects: {($p): {hasTrustDialogAccepted: true, hasCompletedProjectOnboarding: true, allowedTools: []}}
  }' >"${HOME}/.claude.json"
fi
[[ -s "${HOME}/.claude/settings.json" ]] || echo '{"skipDangerousModePermissionPrompt": true}' >"${HOME}/.claude/settings.json"

# Codex: copy auth from the read-only Secret once (Codex rewrites auth.json on refresh, so the
# writable copy on the PVC is authoritative afterwards); trust the workspace.
if [[ -f /etc/agent-secrets/codex/auth.json ]] && [[ ! -s "${CODEX_HOME}/auth.json" ]]; then
  install -m 600 /etc/agent-secrets/codex/auth.json "${CODEX_HOME}/auth.json"
fi
if [[ ! -s "${CODEX_HOME}/config.toml" ]]; then
  printf '[projects."%s"]\ntrust_level = "trusted"\n' "${WORKSPACE_PATH}" >"${CODEX_HOME}/config.toml"
fi
# Codex shows an "Approaching rate limits - switch model?" modal after a turn, which swallows the next
# prompt. Hide only that nudge (Codex's own "keep current model, never show again"); no model change.
grep -q '^\[notice\]' "${CODEX_HOME}/config.toml" || printf '\n[notice]\nhide_rate_limit_model_nudge = true\n' >>"${CODEX_HOME}/config.toml"

echo "herdr $(herdr --version) server starting (HOME=${HOME} session=${HERDR_SESSION})"
exec herdr --session "${HERDR_SESSION}" server
