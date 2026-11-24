# Mainloop for iOS

A native iPhone app for Mainloop's owner, laid out for iPhone Duo. See
[docs/specs/ios-app.md](../docs/specs/ios-app.md) for what it does today and what is planned.

## Layout

| Path                                 | What it is                                                                                                                                                                                                                           |
| ------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `project.yml`                        | XcodeGen spec, the source of truth. The generated `Mainloop.xcodeproj` and `App/Info.plist` are gitignored.                                                                                                                          |
| `Config/Mainloop.xcconfig`           | Placeholder bundle ID and origin. Includes `Config/Local.xcconfig` if it exists.                                                                                                                                                     |
| `App/`                               | SwiftUI screens: Needs you, Sessions, Settings, and the list-beside-detail layout for the inner screen.                                                                                                                              |
| `Tests/`                             | App tests, run on the simulator.                                                                                                                                                                                                     |
| `Packages/MainloopKit`               | Everything that doesn't need UIKit: `MainloopAPI` (the generated client) and `MainloopCore` (models, fictional fixtures, layout math). Its tests run on macOS with `swift test`.                                                     |
| `Packages/MainloopKit/.../Generated` | The Swift client generated from `api/openapi.yaml`. Do not edit; run `scripts/generate-api.sh`.                                                                                                                                      |
| `Packages/APIGen`                    | Pins swift-openapi-generator, and `PrepareContract`, which rewrites a temporary copy of the contract into a form the generator reads (nullable `anyOf: [X, null]` becomes an optional `X`; integer bounds beyond Int64 are dropped). |
| `scripts/generate-api.sh`            | Regenerates the client. Add an operation to `Packages/APIGen/openapi-generator-config.yaml` first.                                                                                                                                   |
| `scripts/ci.sh`                      | What CI runs: the generated-client drift check, `swift test`, then the simulator build and tests.                                                                                                                                    |
| `Design/make-app-icon.py`            | Draws the placeholder app icon (standard library only).                                                                                                                                                                              |

## Build

Requires Xcode 27.1 or later (the iOS 27.1 SDK is what lays the app out for iPhone Duo) and
[XcodeGen](https://github.com/yonaskolb/XcodeGen). CI runs on GitHub's `xcode-27` image.

```sh
ios/scripts/ci.sh                                      # everything CI runs
swift test --package-path ios/Packages/MainloopKit     # models and fixtures only, no simulator
```

By hand:

```sh
cd ios
xcodegen generate
open Mainloop.xcodeproj
```

The app builds with the placeholder origin and shows sample data. To run it on a phone, copy
`Config/Local.xcconfig.example` to `Config/Local.xcconfig` and set your team ID, bundle ID and
Mainloop origin. Nothing in `Local.xcconfig` is committed. Until sign-in lands, a real origin
shows "Signed out".

## Rules

- The API is called only through the generated client, as in `web/`.
- Fixtures, previews and screenshots use fictional data only.
- No signing material in the tree or in CI logs; CI builds unsigned.
