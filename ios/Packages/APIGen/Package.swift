// swift-tools-version: 6.0
// Tools for ios/scripts/generate-api.sh. Not part of the app.
// - swift-openapi-generator, pinned.
// - PrepareContract: rewrites a temporary copy of api/openapi.yaml into a form the generator
//   reads (see Sources/PrepareContract/main.swift). The contract itself is never changed.
import PackageDescription

let package = Package(
    name: "APIGen",
    platforms: [.macOS("15.0")],
    dependencies: [
        .package(url: "https://github.com/apple/swift-openapi-generator", exact: "1.14.4"),
        .package(url: "https://github.com/jpsim/Yams", exact: "6.2.2"),
    ],
    targets: [
        .executableTarget(name: "PrepareContract", dependencies: ["Yams"]),
    ]
)
