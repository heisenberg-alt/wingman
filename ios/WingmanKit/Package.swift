// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "WingmanKit",
    platforms: [.iOS(.v17), .macOS(.v14)],
    products: [
        .library(name: "WingmanKit", targets: ["WingmanKit"])
    ],
    targets: [
        .target(name: "WingmanKit"),
        .testTarget(name: "WingmanKitTests", dependencies: ["WingmanKit"]),
    ]
)
