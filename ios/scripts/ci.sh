#!/usr/bin/env bash
# What CI runs (.github/workflows/ios.yml); works the same locally, from anywhere.
#
#   1. The generated API client matches api/openapi.yaml (scripts/generate-api.sh, no diff).
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

scripts/generate-api.sh
generated=Packages/MainloopKit/Sources/MainloopAPI/Generated
drift=$(git status --porcelain -- "${generated}")
if [[ -n ${drift} ]]; then
  echo "${drift}" >&2
  echo "The generated API client is out of date: run ios/scripts/generate-api.sh and commit." >&2
  exit 1
fi

swift test --package-path Packages/MainloopKit

xcodegen generate

xcodebuild -project Mainloop.xcodeproj -scheme Mainloop \
  -destination 'generic/platform=iOS Simulator' -derivedDataPath "${derived}" \
  -skipPackagePluginValidation CODE_SIGNING_ALLOWED=NO build

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
  -skipPackagePluginValidation \
  CODE_SIGN_IDENTITY=- CODE_SIGN_STYLE=Manual DEVELOPMENT_TEAM= test || status=$?

# One simulator at a time: don't leave it running.
xcrun simctl shutdown all || true
exit "${status}"
