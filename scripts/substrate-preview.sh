#!/usr/bin/env bash
set -Eeuo pipefail

CONTEXT=kind-substrate-preview
NAMESPACE=mainloop-control
REGISTRY=localhost:5001
IMAGE_TAG=substrate-preview
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
PREVIEW_KUBECONFIG=
PREFLIGHT_DIGEST=

usage() {
  cat <<'EOF'
Usage: scripts/substrate-preview.sh <build|deploy|open|status|logs> [component]

Commands:
  build              Build app images, mirror PostgreSQL, push and preflight manifests
  deploy             Apply the Kustomize overlay to kind-substrate-preview
  open               Port-forward frontend and backend; Ctrl+C stops both forwards
  status             Show Mainloop pods, Substrate actors, and WorkerPools
  logs [component]   Follow backend, frontend, or postgres logs (default: backend)

Cluster commands require SUBSTRATE_PREVIEW_KUBECONFIG to name the preview kubeconfig.
EOF
}

fail() {
  printf 'Error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

require_preview_kubeconfig() {
  [[ -n ${SUBSTRATE_PREVIEW_KUBECONFIG-} ]] ||
    fail 'SUBSTRATE_PREVIEW_KUBECONFIG is unset; set it to the preview kubeconfig path.'
  [[ -f ${SUBSTRATE_PREVIEW_KUBECONFIG} && -r ${SUBSTRATE_PREVIEW_KUBECONFIG} ]] ||
    fail "preview kubeconfig is not a readable file: ${SUBSTRATE_PREVIEW_KUBECONFIG}"
  PREVIEW_KUBECONFIG="${SUBSTRATE_PREVIEW_KUBECONFIG}"
}

kube() {
  kubectl --kubeconfig "${PREVIEW_KUBECONFIG}" --context "${CONTEXT}" "$@"
}

image_digest_file() {
  local state_root="${XDG_STATE_HOME:-${HOME-}}"
  [[ -n ${state_root} ]] || fail 'HOME is unset and XDG_STATE_HOME is unset; cannot locate the image digest state file.'
  [[ ${state_root} == /* ]] || fail 'XDG_STATE_HOME must be an absolute path.'
  printf '%s/mainloop/substrate-preview/image-digests\n' "${state_root%/}"
}

validate_image_digest() {
  local image_name="$1"
  local digest="$2"
  [[ ${digest} =~ ^sha256:[a-f0-9]{64}$ ]] ||
    fail "recorded ${image_name} digest is missing or invalid; run scripts/substrate-preview.sh build first."
}

load_image_digests() {
  local digest_file
  digest_file="$(image_digest_file)"
  [[ -f ${digest_file} && -r ${digest_file} ]] ||
    fail "no recorded image digests at ${digest_file}; run scripts/substrate-preview.sh build first."

  BACKEND_DIGEST="$(sed -n 's/^backend=//p' "${digest_file}")"
  FRONTEND_DIGEST="$(sed -n 's/^frontend=//p' "${digest_file}")"
  POSTGRES_DIGEST="$(sed -n 's/^postgres=//p' "${digest_file}")"
  validate_image_digest backend "${BACKEND_DIGEST}"
  validate_image_digest frontend "${FRONTEND_DIGEST}"
  validate_image_digest postgres "${POSTGRES_DIGEST}"
}

record_image_digests() {
  local backend_digest="$1"
  local frontend_digest="$2"
  local postgres_digest="$3"
  local digest_file
  local digest_dir
  local temporary_file

  digest_file="$(image_digest_file)"
  digest_dir="${digest_file%/*}"
  mkdir -p -- "${digest_dir}"
  chmod 700 "${digest_dir}"
  temporary_file="$(mktemp "${digest_dir}/.image-digests.XXXXXX")"
  chmod 600 "${temporary_file}"
  {
    printf 'backend=%s\n' "${backend_digest}"
    printf 'frontend=%s\n' "${frontend_digest}"
    printf 'postgres=%s\n' "${postgres_digest}"
  } >"${temporary_file}"
  mv -f -- "${temporary_file}" "${digest_file}"
  printf 'Recorded preflighted image digests in %s\n' "${digest_file}"
}

ate() {
  kubectl-ate --kubeconfig "${PREVIEW_KUBECONFIG}" --context "${CONTEXT}" "$@"
}

preflight_image() {
  local image_ref="$1"
  local repository="$2"
  local push_log="${BUILD_TMPDIR}/${repository//\//_}.push.log"
  local digest

  docker push "${image_ref}" 2>&1 | tee "${push_log}"
  digest="$(sed -nE 's/.*digest: (sha256:[a-f0-9]{64}).*/\1/p' "${push_log}" | tail -n 1)"
  [[ ${digest} =~ ^sha256:[a-f0-9]{64}$ ]] ||
    fail "could not read a sha256 digest from docker push output for ${image_ref}"

  printf 'Preflighting %s at %s\n' "${image_ref}" "${digest}"
  curl -fsI \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
    "http://${REGISTRY}/v2/${repository}/manifests/${digest}" >/dev/null
  PREFLIGHT_DIGEST="${digest}"
}

build_images() {
  require_command docker
  require_command curl
  BUILD_TMPDIR="$(mktemp -d)"
  trap 'rm -rf -- "$BUILD_TMPDIR"' EXIT

  docker build -f backend/Dockerfile -t "${REGISTRY}/mainloop-backend:${IMAGE_TAG}" .
  docker build -f frontend/Dockerfile \
    --build-arg VITE_API_URL=http://localhost:8000 \
    -t "${REGISTRY}/mainloop-frontend:${IMAGE_TAG}" .
  docker pull postgres:16-alpine
  docker tag postgres:16-alpine "${REGISTRY}/postgres:16-alpine"

  preflight_image "${REGISTRY}/mainloop-backend:${IMAGE_TAG}" mainloop-backend
  local backend_digest="${PREFLIGHT_DIGEST}"
  preflight_image "${REGISTRY}/mainloop-frontend:${IMAGE_TAG}" mainloop-frontend
  local frontend_digest="${PREFLIGHT_DIGEST}"
  preflight_image "${REGISTRY}/postgres:16-alpine" postgres
  local postgres_digest="${PREFLIGHT_DIGEST}"
  record_image_digests "${backend_digest}" "${frontend_digest}" "${postgres_digest}"
}

apply_digest_pinned_overlay() (
  set -Eeuo pipefail
  local overlay_dir="${REPO_ROOT}/k8s/apps/mainloop/overlays/substrate-preview"
  local render_dir
  render_dir="$(mktemp -d)"
  trap 'rm -rf -- "${render_dir}"' EXIT
  cp -R -- "${overlay_dir}/." "${render_dir}/"
  cat >>"${render_dir}/kustomization.yaml" <<EOF

images:
  - name: localhost:5001/mainloop-backend
    newName: localhost:5001/mainloop-backend
    digest: ${BACKEND_DIGEST}
  - name: localhost:5001/mainloop-frontend
    newName: localhost:5001/mainloop-frontend
    digest: ${FRONTEND_DIGEST}
  - name: localhost:5001/postgres
    newName: localhost:5001/postgres
    digest: ${POSTGRES_DIGEST}
EOF
  kube apply -k "${render_dir}"
)

ensure_database_secret() {
  local existing_secret
  local database_password

  existing_secret="$(kube get secret mainloop-db-app -n "${NAMESPACE}" --ignore-not-found -o name)"
  [[ -n ${existing_secret} ]] && return

  require_command openssl
  database_password="$(openssl rand -hex 32)"
  printf 'username=mainloop\npassword=%s\n' "${database_password}" |
    kube create secret generic mainloop-db-app -n "${NAMESPACE}" \
      --from-env-file=/dev/stdin --dry-run=client -o yaml |
    kube create -f - >/dev/null
  unset database_password
  printf 'Created mainloop-db-app with a random password.\n'
}

deploy_overlay() {
  load_image_digests
  require_command kubectl
  require_preview_kubeconfig
  kube apply -f "${REPO_ROOT}/k8s/apps/mainloop/overlays/substrate-preview/namespace.yaml"
  ensure_database_secret
  apply_digest_pinned_overlay
}

show_status() {
  require_command kubectl
  require_command kubectl-ate
  require_preview_kubeconfig

  printf '%s\n' '== Mainloop pods =='
  kube get pods -n "${NAMESPACE}" -o wide
  printf '\n%s\n' '== Substrate actors =='
  ate get actor --all-atespaces
  printf '\n%s\n' '== Substrate WorkerPools =='
  kube get workerpools --all-namespaces -o wide
}

follow_logs() {
  local component="${1:-backend}"
  local target

  case "${component}" in
  backend) target=deployment/mainloop-backend ;;
  frontend) target=deployment/mainloop-frontend ;;
  postgres) target=statefulset/postgres ;;
  *) fail "unknown log component '${component}' (choose backend, frontend, or postgres)" ;;
  esac

  require_command kubectl
  require_preview_kubeconfig
  kube logs --follow --tail=200 -n "${NAMESPACE}" "${target}"
}

open_preview() {
  require_command kubectl
  require_preview_kubeconfig

  local forward_dir
  local frontend_pid
  local backend_pid
  local frontend_ready=0
  local backend_ready=0
  local attempt

  forward_dir="$(mktemp -d)"
  cleanup_forwards() {
    local result=$?
    trap - EXIT
    for child_pid in "${frontend_pid-}" "${backend_pid-}"; do
      if [[ -n ${child_pid} ]]; then
        kill "${child_pid}" 2>/dev/null || true
        wait "${child_pid}" 2>/dev/null || true
      fi
    done
    if [[ ${result} -ne 0 ]]; then
      for forward_log in "${forward_dir}"/*.log; do
        [[ -s ${forward_log} ]] && cat "${forward_log}" >&2
      done
    fi
    rm -rf -- "${forward_dir}"
    return "${result}"
  }
  trap cleanup_forwards EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  kube port-forward --address 127.0.0.1 -n "${NAMESPACE}" service/mainloop-frontend 3000:3000 \
    >"${forward_dir}/frontend.log" 2>&1 &
  frontend_pid=$!
  kube port-forward --address 127.0.0.1 -n "${NAMESPACE}" service/mainloop-backend 8000:8000 \
    >"${forward_dir}/backend.log" 2>&1 &
  backend_pid=$!

  attempt=0
  while ((attempt < 60)); do
    attempt=$((attempt + 1))
    if ! kill -0 "${frontend_pid}" 2>/dev/null || ! kill -0 "${backend_pid}" 2>/dev/null; then
      fail 'a port-forward exited before both local ports became ready'
    fi
    if (echo >/dev/tcp/127.0.0.1/3000) >/dev/null 2>&1; then
      frontend_ready=1
    fi
    if (echo >/dev/tcp/127.0.0.1/8000) >/dev/null 2>&1; then
      backend_ready=1
    fi
    [[ ${frontend_ready} -eq 1 && ${backend_ready} -eq 1 ]] && break
    sleep 0.5
  done
  [[ ${frontend_ready} -eq 1 && ${backend_ready} -eq 1 ]] ||
    fail 'timed out waiting for frontend and backend port-forwards'

  printf 'Mainloop: http://127.0.0.1:3000\n'
  printf 'Backend API: http://127.0.0.1:8000/docs\n'
  printf 'Press Ctrl+C to stop both port-forwards.\n'
  wait -n "${frontend_pid}" "${backend_pid}"
}

main() {
  local command="${1-}"
  case "${command}" in
  build)
    [[ $# -eq 1 ]] || fail 'build takes no additional arguments'
    cd "${REPO_ROOT}"
    build_images
    ;;
  deploy)
    [[ $# -eq 1 ]] || fail 'deploy takes no additional arguments'
    deploy_overlay
    ;;
  open)
    [[ $# -eq 1 ]] || fail 'open takes no additional arguments'
    open_preview
    ;;
  status)
    [[ $# -eq 1 ]] || fail 'status takes no additional arguments'
    show_status
    ;;
  logs)
    [[ $# -le 2 ]] || fail 'logs accepts at most one component'
    follow_logs "${2:-backend}"
    ;;
  -h | --help | help)
    usage
    ;;
  *)
    usage >&2
    fail "unknown command '${command-}'"
    ;;
  esac
}

main "$@"
