#!/usr/bin/env bash

CTX='kind-substrate-preview'
KC=/tmp/substrate-preview-kubeconfig
ACTOR_IMAGE=${ACTOR_IMAGE:-localhost:5001/live-agent-gate@sha256:eb819c5ae18829a18972fd7be9ec44e0f3dbef08e554f8e0dfc1d6240f962f5d}
ROUTER_PORT=${ROUTER_PORT:-18091}
STATE_ROOT=${LIVE_PROOF_STATE_DIR:-/tmp/mainloop-substrate-live-proof}
# Shared with the durable proof and its cleanup script.
# shellcheck disable=SC2034 # cleanup-lane-a.sh uses this shared manifest path.
BEFORE_MANIFEST=${STATE_ROOT}/durable-before.tsv
# shellcheck disable=SC2034 # cleanup-lane-a.sh uses this shared manifest path.
AFTER_MANIFEST=${STATE_ROOT}/durable-after.tsv
# Fork patched-next 0f9635ae worker build (rebased on upstream, keeps the durable-owner fix).
WORKER_IMAGE_DIGEST=sha256:9cff9f35f68bcad9f37ce3574f2e0a4638368475e4b49b4de606e38d9bbcba92
WORKER_IMAGE_REFERENCE=localhost:5001/ateom-gvisor@${WORKER_IMAGE_DIGEST}
LIVE_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -- "${LIVE_DIR}/../../.." && pwd)
BACKEND_DIR=${REPO_ROOT}/backend
MANIFEST=${REPO_ROOT}/spikes/substrate-workspace-adapter/k8s/actor-template.yaml.tmpl
HTTP_HELPER=${LIVE_DIR}/shim_request.py
ATE_CLI=${SUBSTRATE_SRC:-${HOME}/dev/substrate}/bin/kubectl-ate
PORT_FORWARD_PID=
PORT_FORWARD_LOG=

die() {
  printf 'ERROR: %s\n' "$*" >&2
  return 1
}

kubectl_ctx() {
  timeout 300s kubectl --context "${CTX}" --kubeconfig "${KC}" "$@"
}

preflight_provider_resources_absent() {
  local resource kind name get_result
  for resource in secret/claude-oauth service/credprovider deployment/round3-claude-provider \
    serviceaccount/round3-claude-provider networkpolicy/round3-claude-provider; do
    kind=${resource%%/*}
    name=${resource#*/}
    if get_result=$(timeout 300s kubectl --context "${CTX}" --kubeconfig "${KC}" \
      -n mainloop-control get "${kind}" "${name}" -o name 2>&1); then
      die "${resource} already exists in mainloop-control; refusing to replace shared provider state"
      return 1
    elif [[ ${get_result} =~ [Nn]ot[Ff]ound|not\ found|does\ not\ exist ]]; then
      :
    else
      printf '%s\n' "${get_result}" >&2
      die "could not establish whether ${resource} exists; refusing to write provider state"
      return 1
    fi
  done
}

ate_ctx() {
  timeout 120s "${ATE_CLI}" --context "${CTX}" --kubeconfig "${KC}" "$@"
}

require_worker_image() {
  if [[ ! ${WORKER_IMAGE-} =~ ^[^[:space:]]+@${WORKER_IMAGE_DIGEST}$ ]]; then
    die "set WORKER_IMAGE to the pinned fork WorkerPool build with the durable-owner fix, such as ${WORKER_IMAGE_REFERENCE}"
  fi
}

prepare_state_file() {
  local state_file=$1 atespace=$2 namespace_result atespace_result
  mkdir -p "${STATE_ROOT}"
  chmod 700 "${STATE_ROOT}"
  if namespace_result=$(kubectl_ctx get namespace "${atespace}" -o name 2>&1); then
    die "namespace ${atespace} already exists; refusing to adopt or overwrite lane resources"
  elif [[ ! ${namespace_result} =~ [Nn]ot[Ff]ound|not\ found|does\ not\ exist ]]; then
    printf '%s\n' "${namespace_result}" >&2
    die "could not establish whether namespace ${atespace} exists; refusing to continue"
  fi
  if atespace_result=$(ate_ctx get atespace "${atespace}" -o json 2>&1); then
    die "atespace ${atespace} already exists; refusing to adopt or overwrite lane resources"
  elif [[ ! ${atespace_result} =~ [Nn]ot[Ff]ound|not\ found|does\ not\ exist ]]; then
    printf '%s\n' "${atespace_result}" >&2
    die "could not establish whether atespace ${atespace} exists; refusing to continue"
  fi
  rm -f "${state_file}"
  umask 077
}

gate5_setup() {
  local atespace=$1 pool=$2 actor=$3 version=$4 state_file=$5
  local egress_host=$6
  require_worker_image
  (
    cd "${BACKEND_DIR}" || exit
    UV_CACHE_DIR=/tmp/uv-cache uv run --no-sync python scripts/gate5_setup.py \
      --context "${CTX}" \
      --kubeconfig "${KC}" \
      --ate-cli "${SUBSTRATE_SRC:-${HOME}/dev/substrate}/bin/kubectl-ate" \
      --worker-image "${WORKER_IMAGE}" \
      --atespace "${atespace}" \
      --worker-pool "${pool}" \
      --template-version "${version}" \
      --actor-name "${actor}" \
      --image "${ACTOR_IMAGE}" \
      --manifest "${MANIFEST}" \
      --state-file "${state_file}" \
      --egress-hostname "${egress_host}"
  )
}

port_forward_ready() {
  timeout 1 bash -c "exec 3<>/dev/tcp/127.0.0.1/${ROUTER_PORT}" >/dev/null 2>&1
}

start_router() {
  if port_forward_ready; then
    die "127.0.0.1:${ROUTER_PORT} is already in use"
  fi
  PORT_FORWARD_LOG=$(mktemp "${STATE_ROOT}/router-port-forward.XXXXXX.log")
  # Background kubectl itself, not the kubectl_ctx function: $! must be the
  # process stop_router kills, and the tunnel must outlive kubectl_ctx's timeout.
  kubectl --context "${CTX}" --kubeconfig "${KC}" -n ate-system port-forward \
    --address 127.0.0.1 service/atenet-router "${ROUTER_PORT}:8081" >"${PORT_FORWARD_LOG}" 2>&1 &
  PORT_FORWARD_PID=$!
  for _ in $(seq 1 80); do
    if port_forward_ready; then
      printf 'router tunnel ready on 127.0.0.1:%s\n' "${ROUTER_PORT}"
      return 0
    fi
    if ! kill -0 "${PORT_FORWARD_PID}" 2>/dev/null; then
      cat "${PORT_FORWARD_LOG}" >&2
      die 'router port-forward exited before becoming ready'
    fi
    sleep 0.25
  done
  cat "${PORT_FORWARD_LOG}" >&2
  die 'router port-forward did not become ready within 20 seconds'
}

stop_router() {
  if [[ -n ${PORT_FORWARD_PID-} ]] && kill -0 "${PORT_FORWARD_PID}" 2>/dev/null; then
    kill "${PORT_FORWARD_PID}" 2>/dev/null || true
    wait "${PORT_FORWARD_PID}" 2>/dev/null || true
  fi
  PORT_FORWARD_PID=
  if [[ -n ${PORT_FORWARD_LOG-} ]]; then
    rm -f "${PORT_FORWARD_LOG}"
    PORT_FORWARD_LOG=
  fi
}

shim_request() {
  local atespace=$1 actor=$2 state_file=$3 request=$4
  printf '%s\n' "${request}" | (
    cd "${BACKEND_DIR}" || exit
    LIVE_PROOF_ATESPACE=${atespace} \
      LIVE_PROOF_ACTOR=${actor} \
      LIVE_PROOF_ROUTER_PORT=${ROUTER_PORT} \
      LIVE_PROOF_STATE_FILE=${state_file} \
      UV_CACHE_DIR=/tmp/uv-cache uv run --no-sync python "${HTTP_HELPER}"
  )
}

run_actor_command() {
  local atespace=$1 actor=$2 state_file=$3 command=$4 timeout_ms=${5:-180000}
  local request response status run_id result deadline
  if ! request=$(jq -cn --arg command "${command}" --argjson timeout "${timeout_ms}" \
    '{method:"POST",path:"/run",authenticated:true,body:{command:$command,timeout_ms:$timeout}}'); then
    die 'could not encode actor /run request'
    return 1
  fi
  if ! response=$(shim_request "${atespace}" "${actor}" "${state_file}" "${request}"); then
    die 'actor /run request failed before returning a response'
    return 1
  fi
  status=$(jq -r '.status // 0' <<<"${response}")
  if [[ ${status} != 202 ]]; then
    die "actor /run was not accepted (HTTP ${status})"
    return 1
  fi
  run_id=$(jq -r '.body.id // empty' <<<"${response}")
  if [[ -z ${run_id} ]]; then
    die 'actor /run response omitted its id'
    return 1
  fi
  deadline=$((SECONDS + (timeout_ms / 1000) + 30))
  while ((SECONDS < deadline)); do
    if ! request=$(jq -cn --arg id "${run_id}" '{method:"GET",path:("/run/"+$id),authenticated:true}'); then
      die 'could not encode actor /run status request'
      return 1
    fi
    if ! response=$(shim_request "${atespace}" "${actor}" "${state_file}" "${request}"); then
      die 'actor /run status request failed before returning a response'
      return 1
    fi
    status=$(jq -r '.status // 0' <<<"${response}")
    if [[ ${status} != 200 ]]; then
      die "actor /run status fetch failed (HTTP ${status})"
      return 1
    fi
    result=$(jq -r '.body.status // empty' <<<"${response}")
    if [[ -z ${result} ]]; then
      die 'actor /run status response omitted its state'
      return 1
    fi
    case "${result}" in
    completed)
      exit_code=$(jq -r '.body.exit_code' <<<"${response}")
      if [[ ${exit_code} != 0 ]]; then
        die 'actor /run completed with a nonzero exit code'
        return 1
      fi
      jq -r '.body.output // ""' <<<"${response}"
      return 0
      ;;
    failed | timed_out | interrupted)
      exit_code=$(jq -r '.body.exit_code // "unknown"' <<<"${response}")
      die "actor /run ended with status=${result} exit_code=${exit_code}"
      return 1
      ;;
    *)
      # Nonterminal run states continue through the bounded polling loop.
      ;;
    esac
    sleep 1
  done
  die "actor /run ${run_id} exceeded its bounded wait"
  return 1
}

get_actor_json() {
  local atespace=$1 actor=$2
  ate_ctx get actor "${actor}" --atespace "${atespace}" -o json
}

wait_actor_state() {
  local atespace=$1 actor=$2 expected=$3 timeout_s=${4:-120} actor_json state
  local deadline=$((SECONDS + timeout_s))
  while ((SECONDS < deadline)); do
    if actor_json=$(get_actor_json "${atespace}" "${actor}" 2>/dev/null); then
      state=$(jq -r '.status.state // empty' <<<"${actor_json}")
      if [[ ${state} == "${expected}" ]]; then
        printf '%s\n' "${actor_json}"
        return 0
      fi
    fi
    sleep 1
  done
  die "actor ${atespace}/${actor} did not reach ${expected} within ${timeout_s}s"
}

actor_uid_from_state() {
  jq -r '.actor_uid // empty' "$1"
}

remove_shim_token_from_state() {
  local state_file=$1 tmp_file="$1.redacted"
  jq 'del(.shim_token)' "${state_file}" >"${tmp_file}"
  chmod 600 "${tmp_file}"
  mv -f "${tmp_file}" "${state_file}"
}
