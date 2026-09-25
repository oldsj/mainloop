#!/usr/bin/env bash
# Remove only the two uniquely named Lane A resources, after validating the
# gate5 state-file identity and any actor/template/pool UIDs still present.
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

lane=${1-}
case "${lane}" in
claude)
  ATESPACE="lane-a-claude-20260924"
  POOL="lane-a-claude-20260924"
  ACTOR=claude-live-proof
  VERSION="lane-a-claude-20260924"
  STATE_FILE=${STATE_ROOT}/claude-gate5-state.json
  ;;
durable)
  ATESPACE="lane-a-durable-20260924"
  POOL="lane-a-durable-20260924"
  ACTOR=repo-live-proof
  VERSION="lane-a-durable-20260924"
  STATE_FILE=${STATE_ROOT}/durable-gate5-state.json
  ;;
*)
  die 'usage: cleanup-lane-a.sh claude|durable'
  exit 2
  ;;
esac
TEMPLATE=live-agent-gate-${VERSION}

if [[ ! -f ${STATE_FILE} ]]; then
  die "refusing cleanup without the lane's gate5 state file: ${STATE_FILE}"
  exit 1
fi
state_identity=$(jq -r '[.context,.atespace,.worker_pool,.template_name,.actor_name] | @tsv' "${STATE_FILE}")
expected_identity=$(printf '%s\t%s\t%s\t%s\t%s' "${CTX}" "${ATESPACE}" "${POOL}" "${TEMPLATE}" "${ACTOR}")
[[ ${state_identity} == "${expected_identity}" ]] || {
  die 'gate5 state identity does not match the requested cleanup lane'
  exit 1
}

actor_err=$(mktemp "${STATE_ROOT}/cleanup-actor.XXXXXX")
template_err=$(mktemp "${STATE_ROOT}/cleanup-template.XXXXXX")
pool_err=$(mktemp "${STATE_ROOT}/cleanup-pool.XXXXXX")
provider_err=$(mktemp "${STATE_ROOT}/cleanup-provider.XXXXXX")
namespace_err=$(mktemp "${STATE_ROOT}/cleanup-namespace.XXXXXX")
declare -A provider_uids=()
cleanup_tmp() {
  rm -f "${actor_err}" "${template_err}" "${pool_err}" "${provider_err}" "${namespace_err}"
}
trap cleanup_tmp EXIT

recorded_namespace_uid=$(jq -r '.namespace_uid // empty' "${STATE_FILE}")
if [[ -z ${recorded_namespace_uid} ]]; then
  die "gate5 state has no Namespace UID; skipping namespace deletion for ${ATESPACE}"
  exit 1
fi
namespace_exists=0
# shellcheck disable=SC2310 # kubectl_ctx has one command; its status distinguishes NotFound from errors.
if namespace_json=$(kubectl_ctx get namespace "${ATESPACE}" -o json 2>"${namespace_err}"); then
  live_namespace_uid=$(jq -r '.metadata.uid // empty' <<<"${namespace_json}")
  if [[ -z ${live_namespace_uid} || ${live_namespace_uid} != "${recorded_namespace_uid}" ]]; then
    die "Namespace UID differs from gate5 state; skipping namespace deletion for ${ATESPACE}"
    exit 1
  fi
  namespace_exists=1
elif grep -Eiq 'not.?found|code = NotFound|NOT_FOUND|does not exist' "${namespace_err}"; then
  :
else
  cat "${namespace_err}" >&2
  die "could not verify Namespace UID; skipping namespace deletion for ${ATESPACE}"
  exit 1
fi

if [[ ${lane} == claude && ${SKIP_PROVIDER_CLEANUP:-0} != 1 ]]; then
  for resource in secret/claude-oauth service/credprovider deployment/round3-claude-provider serviceaccount/round3-claude-provider networkpolicy/round3-claude-provider; do
    kind=${resource%%/*}
    name=${resource#*/}
    if object_json=$(timeout 300s kubectl --context "${CTX}" --kubeconfig "${KC}" \
      -n mainloop-control get "${kind}" "${name}" -o json 2>"${provider_err}"); then
      label=$(jq -r '.metadata.labels["proof.mainloop.dev/lane"] // empty' <<<"${object_json}")
      [[ ${label} == lane-a-live-proof ]] || {
        die "${resource} exists without Lane A ownership label; leaving all provider resources intact"
        exit 1
      }
      uid=$(jq -r '.metadata.uid // empty' <<<"${object_json}")
      [[ -n ${uid} ]] || {
        die "${resource} has no readable UID; leaving all provider resources intact"
        exit 1
      }
      provider_uids["${resource}"]=${uid}
    elif ! grep -Eiq 'not.?found|code = NotFound|NOT_FOUND|does not exist' "${provider_err}"; then
      cat "${provider_err}" >&2
      die "could not reconcile ${resource}; no lane resources were deleted"
      exit 1
    fi
  done
fi

actor_json=
actor_exists=0
actor_state=
if actor_json=$(timeout 120s "${ATE_CLI}" --context "${CTX}" --kubeconfig "${KC}" \
  get actor "${ACTOR}" --atespace "${ATESPACE}" -o json 2>"${actor_err}"); then
  recorded_uid=$(actor_uid_from_state "${STATE_FILE}")
  actual_uid=$(jq -r '.metadata.uid // empty' <<<"${actor_json}")
  [[ -n ${recorded_uid} && ${actual_uid} == "${recorded_uid}" ]] || {
    die 'actor UID differs from gate5 state; no lane resources were changed'
    exit 1
  }
  actor_state=$(jq -r '.status.state // empty' <<<"${actor_json}")
  actor_exists=1
elif ! grep -Eiq 'not.?found|code = NotFound|NOT_FOUND|does not exist' "${actor_err}"; then
  cat "${actor_err}" >&2
  die 'could not reconcile actor ownership; no lane resources were changed'
  exit 1
fi

template_json=
template_exists=0
if template_json=$(timeout 120s "${ATE_CLI}" --context "${CTX}" --kubeconfig "${KC}" \
  get actor-template "${TEMPLATE}" --atespace "${ATESPACE}" -o json 2>"${template_err}"); then
  recorded_template_uid=$(jq -r '.template_uid // empty' "${STATE_FILE}")
  actual_template_uid=$(jq -r '.metadata.uid // empty' <<<"${template_json}")
  if [[ -z ${recorded_template_uid} || ${actual_template_uid} != "${recorded_template_uid}" ]]; then
    die 'ActorTemplate UID differs from gate5 state; no lane resources were changed'
    exit 1
  fi
  template_exists=1
elif ! grep -Eiq 'not.?found|code = NotFound|NOT_FOUND|does not exist' "${template_err}"; then
  cat "${template_err}" >&2
  die 'could not reconcile ActorTemplate ownership; no lane resources were changed'
  exit 1
fi

pool_exists=0
if pool_json=$(timeout 300s kubectl --context "${CTX}" --kubeconfig "${KC}" \
  -n "${ATESPACE}" get workerpool "${POOL}" -o json 2>"${pool_err}"); then
  pool_name=$(jq -r '.metadata.name // empty' <<<"${pool_json}")
  pool_label=$(jq -r '.metadata.labels.workload // empty' <<<"${pool_json}")
  [[ ${pool_name} == "${POOL}" && ${pool_label} == "${POOL}" ]] || {
    die 'WorkerPool labels differ from the lane identity; no lane resources were changed'
    exit 1
  }
  pool_exists=1
elif ! grep -Eiq 'not.?found|code = NotFound|NOT_FOUND|does not exist' "${pool_err}"; then
  cat "${pool_err}" >&2
  die 'could not reconcile WorkerPool ownership; no lane resources were changed'
  exit 1
fi

atespace_exists=0
if timeout 120s "${ATE_CLI}" --context "${CTX}" --kubeconfig "${KC}" \
  get atespace "${ATESPACE}" -o json 2>"${actor_err}" >/dev/null; then
  atespace_exists=1
elif ! grep -Eiq 'not.?found|code = NotFound|NOT_FOUND|does not exist' "${actor_err}"; then
  cat "${actor_err}" >&2
  die 'could not reconcile atespace; no lane resources were changed'
  exit 1
fi

if ((actor_exists == 1)); then
  if [[ ${actor_state} != ACTOR_STATE_SUSPENDED ]]; then
    ate_ctx suspend actor "${ACTOR}" --atespace "${ATESPACE}" >/dev/null
    wait_actor_state "${ATESPACE}" "${ACTOR}" ACTOR_STATE_SUSPENDED 180 >/dev/null
  fi
  ate_ctx delete actor "${ACTOR}" --atespace "${ATESPACE}" --any-state >/dev/null
fi

if [[ ${lane} == claude && ${SKIP_PROVIDER_CLEANUP:-0} != 1 ]]; then
  # Reconcile every provider UID again before deleting anything. DeleteOptions
  # below still fences the request against a replacement after this check.
  for resource in secret/claude-oauth service/credprovider deployment/round3-claude-provider serviceaccount/round3-claude-provider networkpolicy/round3-claude-provider; do
    kind=${resource%%/*}
    name=${resource#*/}
    # shellcheck disable=SC2310 # kubectl_ctx has one command; its status is checked below.
    if object_json=$(kubectl_ctx -n mainloop-control get "${kind}" "${name}" -o json 2>"${provider_err}"); then
      live_uid=$(jq -r '.metadata.uid // empty' <<<"${object_json}")
      live_label=$(jq -r '.metadata.labels["proof.mainloop.dev/lane"] // empty' <<<"${object_json}")
      recorded_uid=${provider_uids["${resource}"]-}
      if [[ -z ${live_uid} || -z ${recorded_uid} || ${live_uid} != "${recorded_uid}" || ${live_label} != lane-a-live-proof ]]; then
        die "provider cleanup skipped ${resource}: UID or ownership changed since verification"
        exit 1
      fi
    elif grep -Eiq 'not.?found|code = NotFound|NOT_FOUND|does not exist' "${provider_err}"; then
      if [[ -n ${provider_uids["${resource}"]-} ]]; then
        die "provider cleanup skipped ${resource}: its UID could not be read before deletion"
        exit 1
      fi
      continue
    else
      cat "${provider_err}" >&2
      die "provider cleanup skipped ${resource}: its UID could not be read before deletion"
      exit 1
    fi
  done

  for resource in networkpolicy/round3-claude-provider deployment/round3-claude-provider service/credprovider serviceaccount/round3-claude-provider secret/claude-oauth; do
    recorded_uid=${provider_uids["${resource}"]-}
    [[ -n ${recorded_uid} ]] || continue
    kind=${resource%%/*}
    name=${resource#*/}
    case ${kind} in
    networkpolicy)
      delete_path="/apis/networking.k8s.io/v1/namespaces/mainloop-control/networkpolicies/${name}"
      ;;
    deployment)
      delete_path="/apis/apps/v1/namespaces/mainloop-control/deployments/${name}"
      ;;
    service)
      delete_path="/api/v1/namespaces/mainloop-control/services/${name}"
      ;;
    serviceaccount)
      delete_path="/api/v1/namespaces/mainloop-control/serviceaccounts/${name}"
      ;;
    secret)
      delete_path="/api/v1/namespaces/mainloop-control/secrets/${name}"
      ;;
    *)
      die "unsupported provider resource kind ${kind}"
      exit 1
      ;;
    esac
    # shellcheck disable=SC2310 # kubectl_ctx has one command; delete failure is handled below.
    if ! kubectl_ctx delete --raw="${delete_path}" -f - <<JSON; then
{"apiVersion":"v1","kind":"DeleteOptions","preconditions":{"uid":"${recorded_uid}"}}
JSON
      die "could not delete owned provider resource ${resource} with its verified UID"
      exit 1
    fi
    # shellcheck disable=SC2310 # kubectl_ctx has one command; wait failure is handled below.
    if [[ ${kind} == deployment ]] && ! kubectl_ctx -n mainloop-control \
      wait --for=delete "deployment/${name}" --timeout=120s >/dev/null; then
      die "timed out waiting for provider Deployment ${name} deletion"
      exit 1
    fi
  done
fi

if ((template_exists == 1)); then
  ate_ctx delete actor-template "${TEMPLATE}" --atespace "${ATESPACE}" >/dev/null
fi
if ((pool_exists == 1)); then
  kubectl_ctx -n "${ATESPACE}" delete workerpool "${POOL}" --wait=true --timeout=120s >/dev/null
fi
if ((atespace_exists == 1)); then
  ate_ctx delete atespace "${ATESPACE}" >/dev/null
fi
if ((namespace_exists == 1)); then
  # shellcheck disable=SC2310 # kubectl_ctx has one command; recheck failure skips namespace deletion.
  if namespace_json=$(kubectl_ctx get namespace "${ATESPACE}" -o json 2>"${namespace_err}"); then
    live_namespace_uid=$(jq -r '.metadata.uid // empty' <<<"${namespace_json}")
  else
    cat "${namespace_err}" >&2
    die "could not recheck Namespace UID; skipping namespace deletion for ${ATESPACE}"
    exit 1
  fi
  if [[ -z ${live_namespace_uid} || ${live_namespace_uid} != "${recorded_namespace_uid}" ]]; then
    die "Namespace UID changed during cleanup; skipping namespace deletion for ${ATESPACE}"
    exit 1
  fi
  kubectl_ctx delete --raw="/api/v1/namespaces/${ATESPACE}" -f - <<JSON
{"apiVersion":"v1","kind":"DeleteOptions","preconditions":{"uid":"${recorded_namespace_uid}"}}
JSON
  kubectl_ctx wait --for=delete "namespace/${ATESPACE}" --timeout=180s >/dev/null
fi
rm -f "${STATE_FILE}" "${BEFORE_MANIFEST}" "${AFTER_MANIFEST}"
printf 'CLEANUP=PASS lane=%s namespace=%s provider_secret_and_service=removed_if_owned\n' "${lane}" "${ATESPACE}"
