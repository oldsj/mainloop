#!/usr/bin/env bash
# shellcheck disable=SC2312,SC2310,SC2311,SC2249  # pedantic optional checks; spike scripts
# Build the real-agent workspace image (host claude/codex/herdr copied into a transient build
# context, never committed), load it into kind-mainloop-test, create credential Secrets BY PATH
# (values are never printed), and apply the workspace manifest. No cleanup, nothing deleted.
set -euo pipefail
SPIKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_ID="${RUN_ID:?RUN_ID required}"
KUBECONFIG_FILE="${KUBECONFIG_FILE:?run-owned kubeconfig required}"
CTX=kind-mainloop-test
NS=herdr-spike
IMAGE="mainloop-spike-herdr:real-${RUN_ID}${IMAGE_SUFFIX-}"
k() { kubectl --kubeconfig "${KUBECONFIG_FILE}" --context "${CTX}" "$@"; }
B="$(mktemp -d)"
trap 'rm -rf "$B"' EXIT
cp "$(readlink -f "$(command -v herdr)")" "${B}/herdr"
cp "$(readlink -f "$(command -v claude)")" "${B}/claude"
cp "$(readlink -f "$(command -v codex)")" "${B}/codex"
# Codex shell tools need its companion helper (without it: "codex-code-mode-host is missing").
cp "$(dirname "$(readlink -f "$(command -v codex)")")/codex-code-mode-host" "${B}/codex-code-mode-host"
cp -r "${SPIKE_DIR}/bin" "${SPIKE_DIR}/Dockerfile" "${B}/"
sudo -n docker build -q -t "${IMAGE}" "${B}"
sudo -n docker image inspect "${IMAGE}" --format 'image {{.Id}} user={{.Config.User}}'
sudo -n "$(command -v kind)" load docker-image "${IMAGE}" --name mainloop-test
# Secrets by path. Claude token: whitespace stripped through a process substitution, never echoed.
k create ns "${NS}" --dry-run=client -o yaml | k apply -f - >/dev/null
k -n "${NS}" create secret generic claude-oauth --from-file=oauth-token=<(tr -d ' \r\n' <"${HOME}/.claude-token") --dry-run=client -o yaml | k apply -f - >/dev/null
k -n "${NS}" create secret generic codex-auth --from-file=auth.json="${HOME}/.codex/auth.json" --dry-run=client -o yaml | k apply -f - >/dev/null
sed "s#__IMAGE__#${IMAGE}#" "${SPIKE_DIR}/k8s/workspace.yaml" | k apply -f -
k -n "${NS}" rollout status statefulset/workspace --timeout=240s
