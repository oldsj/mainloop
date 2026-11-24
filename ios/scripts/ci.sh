#!/usr/bin/env bash
# What CI runs (.github/workflows/ios.yml); works the same locally, from anywhere.
#
#   1. Regenerates the API client from api/openapi.yaml; a stale committed copy is a warning.
#   2. MainloopKit's tests on macOS (`swift test`): models, fixtures against the generated types.
#   3. The app built for the simulator without signing, and its tests on an iPhone Duo
#      simulator when the runtime has one, otherwise the newest iPhone.
#
#   ios/scripts/ci.sh
#   SIMULATOR="iPhone Duo" ios/scripts/ci.sh
set -euo pipefail

cd "$(dirname "$0")/.."
derived=build/DerivedData

xcodebuild -version

# The build and tests below always use a client freshly generated from api/openapi.yaml.
# A committed copy that differs is a warning, not a failure: a contract change made in a
# workspace without Swift can't regenerate it. The patch is written to build/ and CI uploads it
# as the `ios-generated-client` artifact, to apply with `git apply` (see AGENTS.md).
scripts/generate-api.sh
generated=Packages/MainloopKit/Sources/MainloopAPI/Generated
mkdir -p build
rm -f build/generated-client.patch
drift=$(git status --porcelain -- "${generated}")
if [[ -n ${drift} ]]; then
  msg="The committed Swift API client is out of date with api/openapi.yaml. From the repository root, apply the ios-generated-client artifact (git apply generated-client.patch), or run ios/scripts/generate-api.sh."
  if [[ -n ${GITHUB_ACTIONS-} ]]; then
    # Intent-to-add, so new generated files are in the patch too (CI's checkout only).
    git add -N -- "${generated}"
    git diff -- "${generated}" >build/generated-client.patch
    echo "::warning title=Swift API client::${msg}"
  else
    echo "${drift}" >&2
    echo "warning: ${msg}" >&2
  fi
fi

swift test --package-path Packages/MainloopKit

xcodegen generate

xcodebuild -project Mainloop.xcodeproj -scheme Mainloop \
  -destination 'generic/platform=iOS Simulator' -derivedDataPath "${derived}" \
  CODE_SIGNING_ALLOWED=NO build

if [[ -z ${SIMULATOR-} ]]; then
  # Prefer iPhone Duo on the newest runtime; otherwise that runtime's first iPhone.
  SIMULATOR=$(xcrun simctl list devices available --json | jq -r '
    [.devices | to_entries[] | select(.key | test("SimRuntime\\.iOS-"))
      | {v: (.key | capture("iOS-(?<a>[0-9]+)-(?<b>[0-9]+)") | [(.a|tonumber), (.b|tonumber)]),
         d: .value[]}
      | select(.d.name | startswith("iPhone"))]
    | sort_by(.v) | (last.v) as $newest | map(select(.v == $newest))
    | ((map(select(.d.name == "iPhone Duo")) | first) // first) | .d.udid // empty')
  [[ -n ${SIMULATOR} ]] || {
    echo "No iPhone simulator is available." >&2
    exit 1
  }
  dest="platform=iOS Simulator,id=${SIMULATOR}"
  xcrun simctl list devices available | grep "${SIMULATOR}" || true
else
  dest="platform=iOS Simulator,name=${SIMULATOR}"
fi

rm -rf build/Tests.xcresult
status=0
xcodebuild -project Mainloop.xcodeproj -scheme Mainloop -destination "${dest}" \
  -derivedDataPath "${derived}" -resultBundlePath build/Tests.xcresult \
  CODE_SIGN_IDENTITY=- CODE_SIGN_STYLE=Manual DEVELOPMENT_TEAM= test || status=$?

# One simulator at a time: don't leave it running.
xcrun simctl shutdown all || true
exit "${status}"
