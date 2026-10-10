#!/usr/bin/env bash
# Shared live-sync instance mainloop-dev on internal-01 (DevSpace profile `live`).
#
#   dev-live.sh dev      deploy and start syncing (Ctrl-C stops sync; pods keep running)
#   dev-live.sh deploy   deploy mainloop-dev without syncing
#   dev-live.sh agents   apply the *-dev kagent agents (shared kagent namespace)
#   dev-live.sh purge [--yes]  delete everything DevSpace deployed: namespace mainloop-dev
#                        and the *-dev kagent agents (asks for the namespace name unless --yes)
#   dev-live.sh status   show pods and the dev agents
set -euo pipefail

cd "$(dirname "$0")/.."
CONTEXT=${DEV_LIVE_CONTEXT:-admin@internal-01}
NAMESPACE=mainloop-dev
DEVSPACE=(devspace --kube-context "${CONTEXT}" --namespace "${NAMESPACE}" --profile live)

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
dev) exec "${DEVSPACE[@]}" dev ;;
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
  sed -n '2,10p' "$0" >&2
  exit 2
  ;;
esac
