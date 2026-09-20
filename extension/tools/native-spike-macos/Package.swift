// swift-tools-version: 6.2
import PackageDescription

let package = Package(
    name: "PapioNativeSpike",
    platforms: [.macOS(.v14)],
    products: [.executable(name: "papio-native-spike", targets: ["PapioNativeSpike"])],
    dependencies: [.package(url: "https://github.com/openclaw/AXorcist.git", revision: "4bc531de8c36bf0e4740ab1b1a3a026b3c6a5eb9")],
    targets: [.executableTarget(name: "PapioNativeSpike", dependencies: [.product(name: "AXorcist", package: "AXorcist")])]
)
