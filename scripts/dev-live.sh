#!/usr/bin/env bash
# Shared live-sync instance mainloop-dev on internal-01 (DevSpace profile `live`).
#
#   dev-live.sh dev      deploy and start syncing (Ctrl-C stops sync; pods keep running).
#                        If MAINLOOP_API_URL is set it first acquires the devloop lease as the
#                        owner and refuses to start unless the lease is granted, keeps it alive
#                        while devspace runs (stopping devspace if it is lost), and releases it
#                        on exit.
#   dev-live.sh deploy   deploy mainloop-dev without syncing
#   dev-live.sh agents   apply the *-dev kagent agents (shared kagent namespace)
#   dev-live.sh purge [--yes]  delete everything DevSpace deployed: namespace mainloop-dev
#                        and the *-dev kagent agents (asks for the namespace name unless --yes)
#   dev-live.sh status   show pods and the dev agents
#   dev-live.sh lease-acquire  acquire the devloop lease as the owner (needs MAINLOOP_API_URL,
#                        the control-plane instance's own owner API base URL, e.g.
#                        https://mainloop.example.com/api: the lease lives in that instance's
#                        database, not in mainloop-dev, and this needs no kube context)
#   dev-live.sh lease-release  release it
set -euo pipefail

cd "$(dirname "$0")/.."
CONTEXT=${DEV_LIVE_CONTEXT:-admin@internal-01}
NAMESPACE=mainloop-dev
DEVSPACE=(devspace --kube-context "${CONTEXT}" --namespace "${NAMESPACE}" --profile live)

# lease calls the devloop lease REST API (served by the control-plane instance that runs the
# devloop tools, not by mainloop-dev itself): POST /devloop/lease/acquire or .../release.
lease() {
  : "${MAINLOOP_API_URL:?set MAINLOOP_API_URL to the control-plane instance owner API base URL first}"
  curl -fsS -X POST "${MAINLOOP_API_URL%/}/devloop/lease/$1"
  echo
}

# lease_acquired prints the lease reply and succeeds only when the owner now holds the lease. A
# queued reply is a refusal: someone else is syncing into mainloop-dev, and a queued caller must
# poll again within two minutes or lose its place.
lease_acquired() {
  local reply status
  # shellcheck disable=SC2310
  reply=$(lease acquire) || return 1
  echo "${reply}"
  status=$(jq -r '.status // empty' <<<"${reply}") || return 1
  [[ ${status} == acquired ]]
}

# lease_heartbeat renews the owner's lease every few minutes until it is killed, so a long
# session does not lose it to the idle timeout (20 minutes) while another agent waits. If the
# lease is lost it stops devspace (a child of the main script, $$), because syncing on would
# overwrite the new holder's code.
lease_heartbeat() {
  while sleep 240; do
    # shellcheck disable=SC2310
    if ! lease_acquired >/dev/null; then
      echo "devloop lease lost; stopping devspace so it does not overwrite the new holder's code" >&2
      pkill -TERM -x devspace -P "$$" || true
      return 0
    fi
  done
}

case "${1:-dev}" in
lease-acquire)
  lease acquire
  exit 0
  ;;
lease-release)
  lease release
  exit 0
  ;;
*) ;;
esac

# DevSpace renders manifests with a bare `kubectl create --dry-run=client`, which uses the
# kubeconfig's current context. Prepend a credential-free fragment that selects $CONTEXT,
# so the render never talks to whatever context happens to be current.
mkdir -p .devspace
printf 'apiVersion: v1\nkind: Config\ncurrent-context: %s\n' "${CONTEXT}" >.devspace/dev-live.kubeconfig
export KUBECONFIG="${PWD}/.devspace/dev-live.kubeconfig:${KUBECONFIG:-${HOME}/.kube/config}"

kubectl --context "${CONTEXT}" version --request-timeout=10s >/dev/null || {
  echo "kube context ${CONTEXT} is not reachable" >&2
  exit 1
}

case "${1:-dev}" in
dev)
  if [[ -n ${MAINLOOP_API_URL-} ]]; then
    # shellcheck disable=SC2310
    if ! lease_acquired; then
      echo "The mainloop-dev devloop lease is not granted (held by another session, or the lease API failed)." >&2
      echo "Syncing now would overwrite that session's code. Retry later, or unset MAINLOOP_API_URL to skip coordination." >&2
      lease release >/dev/null 2>&1 || true # leave the queue so a refused run does not hold a place
      exit 1
    fi
    lease_heartbeat &
    heartbeat=$!
    trap 'kill "${heartbeat}" 2>/dev/null || true; lease release || true' EXIT
  else
    echo "MAINLOOP_API_URL is not set; not coordinating the devloop lease" >&2
  fi
  "${DEVSPACE[@]}" dev
  ;;
deploy) exec "${DEVSPACE[@]}" deploy ;;
agents) exec "${DEVSPACE[@]}" run-pipeline deploy-agents ;;
purge)
  if [[ ${2-} != --yes ]]; then
    echo "This deletes namespace ${NAMESPACE} on ${CONTEXT}, including its database and volume." >&2
    read -r -p "Type ${NAMESPACE} to confirm: " answer
    [[ ${answer} == "${NAMESPACE}" ]] || {
      echo "Aborted." >&2
      exit 1
    }
  fi
  exec "${DEVSPACE[@]}" purge
  ;;
status)
  kubectl --context "${CONTEXT}" -n "${NAMESPACE}" get pods,cluster.postgresql.cnpg.io,httproute -o wide
  kubectl --context "${CONTEXT}" -n kagent get agents,agenttemplates,harnesses,remotemcpservers |
    grep -E "^NAME|-dev(-|\s|$)" || true
  ;;
*)
  sed -n '2,16p' "$0" >&2
  exit 2
  ;;
esac
