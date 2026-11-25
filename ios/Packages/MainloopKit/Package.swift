// swift-tools-version: 6.0
// MainloopKit: everything in the iOS app that doesn't need UIKit or a simulator.
// - MainloopAPI: the client generated from api/openapi.yaml by ios/scripts/generate-api.sh
//   (committed in Sources/MainloopAPI/Generated), filtered to the operations the app calls.
// - MainloopCore: device keys and signing, the approval sheet's model, the web view's session,
//   bridge and navigation rules, over the generated types.
// `swift test` runs the tests on macOS; the app target builds against the same package.
import PackageDescription

let package = Package(
    name: "MainloopKit",
    platforms: [.iOS("27.1"), .macOS("26.0")],
    products: [
        .library(name: "MainloopAPI", targets: ["MainloopAPI"]),
        .library(name: "MainloopCore", targets: ["MainloopCore"]),
    ],
    dependencies: [
        .package(url: "https://github.com/apple/swift-openapi-runtime", exact: "1.13.4"),
        .package(url: "https://github.com/apple/swift-openapi-urlsession", exact: "1.4.0"),
        .package(url: "https://github.com/apple/swift-http-types", exact: "1.8.0"),
    ],
    targets: [
        .target(
            name: "MainloopAPI",
            dependencies: [
                .product(name: "OpenAPIRuntime", package: "swift-openapi-runtime"),
                .product(name: "OpenAPIURLSession", package: "swift-openapi-urlsession"),
                .product(name: "HTTPTypes", package: "swift-http-types"),
            ]
        ),
        .target(
            name: "MainloopCore",
            dependencies: [
                "MainloopAPI",
                .product(name: "OpenAPIRuntime", package: "swift-openapi-runtime"),
                .product(name: "HTTPTypes", package: "swift-http-types"),
            ],
            resources: [.copy("Fixtures")]
        ),
        .testTarget(
            name: "MainloopCoreTests",
            dependencies: [
                "MainloopCore", "MainloopAPI",
                .product(name: "OpenAPIRuntime", package: "swift-openapi-runtime"),
                .product(name: "HTTPTypes", package: "swift-http-types"),
            ]
        ),
    ],
    swiftLanguageModes: [.v6]
)
