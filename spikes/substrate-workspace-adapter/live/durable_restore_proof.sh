#!/usr/bin/env bash
# Snapshot a real repository and restore it after every worker in this lane's
# pool has been deleted. Run only through the supervisor's explicit cluster lane.
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

ATESPACE=lane-a-durable-20260924
POOL=lane-a-durable-20260924
ACTOR=repo-live-proof
STATE_FILE=${STATE_ROOT}/durable-gate5-state.json
BEFORE_MANIFEST=${STATE_ROOT}/durable-before.tsv
AFTER_MANIFEST=${STATE_ROOT}/durable-after.tsv
COMPLETE=0
CLEANUP_ARMED=0

finish() {
  local status=$?
  stop_router
  if ((status != 0 && COMPLETE == 0 && CLEANUP_ARMED == 1)); then
    printf 'run failed; attempting cleanup of the owned durable lane\n' >&2
    "${SCRIPT_DIR}/cleanup-lane-a.sh" durable || printf 'cleanup did not complete; inspect %s\n' "${STATE_FILE}" >&2
  fi
  return "${status}"
}

require_worker_image
prepare_state_file "${STATE_FILE}" "${ATESPACE}"
rm -f "${BEFORE_MANIFEST}" "${AFTER_MANIFEST}"
if timeout 300s kubectl --context "${CTX}" --kubeconfig "${KC}" \
  get workerpool "${POOL}" -n "${ATESPACE}" -o name >/dev/null 2>&1; then
  die "WorkerPool ${ATESPACE}/${POOL} already exists; refusing to adopt it"
fi
trap finish EXIT INT TERM

printf 'creating the durable restore actor from the product template\n'
# shellcheck disable=SC2310 # handle gate5_setup's returned status and persisted ownership below.
if gate5_setup "${ATESPACE}" "${POOL}" "${ACTOR}" lane-a-durable-20260924 "${STATE_FILE}" github.com; then
  CLEANUP_ARMED=1
else
  setup_status=$?
  if jq -e '.namespace_uid | strings | length > 0' "${STATE_FILE}" >/dev/null 2>&1; then
    CLEANUP_ARMED=1
  fi
  exit "${setup_status}"
fi
start_router

uid=$(run_actor_command "${ATESPACE}" "${ACTOR}" "${STATE_FILE}" 'id -u' 30000)
if [[ ${uid} != 10001 ]]; then
  die "actor runtime uid is ${uid}; durable owner proof requires image uid 10001"
fi
printf 'actor runtime uid=%s\n' "${uid}"

setup_command=$(
  cat <<'EOF'
set -eu
git clone --depth=1 --quiet https://github.com/octocat/Hello-World.git /work/repo/source
printf 'durable-owner-check\n' > /work/repo/lane-a-owned-by-10001.txt
chmod 0644 /work/repo/lane-a-owned-by-10001.txt
printf 'durable-private-check\n' > /work/repo/lane-a-mode-0600.txt
chmod 0600 /work/repo/lane-a-mode-0600.txt
test "$(stat -c %u /work/repo/lane-a-owned-by-10001.txt)" = 10001
test "$(stat -c %g /work/repo/lane-a-owned-by-10001.txt)" = 10001
test "$(stat -c %a /work/repo/lane-a-mode-0600.txt)" = 600
test -s /work/repo/source/README
git -C /work/repo/source rev-parse HEAD
EOF
)
repo_commit=$(run_actor_command "${ATESPACE}" "${ACTOR}" "${STATE_FILE}" "${setup_command}" 300000)
printf 'repository commit=%s\n' "${repo_commit}"

manifest_command=$(
  cat <<'EOF'
set -eu
# The shim writes its own run records, including this command's, under runs/.
find /work/repo -path /work/repo/.mainloop/exec-shim/runs -prune -o -type f -print | LC_ALL=C sort | while IFS= read -r path; do
  relative=${path#/work/repo/}
  owner=$(stat -c %u "$path")
  group=$(stat -c %g "$path")
  mode=$(stat -c %a "$path")
  digest=$(sha256sum "$path" | cut -d ' ' -f 1)
  printf '%s\t%s\t%s\t%s\t%s\n' "$relative" "$owner" "$group" "$mode" "$digest"
done
EOF
)
before=$(run_actor_command "${ATESPACE}" "${ACTOR}" "${STATE_FILE}" "${manifest_command}" 120000)
printf '%s\n' "${before}" >"${BEFORE_MANIFEST}"
chmod 600 "${BEFORE_MANIFEST}"
before_count=$(wc -l <"${BEFORE_MANIFEST}" | tr -d ' ')
printf 'manifest_before files=%s owner_file_uid=10001 private_file_mode=600\n' "${before_count}"

started_ns=$(date +%s%N)
ate_ctx suspend actor "${ACTOR}" --atespace "${ATESPACE}" >/dev/null
suspended_json=$(wait_actor_state "${ATESPACE}" "${ACTOR}" ACTOR_STATE_SUSPENDED 180)
suspend_ms=$((($(date +%s%N) - started_ns) / 1000000))
snapshot_uri=$(jq -r '.status.externalSnapshot.snapshotUri // .status.externalSnapshot.snapshot_uri // empty' <<<"${suspended_json}")
snapshot_scope=$(jq -r '.status.externalSnapshot.contentScope // .status.externalSnapshot.content_scope // empty' <<<"${suspended_json}")
[[ -n ${snapshot_uri} ]] || die 'suspend completed without an external snapshot URI'
[[ ${snapshot_scope} == SNAPSHOT_CONTENT_SCOPE_FULL || ${snapshot_scope} == 1 ]] || die "snapshot scope was not FULL: ${snapshot_scope}"
printf 'suspend_ms=%s snapshot_scope=FULL\n' "${suspend_ms}"

old_pods=$(kubectl_ctx -n "${ATESPACE}" get pods -l "ate.dev/worker-pool=${POOL}" -o json)
old_count=$(jq '.items | length' <<<"${old_pods}")
[[ ${old_count} == 2 ]] || die "expected 2 workers in the owned pool, found ${old_count}"
old_uids=$(jq -r '.items[].metadata.uid' <<<"${old_pods}" | sort)
printf 'deleting owned worker pods after completed suspend:\n'
jq -r '.items[] | "  \(.metadata.name) uid=\(.metadata.uid)"' <<<"${old_pods}"
kubectl_ctx -n "${ATESPACE}" delete pod -l "ate.dev/worker-pool=${POOL}" --wait=true --timeout=120s >/dev/null

replacement_json=
for _ in $(seq 1 180); do
  candidate=$(kubectl_ctx -n "${ATESPACE}" get pods -l "ate.dev/worker-pool=${POOL}" -o json)
  count=$(jq '[.items[] | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))] | length' <<<"${candidate}")
  if [[ ${count} == 2 ]]; then
    replacement_json=${candidate}
    break
  fi
  sleep 1
done
[[ -n ${replacement_json} ]] || die 'replacement WorkerPool pods did not become Ready within 180 seconds'
new_uids=$(jq -r '.items[].metadata.uid' <<<"${replacement_json}" | sort)
while IFS= read -r new_uid; do
  [[ -n ${new_uid} ]] || continue
  if grep -Fxq "${new_uid}" <<<"${old_uids}"; then
    die 'a replacement worker reused an old pod UID; worker-loss proof is inconclusive'
  fi
done <<<"${new_uids}"
printf 'replacement workers ready with new pod UIDs\n'

started_ns=$(date +%s%N)
# Ready pods register as free workers a little later; retry only that refusal.
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
((resume_ok == 1)) || die 'no replacement worker registered as free within 120 seconds'
resumed_json=$(wait_actor_state "${ATESPACE}" "${ACTOR}" ACTOR_STATE_RUNNING 240)
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
[[ ${health_status} == 200 ]] || die 'restored actor did not pass /healthz'
resume_ms=$((($(date +%s%N) - started_ns) / 1000000))
assigned_pod=$(jq -r '.status.workerAssignment.workerPod // empty' <<<"${resumed_json}")
[[ -n ${assigned_pod} ]] || die 'restored actor has no worker-pod assignment'
replacement_names=$(jq -r '.items[].metadata.name' <<<"${replacement_json}")
grep -Fxq "${assigned_pod}" <<<"${replacement_names}" || die 'actor resumed on a pod outside the replacement worker set'
printf 'resume_ms=%s worker_pod=%s health=200\n' "${resume_ms}" "${assigned_pod}"

after=$(run_actor_command "${ATESPACE}" "${ACTOR}" "${STATE_FILE}" "${manifest_command}" 120000)
printf '%s\n' "${after}" >"${AFTER_MANIFEST}"
chmod 600 "${AFTER_MANIFEST}"
after_count=$(wc -l <"${AFTER_MANIFEST}" | tr -d ' ')
diff -u "${BEFORE_MANIFEST}" "${AFTER_MANIFEST}"
printf 'manifest_after files=%s exact_match=yes\n' "${after_count}"
printf 'manifest rows (relative path, uid, gid, mode, sha256):\n'
cat "${AFTER_MANIFEST}"

ate_ctx suspend actor "${ACTOR}" --atespace "${ATESPACE}" >/dev/null
wait_actor_state "${ATESPACE}" "${ACTOR}" ACTOR_STATE_SUSPENDED 180 >/dev/null
remove_shim_token_from_state "${STATE_FILE}"
COMPLETE=1
printf 'DURABLE_RESTORE_PROOF=PASS actor=%s/%s suspend_ms=%s resume_ms=%s worker_loss=yes files=%s\n' \
  "${ATESPACE}" "${ACTOR}" "${suspend_ms}" "${resume_ms}" "${after_count}"
printf 'actor remains SUSPENDED; lane resources are retained for review\n'
