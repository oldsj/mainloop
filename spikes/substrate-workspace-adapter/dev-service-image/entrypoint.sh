#!/usr/bin/env bash
# shellcheck disable=SC2312,SC2310,SC2311,SC2249  # pedantic optional checks; spike scripts
# Dev-service-gate actor entrypoint (gate 4 in .tasknotes/plan.md): a real Herdr server plus a
# generic exec shim, no dev server -- just enough to run a real psql client against an external
# Postgres Service from inside the actor. See docs/spikes/substrate-workspace-adapter.md.
set -eu
mkdir -p "${HOME}"

echo "herdr $(herdr --version) server starting (HOME=${HOME} session=${HERDR_SESSION})"
herdr --session "${HERDR_SESSION}" server &
HERDR_PID=$!

for _ in $(seq 1 60); do
  herdr --session "${HERDR_SESSION}" status server >/dev/null 2>&1 && break
  sleep 0.5
done

shell_ws=$(herdr --session "${HERDR_SESSION}" workspace create --label shell --cwd /work)
shell_pane=$(echo "${shell_ws}" | jq -r '.result.root_pane.pane_id')

EXEC_SHIM_PANE_ID="${shell_pane}" HERDR_SESSION="${HERDR_SESSION}" node "${EXEC_SHIM}" &

wait "${HERDR_PID}"
