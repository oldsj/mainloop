#!/usr/bin/env bash
# shellcheck disable=SC2312,SC2310,SC2311,SC2249  # pedantic optional checks; spike scripts
# Preview-gate actor entrypoint. Starts a real Herdr server, a real Vite dev server in one pane,
# and a minimal generic exec shim (Node http server -> `herdr pane run`) in a second pane. The
# shim is a stand-in for a credentialed native agent's own Bash tool -- Substrate's pinned commit
# has no generic secret-injection mechanism (only SystemInfo actor-identity/trust-bundle volumes
# and an egress CredentialProvider), so a real Claude/Codex session cannot be started here yet.
# See docs/spikes/substrate-workspace-adapter.md.
set -eu
STATE_DIR=/work/.mainloop
mkdir -p "${HOME}" "${STATE_DIR}"

echo "herdr $(herdr --version) server starting (HOME=${HOME} session=${HERDR_SESSION})"
herdr --session "${HERDR_SESSION}" server &
HERDR_PID=$!

for _ in $(seq 1 60); do
  herdr --session "${HERDR_SESSION}" status server >/dev/null 2>&1 && break
  sleep 0.5
done

dev_ws=$(herdr --session "${HERDR_SESSION}" workspace create --label dev --cwd "${VITE_DIR}")
dev_pane=$(echo "${dev_ws}" | jq -r '.result.root_pane.pane_id')
shell_ws=$(herdr --session "${HERDR_SESSION}" workspace create --label shell --cwd "${VITE_DIR}")
shell_pane=$(echo "${shell_ws}" | jq -r '.result.root_pane.pane_id')
echo "${shell_pane}" >"${STATE_DIR}/shell-pane-id"

herdr --session "${HERDR_SESSION}" pane run "${dev_pane}" "npm run dev"

EXEC_SHIM_PANE_ID="${shell_pane}" HERDR_SESSION="${HERDR_SESSION}" node "${EXEC_SHIM}" &

wait "${HERDR_PID}"
