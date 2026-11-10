#!/usr/bin/env bash
# Prove Substrate works on the Kind cluster: create an actor, call it through the router, write a
# file to its durable directory, suspend it, and check the file after it resumes.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
CLUSTER_NAME="${KIND_CLUSTER_NAME:-mainloop-local}"
ATE="${REPO_ROOT}/.cache/substrate/v1.0.1/kubectl-ate"
ATESPACE=substrate-demo
ACTOR="smoke-$(date +%s)"
PORT="${SMOKE_PORT:-18080}"

# Without --token-file the CLI acts as the shared ate-client identity and says so on every call.
ate() { "${ATE}" --context "kind-${CLUSTER_NAME}" "$@" 2> >(grep -v 'no --token-file given' >&2); }

if [[ ! -x ${ATE} ]]; then
  echo "ERROR: ${ATE} is missing; run scripts/kind/substrate/install.sh first" >&2
  exit 1
fi

ate get atespace "${ATESPACE}" >/dev/null 2>&1 || ate create atespace "${ATESPACE}"
ate get actor-template smoke -a "${ATESPACE}" >/dev/null 2>&1 ||
  ate create actor-template -f "${SCRIPT_DIR}/smoke-template.yaml"

proxy_pid=""
cleanup() {
  [[ -n ${proxy_pid} ]] && kill "${proxy_pid}" 2>/dev/null || true
  ate delete actor "${ACTOR}" -a "${ATESPACE}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# The template is usable once its golden snapshot exists, so creating the actor is retried.
for attempt in $(seq 1 60); do
  if ate create actor "${ACTOR}" -a "${ATESPACE}" --template smoke; then break; fi
  if [[ ${attempt} == 60 ]]; then
    echo "ERROR: could not create an actor from the smoke template" >&2
    exit 1
  fi
  sleep 5
done

# Started without the ate function: a function in the background runs in a subshell, and killing
# the subshell would leave the proxy running.
"${ATE}" --context "kind-${CLUSTER_NAME}" proxy actor "${ACTOR}" "${PORT}:80" -a "${ATESPACE}" &
proxy_pid=$!

call() { curl -fsS --retry 30 --retry-delay 2 --retry-all-errors --max-time 90 "$@"; }

call "http://localhost:${PORT}/hostname" >/dev/null
echo "=== actor answers through the router ==="
call --get --data-urlencode "cmd=sh -c 'echo kept > /data/marker; id -u; stat -c %a:%u /data'" \
  "http://localhost:${PORT}/shell"
echo

ate suspend actor "${ACTOR}" -a "${ATESPACE}"
ate get actor "${ACTOR}" -a "${ATESPACE}"

# A request to a suspended actor resumes it.
out="$(call --get --data-urlencode 'cmd=cat /data/marker' "http://localhost:${PORT}/shell")"
if [[ ${out} != *kept* ]]; then
  echo "ERROR: the durable directory did not survive suspend and resume: ${out}" >&2
  exit 1
fi
ate get actor "${ACTOR}" -a "${ATESPACE}"
echo "=== durable directory survived suspend and resume ==="
