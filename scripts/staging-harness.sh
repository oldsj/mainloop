#!/usr/bin/env bash
# Staging kagent harness images for mainloop-staging (`make staging-harness`).
#
#   staging-harness.sh build        build arm64 Claude and Codex harness images from the
#                                    kagent fork, push dev-<sha> tags to ghcr, record them
#                                    and apply the *-workspace-staging agents with them
#   staging-harness.sh image <p>    print the recorded image for claude|codex, else the
#                                    production pin from the staging manifests (DevSpace var)
#   staging-harness.sh reset        forget recorded images (next `make staging` uses the pins)
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
STATE="${ROOT}/.devspace/staging-harness"
MANIFESTS="${ROOT}/k8s/apps/mainloop/overlays/staging/kagent"
KAGENT_DIR=${KAGENT_DIR:-${HOME}/dev/kagent}
REGISTRY=ghcr.io
REPO=oldsj

image() {
  local provider=$1
  if [[ -s "${STATE}/${provider}" ]]; then
    cat "${STATE}/${provider}"
  else
    sed -n 's/^ *image: *\(.*\)$/\1/p' "${MANIFESTS}/${provider}.yaml" | head -1
  fi
}

build() {
  [[ -d "${KAGENT_DIR}/.git" ]] || {
    echo "kagent fork not found at ${KAGENT_DIR}" >&2
    exit 1
  }
  local sha tag dirty
  sha=$(git -C "${KAGENT_DIR}" rev-parse --short=7 HEAD)
  tag="dev-${sha}"
  dirty=$(git -C "${KAGENT_DIR}" status --porcelain)
  if [[ -n ${dirty} ]]; then
    echo "${KAGENT_DIR} has uncommitted changes; commit them first (images are pushed to ghcr)" >&2
    exit 1
  fi
  echo "Building arm64 harness images ${tag} from ${KAGENT_DIR}"
  make -C "${KAGENT_DIR}" build-claude-harness build-codex-harness \
    LOCALARCH=arm64 DOCKER_REGISTRY="${REGISTRY}" DOCKER_REPO="${REPO}" \
    CLAUDE_HARNESS_IMAGE_NAME=kagent-claude-harness CLAUDE_HARNESS_IMAGE_TAG="${tag}" \
    CODEX_HARNESS_IMAGE_NAME=kagent-codex-harness CODEX_HARNESS_IMAGE_TAG="${tag}"

  mkdir -p "${STATE}"
  local provider ref digest
  for provider in claude codex; do
    ref="${REGISTRY}/${REPO}/kagent-${provider}-harness:${tag}"
    digest=$(docker buildx imagetools inspect "${ref}" --format '{{json .Manifest}}' | jq -er .digest)
    echo "${ref}@${digest}" >"${STATE}/${provider}"
    echo "${provider}-workspace-staging-harness -> ${ref}@${digest}"
  done
  "${ROOT}/scripts/staging.sh" agents
}

case "${1-}" in
image) image "${2:?provider}" ;;
build) build ;;
reset) rm -rf "${STATE}" ;;
*)
  sed -n '2,10p' "$0" >&2
  exit 2
  ;;
esac
