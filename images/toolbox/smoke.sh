#!/bin/bash
# Smoke test for the toolbox image: every tool starts and reports the version install-tools pinned
# (or, for apt packages, a version at all), and the image runs as the unprivileged user pods use.
# Run inside the image: docker run --rm -v "$PWD/images/toolbox/smoke.sh:/smoke.sh:ro" <image> /smoke.sh
# shellcheck disable=SC2312
set -euo pipefail
[[ $(id -u):$(id -g) == 65532:65532 ]] || {
  echo "not uid 65532" >&2
  exit 1
}

want() {
  grep -E "^$1=" /usr/local/share/toolbox/versions | cut -d= -f2
}
# check <tool> <version command...>: the command's output must contain the pinned version.
check() {
  local tool=$1 v out
  shift
  v=$(want "${tool}")
  [[ -n ${v} ]] || {
    echo "no pin recorded for ${tool}" >&2
    exit 1
  }
  out=$("$@" 2>&1) || {
    echo "${tool}: '$*' failed: ${out}" >&2
    exit 1
  }
  grep -qF -- "${v#v}" <<<"${out}" || {
    echo "${tool}: want ${v}, got: ${out}" >&2
    exit 1
  }
  echo "ok ${tool} ${v}"
}
# present <name> <version command...>: for tools installed from the distribution.
present() {
  local name=$1
  shift
  "$@" >/dev/null 2>&1 || {
    echo "${name}: '$*' failed" >&2
    exit 1
  }
  echo "ok ${name}"
}

check kubectl kubectl version --client
check talosctl talosctl version --client --short
check omnictl omnictl --version
check helm helm version --short
check kustomize kustomize version
check cilium cilium version --client
check crane crane version
check yq yq --version
check gh gh --version
check flyctl flyctl version
check doctl doctl version
check uv uv --version
check node node --version
check pnpm pnpm --version
check wrangler wrangler --version
present python3 python3 --version
present jq jq --version
present psql psql --version
present ssh ssh -V
present dig dig -v
present curl curl --version
present openssl openssl version
present git git --version
present ping ping -V
present envsubst envsubst --version
# No docker client or buildah: images are handled with crane, and builds stay in CI.
for banned in docker buildah podman; do
  ! command -v "${banned}" >/dev/null || {
    echo "${banned} must not be in the toolbox" >&2
    exit 1
  }
done
echo "toolbox smoke ok"
