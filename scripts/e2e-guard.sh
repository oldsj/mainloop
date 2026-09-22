#!/usr/bin/env bash
# Browser (Playwright) and live-agent e2e suites are disabled by default.
#
# They require a Kind cluster, built images, and in some cases a live Claude
# subscription, so nothing should launch them implicitly. Opt in explicitly:
#
#   ENABLE_E2E=1 make test-run
#   ENABLE_E2E=1 pnpm test
set -euo pipefail

if [[ ${ENABLE_E2E:-0} != "1" ]]; then
  echo "Playwright/e2e tests are disabled." >&2
  echo "Set ENABLE_E2E=1 to run them explicitly (needs Kind + built images, and a live agent for the e2e project)." >&2
  exit 1
fi
