#!/usr/bin/env bash
# Bounded live Claude continuity proof on the product template. This script
# uses a prebuilt provider image and never builds or pushes images.
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

ATESPACE=lane-a-claude-20260924
POOL=lane-a-claude-20260924
ACTOR=claude-live-proof
STATE_FILE=${STATE_ROOT}/claude-gate5-state.json
CLAUDE_TOKEN_FILE=${CLAUDE_TOKEN_FILE:-${HOME}/.claude-token}
CLAUDE_SECRET_URI=ate-secret://kubernetes.io/mainloop-control/claude-oauth/oauth-token
PROVIDER_IMAGE=${CLAUDE_PROVIDER_IMAGE-}
PROVIDER_READY=0
LANE_STARTED=0
COMPLETE=0
declare -A PROVIDER_UIDS=()

record_provider_uid() {
  local resource=$1 object_json=$2 uid
  PROVIDER_READY=1
  if ! uid=$(jq -er '.metadata.uid | strings | select(length > 0)' <<<"${object_json}"); then
    die "created provider resource ${resource} response omitted metadata.uid"
    return 1
  fi
  PROVIDER_UIDS["${resource}"]=${uid}
}

cleanup_provider() {
  ((PROVIDER_READY == 1)) || return 0
  local resource kind name recorded_uid live_uid
  for resource in networkpolicy/round3-claude-provider deployment/round3-claude-provider \
    service/credprovider serviceaccount/round3-claude-provider secret/claude-oauth; do
    recorded_uid=${PROVIDER_UIDS["${resource}"]-}
    [[ -n ${recorded_uid} ]] || continue
    kind=${resource%%/*}
    name=${resource#*/}
    # shellcheck disable=SC2310 # kubectl_ctx has one command; inspect its status below.
    if live_uid=$(kubectl_ctx -n mainloop-control get "${kind}" "${name}" \
      -o jsonpath='{.metadata.uid}' 2>&1); then
      if [[ ${live_uid} != "${recorded_uid}" ]]; then
        printf 'provider cleanup skipped %s: UID changed from %s to %s\n' \
          "${resource}" "${recorded_uid}" "${live_uid:-missing}" >&2
        continue
      fi
      local delete_path
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
        return 1
        ;;
      esac
      if ! kubectl_ctx delete --raw="${delete_path}" -f - <<JSON; then
{"apiVersion":"v1","kind":"DeleteOptions","preconditions":{"uid":"${recorded_uid}"}}
JSON
        die "could not delete owned provider resource ${resource}"
        return 1
      fi
      if [[ ${kind} == deployment ]] && ! kubectl_ctx -n mainloop-control \
        wait --for=delete "deployment/${name}" --timeout=120s >/dev/null; then
        die "timed out waiting for provider Deployment ${name} deletion"
        return 1
      fi
    elif [[ ${live_uid} =~ [Nn]ot[Ff]ound|not\ found|does\ not\ exist ]]; then
      continue
    else
      printf '%s\n' "${live_uid}" >&2
      die "could not verify provider resource ${resource} before cleanup"
      return 1
    fi
  done
  PROVIDER_READY=0
}

finish() {
  local status=$?
  stop_router
  if ((status != 0 && COMPLETE == 0 && LANE_STARTED == 1)); then
    printf 'run failed; attempting cleanup of the owned Claude lane\n' >&2
    # shellcheck disable=SC2310 # cleanup_provider explicitly checks and returns each delete failure.
    cleanup_provider || printf 'provider cleanup did not complete; inspect provider resources\n' >&2
    SKIP_PROVIDER_CLEANUP=1 "${SCRIPT_DIR}/cleanup-lane-a.sh" claude ||
      printf 'cleanup did not complete; inspect %s\n' "${STATE_FILE}" >&2
  fi
  return "${status}"
}
trap finish EXIT INT TERM

require_worker_image
if [[ ! -r ${CLAUDE_TOKEN_FILE} || ! -s ${CLAUDE_TOKEN_FILE} ]]; then
  die "Claude credential file is missing or empty: ${CLAUDE_TOKEN_FILE}"
fi
if [[ -z ${PROVIDER_IMAGE} ]]; then
  die 'set CLAUDE_PROVIDER_IMAGE to an existing digest-pinned provider image built from the pinned fork commit; this script does not build or push provider images'
fi
if [[ ! ${PROVIDER_IMAGE} =~ @sha256:[0-9a-fA-F]{64}$ ]]; then
  die 'CLAUDE_PROVIDER_IMAGE must be an existing reference pinned by a full sha256 digest'
fi
prepare_state_file "${STATE_FILE}" "${ATESPACE}"
LANE_STARTED=1
if timeout 300s kubectl --context "${CTX}" --kubeconfig "${KC}" \
  get workerpool "${POOL}" -n "${ATESPACE}" -o name >/dev/null 2>&1; then
  die "WorkerPool ${ATESPACE}/${POOL} already exists; refusing to adopt it"
fi

printf 'creating the Claude actor from the product template\n'
gate5_setup "${ATESPACE}" "${POOL}" "${ACTOR}" lane-a-claude-20260924 "${STATE_FILE}" api.anthropic.com
start_router

uid=$(run_actor_command "${ATESPACE}" "${ACTOR}" "${STATE_FILE}" 'id -u' 30000)
if [[ ${uid} != 10001 ]]; then
  die "actor runtime uid is ${uid}; refusing to deliver credentials or submit a Claude turn"
fi
printf 'actor runtime uid=%s\n' "${uid}"

preflight_provider_resources_absent

# The Secret receives only a path argument. Its generated manifest is piped directly
# to kubectl apply and is never printed or written to disk.
created_secret=$(kubectl_ctx -n mainloop-control create secret generic claude-oauth \
  --from-file="oauth-token=${CLAUDE_TOKEN_FILE}" \
  --dry-run=client -o json |
  jq -c '.metadata.labels["proof.mainloop.dev/lane"] = "lane-a-live-proof"' |
  kubectl_ctx create -f - -o json)
record_provider_uid secret/claude-oauth "${created_secret}"
unset created_secret

created_service_account=$(
  kubectl_ctx -n mainloop-control create -f - -o json <<'YAML'
apiVersion: v1
kind: ServiceAccount
metadata:
  name: round3-claude-provider
  namespace: mainloop-control
  labels:
    proof.mainloop.dev/lane: lane-a-live-proof
YAML
)
record_provider_uid serviceaccount/round3-claude-provider "${created_service_account}"

created_service=$(
  kubectl_ctx -n mainloop-control create -f - -o json <<'YAML'
apiVersion: v1
kind: Service
metadata:
  name: credprovider
  namespace: mainloop-control
  labels:
    proof.mainloop.dev/lane: lane-a-live-proof
spec:
  selector:
    app: round3-claude-provider
  ports:
  - name: grpc
    port: 50051
    targetPort: 50051
YAML
)
record_provider_uid service/credprovider "${created_service}"

created_deployment=$(
  kubectl_ctx -n mainloop-control create -f - -o json <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: round3-claude-provider
  namespace: mainloop-control
  labels:
    app: round3-claude-provider
    proof.mainloop.dev/lane: lane-a-live-proof
spec:
  replicas: 1
  selector:
    matchLabels:
      app: round3-claude-provider
  template:
    metadata:
      labels:
        app: round3-claude-provider
        proof.mainloop.dev/lane: lane-a-live-proof
    spec:
      serviceAccountName: round3-claude-provider
      containers:
      - name: provider
        image: ${PROVIDER_IMAGE}
        env:
        - name: EXPECTED_ACTOR_SPIFFE_ID
          value: "spiffe://substrate-actor.local/atespace/lane-a-claude-20260924/actor/claude-live-proof"
        ports:
        - name: grpc
          containerPort: 50051
        volumeMounts:
        - { name: claude-oauth, mountPath: /run/claude, readOnly: true }
        - { name: servicedns, mountPath: /run/servicedns, readOnly: true }
        - { name: podidentity-ca, mountPath: /run/podidentity-ca, readOnly: true }
      volumes:
      - name: claude-oauth
        secret:
          secretName: claude-oauth
          items:
          - { key: oauth-token, path: oauth-token }
      - name: servicedns
        projected:
          sources:
          - podCertificate:
              signerName: servicedns.podcert.ate.dev/identity
              keyType: ECDSAP256
              credentialBundlePath: credential-bundle.pem
      - name: podidentity-ca
        projected:
          sources:
          - clusterTrustBundle:
              signerName: podidentity.podcert.ate.dev/identity
              labelSelector:
                matchLabels: { podcert.ate.dev/canarying: live }
              path: trust-bundle.pem
YAML
)
record_provider_uid deployment/round3-claude-provider "${created_deployment}"

created_network_policy=$(
  kubectl_ctx -n mainloop-control create -f - -o json <<'YAML'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: round3-claude-provider
  namespace: mainloop-control
  labels:
    proof.mainloop.dev/lane: lane-a-live-proof
spec:
  podSelector:
    matchLabels: { app: round3-claude-provider }
  policyTypes: [Ingress]
  ingress:
  - from:
    - namespaceSelector:
        matchLabels: { kubernetes.io/metadata.name: ate-system }
      podSelector:
        matchLabels: { app: atenet-egress }
    ports:
    - { protocol: TCP, port: 50051 }
YAML
)
record_provider_uid networkpolicy/round3-claude-provider "${created_network_policy}"

kubectl_ctx -n mainloop-control rollout status deployment/round3-claude-provider --timeout=180s

# Egress-policy updates must carry the current metadata.uid and metadata.version preconditions.
update_egress_policy() {
  local rules=$1 current preconditions
  if ! current=$(timeout 120s "${ATE_CLI}" --context "${CTX}" --kubeconfig "${KC}" \
    get egress-policy "${ACTOR}" --atespace "${ATESPACE}" -o json); then
    die 'could not read the egress policy before updating it'
  fi
  preconditions=$(jq -ce '.metadata | select((.uid | type == "string" and length > 0)
    and ((.version | tostring | test("^[1-9][0-9]*$")))) | {uid, version: (.version | tostring)}' \
    <<<"${current}") || die 'egress policy lacks metadata.uid and metadata.version preconditions'
  jq -c --argjson metadata "${preconditions}" '. + {metadata: $metadata}' <<<"${rules}" |
    ate_ctx update egress-policy "${ACTOR}" --atespace "${ATESPACE}" --filename - >/dev/null
}

egress_policy=$(jq -cn --arg uri "${CLAUDE_SECRET_URI}" \
  '{rules:[{hostnames:{patterns:["api.anthropic.com"],effects:{injectStaticHeaders:[{header:"Authorization",prefix:"Bearer ",credentialUri:$uri}]}}}]}')
update_egress_policy "${egress_policy}"

# This exact placeholder is allowlisted by the shim and carries no credential.
placeholder_request='{"method":"POST","path":"/credential","authenticated":true,"body":{"name":"claude-token","contents":"sk-ant-oat01-mainloop-egress-placeholder"}}'
response=$(shim_request "${ATESPACE}" "${ACTOR}" "${STATE_FILE}" "${placeholder_request}")
response_status=$(jq -r '.status // 0' <<<"${response}")
[[ ${response_status} == 201 ]] || die 'placeholder Claude credential was not installed through /credential'
ready=$(shim_request "${ATESPACE}" "${ACTOR}" "${STATE_FILE}" \
  '{"method":"GET","path":"/agent/ready?agent=claude","authenticated":true}')
ready_status=$(jq -r '.status // 0' <<<"${ready}")
[[ ${ready_status} == 200 ]] || die 'Claude credential readiness did not become healthy'
printf 'actor credential is the fixed placeholder; real OAuth remains in the control Secret\n'

provider_probe_command="curl --silent --show-error --head --max-time 30 --output /dev/null --write-out '%{http_code}' --header 'Authorization: Bearer sk-ant-oat01-mainloop-egress-placeholder' https://api.anthropic.com/api/hello"
# The non-model probe is safe to repeat; retry only while the egress path to a
# freshly rolled-out provider is still settling (503, or no response).
for _ in $(seq 1 12); do
  provider_probe=$(run_actor_command "${ATESPACE}" "${ACTOR}" "${STATE_FILE}" "${provider_probe_command}" 45000)
  [[ ${provider_probe} == 503 || ${provider_probe} == 000 ]] || break
  sleep 5
done
[[ ${provider_probe} == 200 ]] || die "credential provider preflight returned HTTP ${provider_probe}"
provider_logs=$(kubectl_ctx -n mainloop-control logs deployment/round3-claude-provider --since=5m)
provider_fetches=$(grep -c 'credential_fetch=ok actor_identity_match=true' <<<"${provider_logs}" || true)
((provider_fetches > 0)) || die 'provider image did not accept the lane actor identity during the non-model preflight'
printf 'provider_preflight=http-200 matching_fetches=%s\n' "${provider_fetches}"

nonce_timestamp=$(date +%s%N)
nonce_seed=$(printf '%s-%s-%s' "$$" "${RANDOM}" "${nonce_timestamp}")
nonce_hash=$(printf '%s' "${nonce_seed}" | sha256sum)
N1=${nonce_hash:0:24}
SESSION_ID=$(cat /proc/sys/kernel/random/uuid)
SESSION_KEY=lane-a-claude-liveproof
prompt=$(printf 'Remember this nonce exactly: %s. In /work/repo, create lane-a-claude-marker.txt containing exactly the nonce followed by one newline. Reply with only the nonce after both are done.' "${N1}")
turn_request=$(jq -cn --arg prompt "${prompt}" --arg session "${SESSION_ID}" --arg key "${SESSION_KEY}" \
  '{method:"POST",path:"/turn",authenticated:true,body:{agent:"claude",session_id:$session,session_key:$key,resume:false,timeout_ms:600000,prompt:$prompt}}')
first=$(shim_request "${ATESPACE}" "${ACTOR}" "${STATE_FILE}" "${turn_request}")
first_status=$(jq -r '.status // 0' <<<"${first}")
[[ ${first_status} == 202 ]] || die "first Claude turn was not accepted (HTTP ${first_status})"
TURN_ID=$(jq -r '.body.id // empty' <<<"${first}")
[[ -n ${TURN_ID} ]] || die 'first Claude turn response omitted its ID'

concurrent_request=$(jq -cn --arg prompt 'This concurrent turn must be rejected; do not execute it.' \
  --arg session "${SESSION_ID}" --arg key "${SESSION_KEY}" \
  '{method:"POST",path:"/turn",authenticated:true,body:{agent:"claude",session_id:$session,session_key:$key,resume:true,timeout_ms:600000,prompt:$prompt}}')
concurrent=$(shim_request "${ATESPACE}" "${ACTOR}" "${STATE_FILE}" "${concurrent_request}")
concurrent_status=$(jq -r '.status // 0' <<<"${concurrent}")
if [[ ${concurrent_status} != 409 ]]; then
  if [[ ${concurrent_status} == 202 ]]; then
    stop_request=$(jq -cn --arg key "${SESSION_KEY}" '{method:"POST",path:"/turn/stop",authenticated:true,body:{agent:"claude",session_key:$key}}')
    # shellcheck disable=SC2310 # This stop request is best effort; its failure is intentionally ignored.
    shim_request "${ATESPACE}" "${ACTOR}" "${STATE_FILE}" "${stop_request}" >/dev/null || true
  fi
  die "concurrent Claude /turn returned HTTP ${concurrent_status}, expected 409; accepted turns are not replayed"
fi
printf 'concurrent_second_turn=HTTP-409\n'

turn_deadline=$((SECONDS + 610))
turn_json=
while ((SECONDS < turn_deadline)); do
  poll_request=$(jq -cn --arg id "${TURN_ID}" '{method:"GET",path:("/turn/"+$id),authenticated:true}')
  turn_response=$(shim_request "${ATESPACE}" "${ACTOR}" "${STATE_FILE}" "${poll_request}")
  turn_http_status=$(jq -r '.status // 0' <<<"${turn_response}")
  [[ ${turn_http_status} == 200 ]] || die 'could not read first Claude turn status'
  turn_state=$(jq -r '.body.status // empty' <<<"${turn_response}")
  case "${turn_state}" in
  completed | failed | timed_out | interrupted)
    turn_json=$(jq -c '.body' <<<"${turn_response}")
    break
    ;;
  *)
    # Nonterminal turn states continue through the bounded polling loop.
    ;;
  esac
  sleep 2
done
[[ -n ${turn_json} ]] || die 'first Claude turn exceeded its 10-minute bound'
turn_status=$(jq -r '.status' <<<"${turn_json}")
turn_exit=$(jq -r '.exit_code // "unknown"' <<<"${turn_json}")
if [[ ${turn_status} != completed || ${turn_exit} != 0 ]]; then
  # The actor holds only the placeholder credential, so its diagnostics are safe to print.
  jq '{credential_rejected, final_message, stderr_tail: ((.stderr // "")[-2000:]), last_events: ((.events // [])[-5:])}' \
    <<<"${turn_json}" >&2
  die "first Claude turn ended status=${turn_status} exit_code=${turn_exit}; no prompt retry was sent"
fi
native_session=$(jq -r '.native_session_id // empty' <<<"${turn_json}")
final_message=$(jq -r '.final_message // empty' <<<"${turn_json}")
[[ ${native_session} == "${SESSION_ID}" ]] || die 'Claude returned a different native session ID'
[[ ${final_message} == *"${N1}"* ]] || die 'Claude first-turn response did not contain the requested nonce'
printf 'first_turn=completed exit=0 session_id=%s nonce_match=yes\n' "${native_session}"

marker_command="test \"\$(cat /work/repo/lane-a-claude-marker.txt)\" = '${N1}' && sha256sum /work/repo/lane-a-claude-marker.txt | cut -d ' ' -f 1"
marker_hash=$(run_actor_command "${ATESPACE}" "${ACTOR}" "${STATE_FILE}" "${marker_command}" 30000)
printf 'marker_hash_before_suspend=%s\n' "${marker_hash}"

started_ns=$(date +%s%N)
ate_ctx suspend actor "${ACTOR}" --atespace "${ATESPACE}" >/dev/null
wait_actor_state "${ATESPACE}" "${ACTOR}" ACTOR_STATE_SUSPENDED 180 >/dev/null
suspend_ms=$((($(date +%s%N) - started_ns) / 1000000))
started_ns=$(date +%s%N)
resume_ok=0
for _ in $(seq 1 60); do
  if resume_err=$(timeout 120s "${ATE_CLI}" --context "${CTX}" --kubeconfig "${KC}" \
    resume actor "${ACTOR}" --atespace "${ATESPACE}" 2>&1 >/dev/null); then
    resume_ok=1
    break
  fi
  grep -q 'no free workers available' <<<"${resume_err}" || die "resume failed: ${resume_err}"
  sleep 2
done
((resume_ok == 1)) || die 'no worker registered as free within 120 seconds'
wait_actor_state "${ATESPACE}" "${ACTOR}" ACTOR_STATE_RUNNING 240 >/dev/null
health=''
for _ in $(seq 1 60); do
  health=$(shim_request "${ATESPACE}" "${ACTOR}" "${STATE_FILE}" \
    '{"method":"GET","path":"/healthz","authenticated":false}')
  health_status=$(jq -r '.status // 0' <<<"${health}")
  if [[ ${health_status} == 200 ]]; then
    break
  fi
  sleep 1
done
health_status=$(jq -r '.status // 0' <<<"${health}")
[[ ${health_status} == 200 ]] || die 'resumed Claude actor did not pass /healthz'
resume_ms=$((($(date +%s%N) - started_ns) / 1000000))
printf 'suspend_ms=%s resume_ms=%s health=200 worker_pool=%s\n' "${suspend_ms}" "${resume_ms}" "${POOL}"

recall_prompt='Without reading any files, what exact nonce did I ask you to remember? Return only that nonce.'
recall_request=$(jq -cn --arg prompt "${recall_prompt}" --arg session "${SESSION_ID}" --arg key "${SESSION_KEY}" \
  '{method:"POST",path:"/turn",authenticated:true,body:{agent:"claude",session_id:$session,session_key:$key,resume:true,timeout_ms:600000,prompt:$prompt}}')
recall_start=$(shim_request "${ATESPACE}" "${ACTOR}" "${STATE_FILE}" "${recall_request}")
recall_start_status=$(jq -r '.status // 0' <<<"${recall_start}")
[[ ${recall_start_status} == 202 ]] || die 'Claude recall turn was not accepted'
recall_id=$(jq -r '.body.id // empty' <<<"${recall_start}")
[[ -n ${recall_id} ]] || die 'Claude recall response omitted its ID'
recall_deadline=$((SECONDS + 610))
recall_json=
while ((SECONDS < recall_deadline)); do
  poll_request=$(jq -cn --arg id "${recall_id}" '{method:"GET",path:("/turn/"+$id),authenticated:true}')
  recall_response=$(shim_request "${ATESPACE}" "${ACTOR}" "${STATE_FILE}" "${poll_request}")
  recall_http_status=$(jq -r '.status // 0' <<<"${recall_response}")
  [[ ${recall_http_status} == 200 ]] || die 'could not read Claude recall status'
  recall_state=$(jq -r '.body.status // empty' <<<"${recall_response}")
  case "${recall_state}" in
  completed | failed | timed_out | interrupted)
    recall_json=$(jq -c '.body' <<<"${recall_response}")
    break
    ;;
  *)
    # Nonterminal turn states continue through the bounded polling loop.
    ;;
  esac
  sleep 2
done
[[ -n ${recall_json} ]] || die 'Claude recall turn exceeded its 10-minute bound'
recall_status=$(jq -r '.status' <<<"${recall_json}")
recall_exit_code=$(jq -r '.exit_code // "unknown"' <<<"${recall_json}")
[[ ${recall_status} == completed && ${recall_exit_code} == 0 ]] || die 'Claude recall turn failed; no retry was sent'
recall_message=$(jq -r '.final_message // empty' <<<"${recall_json}")
[[ ${recall_message} == *"${N1}"* ]] || die 'same-session Claude recall did not contain the nonce'
recall_session=$(jq -r '.native_session_id // empty' <<<"${recall_json}")
[[ ${recall_session} == "${SESSION_ID}" ]] || die 'Claude recall changed native session ID'
marker_hash_after=$(run_actor_command "${ATESPACE}" "${ACTOR}" "${STATE_FILE}" "${marker_command}" 30000)
[[ ${marker_hash_after} == "${marker_hash}" ]] || die 'marker hash changed across suspend/resume'
printf 'recall_turn=completed exit=0 same_session=yes nonce_match=yes marker_hash_match=yes\n'

ate_ctx suspend actor "${ACTOR}" --atespace "${ATESPACE}" >/dev/null
wait_actor_state "${ATESPACE}" "${ACTOR}" ACTOR_STATE_SUSPENDED 180 >/dev/null

provider_logs=$(kubectl_ctx -n mainloop-control logs deployment/round3-claude-provider --since=30m)
if ! worker_logs=$(timeout 300s kubectl --context "${CTX}" --kubeconfig "${KC}" -n "${ATESPACE}" \
  logs -l "ate.dev/worker-pool=${POOL}" --all-containers=true --since=30m); then
  die 'could not fetch WorkerPool logs; refusing to report a zero credential-leak count'
  exit 1
fi
provider_leaks=$(printf '%s' "${provider_logs}" | (
  cd "${BACKEND_DIR}"
  LIVE_PROOF_TOKEN_FILE="${CLAUDE_TOKEN_FILE}" UV_CACHE_DIR=/tmp/uv-cache \
    uv run --no-sync python "${SCRIPT_DIR}/count_token_prefix.py"
))
worker_leaks=$(printf '%s' "${worker_logs}" | (
  cd "${BACKEND_DIR}"
  LIVE_PROOF_TOKEN_FILE="${CLAUDE_TOKEN_FILE}" UV_CACHE_DIR=/tmp/uv-cache \
    uv run --no-sync python "${SCRIPT_DIR}/count_token_prefix.py"
))
provider_leak_count=${provider_leaks#matches=}
worker_leak_count=${worker_leaks#matches=}
[[ ${provider_leak_count} == 0 ]] || die 'Claude credential prefix appeared in provider logs'
[[ ${worker_leak_count} == 0 ]] || die 'Claude credential prefix appeared in WorkerPool logs'
printf 'credential_leak_count_provider_logs=%s\ncredential_leak_count_worker_logs=%s\n' \
  "${provider_leak_count}" "${worker_leak_count}"

# Egress policies have no delete; an empty rule set revokes the credential injection.
update_egress_policy '{"rules":[]}'
cleanup_provider
remove_shim_token_from_state "${STATE_FILE}"
COMPLETE=1
printf 'CLAUDE_TURN_PROOF=PASS actor=%s/%s turn=completed recall=same-session marker=matched concurrent=409 suspend_ms=%s resume_ms=%s\n' \
  "${ATESPACE}" "${ACTOR}" "${suspend_ms}" "${resume_ms}"
printf 'actor remains SUSPENDED; lane namespace, pool, template, and snapshot are retained for review\n'
