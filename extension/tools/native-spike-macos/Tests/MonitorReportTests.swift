// Standalone: swiftc Sources/PapioNativeSpike/MonitorReport.swift Tests/MonitorReportTests.swift -o <scratch>/monitor-report-tests
import Foundation

@main struct MonitorReportTests {
    static func main() {
        let a = ["app": [1.0], "window": [1.0, 10], "pointer": [20.0, 30]]
        let b = ["app": [2.0], "window": [2.0, 11], "pointer": [21.0, 30]]
        var report = MonitorReport(startedAtUnixMs: 1000)
        report.add(a, at: 5, readMs: 5)
        report.add(b, at: 30, readMs: 2)
        report.add(a, at: 55, readMs: 3) // Endpoints alone would miss both transitions.
        assert(report.sampleCount == 3 && report.changeCounts.values.allSatisfy { $0 == 2 })
        assert(report.first == report.last && report.firstMs == 5 && report.lastMs == 55)
        report.add([:], at: 155, readMs: 80)
        report.add(b, at: 180, readMs: 2)
        assert(report.changeCounts.values.allSatisfy { $0 == 2 }) // Unknown is not unchanged.
        assert(report.missingSamples.values.allSatisfy { $0 == 1 })
        assert(report.gapCount == 1 && report.maxIntervalMs == 100 && report.maxReadMs == 80)
        report.interval(until: 280) // Include an unobserved tail at shutdown.
        assert(report.gapCount == 2)
        var bounded = MonitorReport(startedAtUnixMs: 0)
        for i in 0..<150 { bounded.add(i % 2 == 0 ? a : b, at: Double(i * 100 + 100), readMs: 1) }
        let json = bounded.json(at: 15000, stopped: true)
        assert(bounded.changeCounts["pointer"] == 149 && bounded.gapCount == 150)
        assert(bounded.changes.count == 64 && bounded.gaps.count == 64)
        assert(json["changesTruncated"] as? Bool == true && json["gapsTruncated"] as? Bool == true)
        assert(JSONSerialization.isValidJSONObject(json))
        print("MonitorReport tests passed")
    }
}
