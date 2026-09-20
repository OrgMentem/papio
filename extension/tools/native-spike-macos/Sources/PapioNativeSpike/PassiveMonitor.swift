// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import Foundation
import Darwin

// The child owns its main thread/run loop: parent readLine(), AX actions and
// model waits cannot starve it. EOF on its control pipe also ends it on parent exit.
@MainActor
final class PassiveMonitor {
    let process = Process(), input = Pipe(), output = Pipe()
    var started: [String: Any] = [:]

    init() throws {
        process.executableURL = Bundle.main.executableURL
        process.arguments = ["--passive-monitor"]
        process.standardInput = input; process.standardOutput = output
        try process.run()
        input.fileHandleForReading.closeFile(); output.fileHandleForWriting.closeFile()
        do {
            var line = Data()
            while let byte = try output.fileHandleForReading.read(upToCount: 1), !byte.isEmpty, byte != Data([10]) { line.append(byte) }
            guard let report = try JSONSerialization.jsonObject(with: line) as? [String: Any] else { throw SpikeError.unsupported }
            started = report
        } catch {
            input.fileHandleForWriting.closeFile()
            if process.isRunning { process.terminate() }
            process.waitUntilExit()
            throw error
        }
    }

    func stop() throws -> [String: Any] {
        input.fileHandleForWriting.closeFile()
        let data = output.fileHandleForReading.readDataToEndOfFile()
        output.fileHandleForReading.closeFile()
        process.waitUntilExit()
        guard process.terminationStatus == 0,
              let report = try JSONSerialization.jsonObject(with: data) as? [String: Any] else { throw SpikeError.unsupported }
        return report
    }

    static func runObserver() {
        let helper = NativeSpike(), origin = ContinuousClock.now
        var report = MonitorReport(startedAtUnixMs: Date().timeIntervalSince1970 * 1000)
        func elapsed() -> Double {
            let duration = origin.duration(to: .now).components
            return Double(duration.seconds) * 1000 + Double(duration.attoseconds) / 1e15
        }
        func emit(stopped: Bool) {
            if let data = try? JSONSerialization.data(withJSONObject: report.json(at: elapsed(), stopped: stopped), options: [.sortedKeys]) {
                FileHandle.standardOutput.write(data + Data([10]))
            }
        }
        while true {
            let began = elapsed(), status = helper.status(timeout: 0.02)
            var sample: [String: [Double]] = [:]
            if let pid = status["frontPID"] as? Int, pid != 0 {
                sample["app"] = [Double(pid)]
                if let window = status["frontWindow"] as? Int, window != 0 { sample["window"] = [Double(pid), Double(window)] }
            }
            if let x = status["pointerX"] as? Double, let y = status["pointerY"] as? Double { sample["pointer"] = [x, y] }
            let end = elapsed()
            report.add(sample, at: end, readMs: end - began)
            if report.sampleCount == 1 { emit(stopped: false) }
            var control = pollfd(fd: STDIN_FILENO, events: Int16(POLLIN | POLLHUP), revents: 0)
            // poll sleeps without blocking the *parent*; status services AppKit's
            // run loop when its workspace fallback is needed. No taps or input posts.
            if poll(&control, 1, Int32(max(1, MonitorReport.intervalMs - (elapsed() - began)))) > 0 { break }
        }
        report.interval(until: elapsed())
        emit(stopped: true)
    }
}
