#!/usr/bin/env bash
# shellcheck disable=SC2312,SC2310,SC2311,SC2249  # pedantic optional checks; spike scripts
# Bounded local demo: kind + real Herdr + two stand-in agent kinds.
# Never cleans up: cluster, PVC, images and evidence are left for inspection.
set -euo pipefail

SPIKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_ID="${RUN_ID:-20260920T022900Z}"
EVIDENCE="${EVIDENCE_DIR:-${SPIKE_DIR}/../../.tasknotes/runs/${RUN_ID}/spike-evidence}"
CLUSTER=mainloop-test
CONTEXT=kind-mainloop-test
IMAGE="mainloop-spike-herdr:${RUN_ID}"
KUBECONFIG_FILE="${KUBECONFIG_FILE:-${EVIDENCE}/kubeconfig-${CLUSTER}}" # run-owned, never committed
NS=herdr-spike
SUDO="${SUDO:-sudo -n}" # docker is root-only on this host
KIND_BIN="$(command -v kind)"
HERDR_BIN="$(readlink -f "$(command -v herdr)")"

mkdir -p "${EVIDENCE}"
LOG="${EVIDENCE}/demo.log"
BUILD_CTX="$(mktemp -d)"
trap 'rm -rf "$BUILD_CTX"' EXIT # only the transient build context is removed

say() { printf '%s\n' "$*" | tee -a "${LOG}"; }
run() {
  say "\$ $*"
  "$@" 2>&1 | tee -a "${LOG}"
}
k() { kubectl --kubeconfig "${KUBECONFIG_FILE}" --context "${CONTEXT}" "$@"; }
kx() { k -n "${NS}" exec workspace-0 -c workspace -- "$@"; }

say "== Mainloop spike: Kubernetes + Herdr + arbitrary agents (${RUN_ID}) =="
say "REAL:     kind cluster ${CLUSTER}, StatefulSet/PVC, non-root pod, Herdr $(herdr --version | cut -d' ' -f2) server + agent detection"
say "STAND-IN: 'pi' and 'qwen' executables are one deterministic script (no provider, no credentials)"

# --- static checks ---
bash -n "${SPIKE_DIR}/demo.sh" "${SPIKE_DIR}"/bin/*
say "static: bash -n ok"

# --- cluster (explicit run-owned kubeconfig; create only if absent) ---
if ! ${SUDO} "${KIND_BIN}" get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
  run ${SUDO} "${KIND_BIN}" create cluster --name "${CLUSTER}" --kubeconfig "${KUBECONFIG_FILE}" --wait 120s
  ${SUDO} chown "$(id -u):$(id -g)" "${KUBECONFIG_FILE}"
elif [[ ! -s ${KUBECONFIG_FILE} ]]; then
  ${SUDO} "${KIND_BIN}" get kubeconfig --name "${CLUSTER}" >"${KUBECONFIG_FILE}"
fi
chmod 600 "${KUBECONFIG_FILE}"
[[ "$(k config current-context)" == "${CONTEXT}" ]] || {
  say "wrong context"
  exit 1
}
say "context: $(k config current-context)"

# --- image ---
cp "${HERDR_BIN}" "${BUILD_CTX}/herdr"
cp -r "${SPIKE_DIR}/bin" "${SPIKE_DIR}/Dockerfile" "${BUILD_CTX}/"
run ${SUDO} docker build -q -t "${IMAGE}" "${BUILD_CTX}"
IMAGE_ID="$(${SUDO} docker image inspect "${IMAGE}" --format '{{.Id}}')"
say "image: ${IMAGE} id=${IMAGE_ID}"
say "image user: $(${SUDO} docker image inspect "${IMAGE}" --format '{{.Config.User}}')"
say "image herdr: $(${SUDO} docker run --rm --entrypoint herdr "${IMAGE}" --version)"
run ${SUDO} "${KIND_BIN}" load docker-image "${IMAGE}" --name "${CLUSTER}"

# --- deploy ---
sed "s#__IMAGE__#${IMAGE}#" "${SPIKE_DIR}/k8s/workspace.yaml" >"${EVIDENCE}/workspace.rendered.yaml"
run kubectl --kubeconfig "${KUBECONFIG_FILE}" --context "${CONTEXT}" apply -f "${EVIDENCE}/workspace.rendered.yaml"
k -n "${NS}" rollout status statefulset/workspace --timeout=180s | tee -a "${LOG}"
POD1_UID="$(k -n "${NS}" get pod workspace-0 -o jsonpath='{.metadata.uid}')"
PVC="$(k -n "${NS}" get pod workspace-0 -o jsonpath='{.spec.volumes[?(@.name=="workspace")].persistentVolumeClaim.claimName}')"
say "pod1 uid=${POD1_UID} pvc=${PVC}"
say "pod user: $(kx id)"
say "pod herdr: $(kx herdr --version)"
say "sa token mounted: $(kx sh -c 'ls /var/run/secrets/kubernetes.io 2>&1 | head -1')"
for _ in $(seq 1 20); do
  kx herdr --session mainloop-spike status server >/dev/null 2>&1 && break
  sleep 1
done

# --- journey 1: both kinds, same operation ---
NONCE1="n1-${RANDOM}${RANDOM}"
for b in alpha beta; do
  say "-- start ${b} (config: $(kx sh -c "tr '\n' ' ' </etc/agent-config/${b}.env"))"
  kx agentctl start "${b}" | tee -a "${LOG}"
  say "-- prompt ${b} nonce=${NONCE1}-${b}"
  kx agentctl prompt "${b}" "${NONCE1}-${b}" | tee -a "${LOG}"
done

# --- normal pod replacement ---
say "== deleting pod workspace-0 normally =="
run k -n "${NS}" delete pod workspace-0 --wait=true
k -n "${NS}" wait --for=condition=Ready pod/workspace-0 --timeout=180s | tee -a "${LOG}"
POD2_UID="$(k -n "${NS}" get pod workspace-0 -o jsonpath='{.metadata.uid}')"
PVC2="$(k -n "${NS}" get pod workspace-0 -o jsonpath='{.spec.volumes[?(@.name=="workspace")].persistentVolumeClaim.claimName}')"
say "pod2 uid=${POD2_UID} pvc=${PVC2}"
[[ ${POD1_UID} != "${POD2_UID}" ]] && [[ ${PVC} == "${PVC2}" ]] || {
  say "FAIL: pod not replaced or PVC changed"
  exit 1
}
for _ in $(seq 1 20); do
  kx herdr --session mainloop-spike status server >/dev/null 2>&1 && break
  sleep 1
done
say "persisted identities: $(kx sh -c 'cat /workspace/repo/.mainloop/*.identity.json')"
say "persisted native state: $(kx sh -c 'ls /workspace/.standin/*')"
say "herdr live agents after restart (agent processes do not survive): $(kx herdr --session mainloop-spike agent list | jq -c '.result.agents|length')"

# --- journey 2: restart agents, resume native state, follow-up prompt ---
NONCE2="n2-${RANDOM}${RANDOM}"
for b in alpha beta; do
  kx agentctl start "${b}" | tee -a "${LOG}"
  say "-- follow-up prompt ${b} nonce=${NONCE2}-${b}"
  REPLY="$(kx agentctl prompt "${b}" "${NONCE2}-${b}")"
  say "${REPLY}"
  case "${REPLY}" in *"turn=2"*"prior=${NONCE1}-${b}"*) say "OK ${b} resumed native session (turn 2, prior=${NONCE1}-${b})" ;; *)
    say "FAIL ${b} did not resume"
    exit 1
    ;;
  esac
done
say "pod2 herdr identities: $(kx herdr --session mainloop-spike agent list | jq -c '[.result.agents[]|{name,agent,pane_id,terminal_id}]')"
say "== PASS. Left running: cluster ${CLUSTER}, ns ${NS}, PVC ${PVC}, image ${IMAGE}, evidence ${EVIDENCE} =="
say "inspect: kubectl --kubeconfig ${KUBECONFIG_FILE} --context ${CONTEXT} -n ${NS} exec -it workspace-0 -- herdr --session mainloop-spike"
