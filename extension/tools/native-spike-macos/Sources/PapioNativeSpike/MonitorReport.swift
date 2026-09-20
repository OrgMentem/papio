// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import Foundation

// Pure accumulation; absent channels never count as observed transitions.
struct MonitorReport {
    var sampleCount = 0, changeEvents = 0, gapCount = 0
    var changeCounts = ["app": 0, "window": 0, "pointer": 0], missingSamples = ["app": 0, "window": 0, "pointer": 0]
    var first: [String: [Double]] = [:], last: [String: [Double]] = [:]
    var changes: [[String: Any]] = [], gaps: [[String: Double]] = []
    var firstMs: Double = 0, lastMs: Double = 0, maxIntervalMs: Double = 0, maxReadMs: Double = 0
    let startedAtUnixMs: Double
    static let intervalMs: Double = 25, gapThresholdMs: Double = 75, limit = 64

    mutating func interval(until ms: Double) {
        let duration = ms - lastMs
        maxIntervalMs = max(maxIntervalMs, duration)
        if duration > Self.gapThresholdMs {
            gapCount += 1
            if gaps.count < Self.limit { gaps.append(["fromMs": lastMs, "toMs": ms, "durationMs": duration]) }
        }
    }

    mutating func add(_ sample: [String: [Double]], at ms: Double, readMs: Double) {
        interval(until: ms)
        maxReadMs = max(maxReadMs, readMs)
        var changed: [String] = []
        for key in ["app", "window", "pointer"] {
            if sample[key] == nil { missingSamples[key, default: 0] += 1 }
            if let before = last[key], let after = sample[key], before != after {
                changeCounts[key, default: 0] += 1
                changed.append(key)
            }
        }
        if !changed.isEmpty {
            changeEvents += 1
            if changes.count < Self.limit { changes.append(["atMs": ms, "channels": changed, "before": last, "after": sample]) }
        }
        if sampleCount == 0 { first = sample; firstMs = ms }
        sampleCount += 1
        last = sample; lastMs = ms
    }

    func json(at ms: Double, stopped: Bool) -> [String: Any] {
        ["status": stopped ? "stopped" : "monitoring", "coverage": "polling; fast transitions may be missed; channel reads are not atomic",
         "intervalMs": Self.intervalMs, "gapThresholdMs": Self.gapThresholdMs,
         "startedAtUnixMs": startedAtUnixMs, "elapsedMs": ms, "sampleCount": sampleCount,
         "firstSampleMs": firstMs, "lastSampleMs": lastMs, "first": first, "last": last,
         "changeCounts": changeCounts, "missingSamples": missingSamples, "changes": changes,
         "changesTruncated": changeEvents > changes.count, "gapCount": gapCount, "gaps": gaps,
         "gapsTruncated": gapCount > gaps.count, "maxIntervalMs": maxIntervalMs, "maxReadMs": maxReadMs,
         "reportedAtUnixMs": Date().timeIntervalSince1970 * 1000]
    }
}
