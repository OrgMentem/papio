// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import Foundation
import Testing
@testable import PapioNativeSpike

struct ViewerSaveTests {
    private var input: [String: Any] {
        ["method": "viewer_prepare", "source_url": "https://example.test/paper.pdf?signature=unchanged#page=1",
         "filename": "papio-viewer-fixture_123.pdf", "expires_at_ms": Int64(2000)]
    }

    private func machine() throws -> ViewerSaveMachine {
        ViewerSaveMachine(request: try ViewerSaveRequest(input, nowMS: 1000))
    }

    private func renamed() throws -> ViewerSaveMachine {
        var machine = try machine()
        _ = try machine.next(.init(), nowMS: 1001)
        _ = try machine.next(.init(hasPanel: true, filename: "Publisher's paper (2).pdf", folder: "Elsewhere"), nowMS: 1002)
        return machine
    }

    @Test func exactURLAndLoopbackArePreserved() throws {
        let request = try ViewerSaveRequest(input, nowMS: 1000)
        #expect(request.sourceURL == input["source_url"] as? String)
        var fixture = input
        fixture["source_url"] = "http://127.0.0.1:34567/papio-native-nonce/file.pdf?a=1&a=2"
        #expect(try ViewerSaveRequest(fixture, nowMS: 1000).sourceURL == fixture["source_url"] as? String)
    }

    @Test func strictPrepareShapeAndValues() {
        let invalid: [(String, Any)] = [
            ("method", "configure"), ("source_url", "file:///tmp/paper.pdf"),
            ("source_url", "https://user:secret@example.test/paper.pdf"), ("source_url", "https:///"), // gitleaks:allow -- synthetic userinfo rejection fixture
            ("source_url", "https://example.test/paper.pdf\n"), ("source_url", "https://example.test/a b"),
            ("source_url", "https://example.test/a\\b"), ("source_url", "https://example.test/?x=\u{0085}"),
            ("source_url", "https://example.test/?x=%zz"),
            ("source_url", String(repeating: "x", count: 8193)), ("source_url", true),
            ("filename", "paper.pdf"), ("filename", "papio-viewer-.pdf"),
            ("filename", "papio-viewer-ABC.pdf"), ("filename", "papio-viewer-x.PDF"),
            ("filename", "papio-viewer-../x.pdf"), ("filename", "papio-viewer-x.pdf\n"),
            ("filename", "papio-viewer-😀.pdf"), ("filename", "papio-viewer-a b.pdf"),
            ("filename", "papio-viewer-" + String(repeating: "a", count: 112) + ".pdf"),
            ("expires_at_ms", true), ("expires_at_ms", "2000"), ("expires_at_ms", 2000.5),
            ("expires_at_ms", 1000), ("expires_at_ms", 999), ("expires_at_ms", 121001),
            ("browser", "firefox"), ("delivery", "pid-click")
        ]
        for (key, value) in invalid {
            var candidate = input
            candidate[key] = value
            #expect(throws: ViewerSaveError.invalidRequest) { try ViewerSaveRequest(candidate, nowMS: 1000) }
        }
        for key in input.keys {
            var candidate = input
            candidate.removeValue(forKey: key)
            #expect(throws: ViewerSaveError.invalidRequest) { try ViewerSaveRequest(candidate, nowMS: 1000) }
        }
    }

    @Test func maximumFilenameAndDeadline() throws {
        var candidate = input
        candidate["filename"] = "papio-viewer-" + String(repeating: "a", count: 111) + ".pdf"
        candidate["expires_at_ms"] = 121000
        let request = try ViewerSaveRequest(candidate, nowMS: 1000)
        #expect(request.filename.utf8.count == 128)
        try request.checkDeadline(120999)
        #expect(throws: ViewerSaveError.expired) { try request.checkDeadline(121000) }
    }

    @Test func capturedToolbarAndEditingVariants() {
        #expect(ViewerToolbar.validates(buttonLabels: ViewerToolbar.labels, pagination: ["of 3"], nestedWebArea: false))
        #expect(ViewerToolbar.validates(buttonLabels: ViewerToolbar.required, pagination: ["of 1"], nestedWebArea: false))
        #expect(ViewerToolbar.validates(buttonLabels: ["Sidebar"] + ViewerToolbar.required + ["New editing tool"], pagination: ["of 123"], nestedWebArea: false))
        #expect(!ViewerToolbar.validates(buttonLabels: ["Save"], pagination: ["of 3"], nestedWebArea: false))
        #expect(!ViewerToolbar.validates(buttonLabels: ViewerToolbar.labels + ["Save"], pagination: ["of 3"], nestedWebArea: false))
        #expect(!ViewerToolbar.validates(buttonLabels: ViewerToolbar.labels, pagination: ["of 3"], nestedWebArea: true))
        #expect(!ViewerToolbar.validates(buttonLabels: ViewerToolbar.labels, pagination: [], nestedWebArea: false))
        #expect(!ViewerToolbar.validates(buttonLabels: ViewerToolbar.labels, pagination: ["of 0", "of 3 documents", "of 3\n"], nestedWebArea: false))
        #expect(!ViewerToolbar.validates(buttonLabels: ViewerToolbar.required.reversed(), pagination: ["of 3"], nestedWebArea: false))
        for label in ViewerToolbar.required {
            #expect(!ViewerToolbar.validates(buttonLabels: ViewerToolbar.labels.filter { $0 != label }, pagination: ["of 3"], nestedWebArea: false))
        }
    }

    @Test func observationOnlyPrepareAndOneShotSequence() throws {
        var machine = try machine()
        #expect(machine.phase == .prepared)
        #expect(try machine.next(.init(), nowMS: 1001) == .documentSave)
        #expect(try machine.next(.init(), nowMS: 1002) == .awaitDialog)
        #expect(try machine.next(.init(), nowMS: 1003) == .awaitDialog)
        // Arbitrary initial native name, including a publisher-supplied path-like
        // string, is replaced once and never accepted as the final destination.
        #expect(try machine.next(.init(hasPanel: true, filename: "../../publisher.pdf", folder: "Documents"), nowMS: 1004) == .rename)
        let filename = machine.request.filename
        #expect(try machine.next(.init(hasPanel: true, filename: filename, folder: "Downloads", downloadsSelected: true), nowMS: 1005) == .downloads)
        #expect(try machine.next(.init(hasPanel: true, filename: filename, folder: "Documents", downloadsSelected: false), nowMS: 1006) == .awaitDownloads)
        #expect(try machine.next(.init(hasPanel: true, filename: filename, folder: "Downloads", downloadsSelected: false), nowMS: 1007) == .awaitDownloads)
        #expect(try machine.next(.init(hasPanel: true, filename: filename, folder: "Downloads", downloadsSelected: true), nowMS: 1008) == .finalSave)
        #expect(machine.phase == .saved)
        #expect(throws: ViewerSaveError.terminal) { try machine.next(.init(), nowMS: 1009) }
    }

    @Test func changedFilenamePanelOrIdentityTerminates() throws {
        for observation in [ViewerSaveMachine.Observation(hasPanel: true, filename: "papio-viewer-wrong.pdf"),
                            .init(hasPanel: false), .init(identityMatches: false, hasPanel: true)] {
            var machine = try renamed()
            #expect(throws: ViewerSaveError.changed) { try machine.next(observation, nowMS: 1010) }
            #expect(machine.phase == .failed)
            #expect(throws: ViewerSaveError.terminal) { try machine.next(.init(hasPanel: true, filename: machine.request.filename), nowMS: 1011) }
        }
        var machine = try machine()
        #expect(throws: ViewerSaveError.changed) { try machine.next(.init(hasPanel: true), nowMS: 1001) }
    }

    @Test func deadlineAndIdentityAreCheckedAtEveryPhase() throws {
        var prepared = try machine()
        var waiting = try machine()
        _ = try waiting.next(.init(), nowMS: 1001)
        var renamed = try renamed()
        var downloads = renamed
        _ = try downloads.next(.init(hasPanel: true, filename: downloads.request.filename), nowMS: 1003)
        for original in [prepared, waiting, renamed, downloads] {
            var expired = original
            #expect(throws: ViewerSaveError.expired) { try expired.next(.init(hasPanel: true, filename: expired.request.filename, folder: "Downloads", downloadsSelected: true), nowMS: 2000) }
            #expect(expired.phase == .failed)
            var changed = original
            #expect(throws: ViewerSaveError.changed) { try changed.next(.init(identityMatches: false), nowMS: 1005) }
        }
        prepared.fail()
        #expect(throws: ViewerSaveError.terminal) { try prepared.next(.init(), nowMS: 1005) }
        renamed.cancel()
        #expect(throws: ViewerSaveError.terminal) { try renamed.next(.init(), nowMS: 1005) }
    }

    @MainActor @Test func APIsCannotBeMixedAndInvalidRequestsNeverReachAX() throws {
        let viewerHelper = NativeSpike()
        #expect(throws: ViewerSaveError.invalidRequest) { try viewerHelper.request(["method": "viewer_prepare"]) }
        #expect(throws: ViewerSaveError.invalidRequest) { try viewerHelper.request(["method": "configure"]) }
        #expect(throws: ViewerSaveError.terminal) { try viewerHelper.request(["method": "viewer_prepare"]) }
        let fixtureHelper = NativeSpike()
        #expect(throws: SpikeError.invalidRequest) { try fixtureHelper.request(["method": "configure"]) }
        #expect(throws: ViewerSaveError.invalidRequest) { try fixtureHelper.request(["method": "viewer_prepare"]) }
    }

    @Test func boundedReaderHandlesPartialMultipleAndUnterminatedLines() throws {
        var chunks = [Data("{\"method\":".utf8), Data("\"viewer_cancel\"}\n{}\n".utf8), Data("last".utf8)]
        var reader = BoundedRequestReader { _ in chunks.isEmpty ? nil : chunks.removeFirst() }
        #expect(try reader.next() == Data("{\"method\":\"viewer_cancel\"}".utf8))
        #expect(try reader.next() == Data("{}".utf8))
        #expect(try reader.next() == Data("last".utf8))
        #expect(try reader.next() == nil)
        var requested = 0
        var oversized = BoundedRequestReader { count in
            requested += count
            return Data(repeating: 65, count: count)
        }
        #expect(throws: SpikeError.invalidRequest) { try oversized.next() }
        #expect(requested == 16384)
    }

    @Test func attentionRequiresCompleteEvidenceAndDetectsChanges() throws {
        let original: [String: Any] = ["focusObserved": true, "pointerObserved": true,
                                       "frontPID": 7, "frontWindow": 9, "pointerX": 20.5, "pointerY": 40.0]
        let before = try ViewerAttention(original)
        #expect(before == (try ViewerAttention(original)))
        for (key, value): (String, Any) in [("frontPID", 8), ("frontWindow", 10), ("pointerX", 21.0), ("pointerY", 41.0)] {
            var changed = original
            changed[key] = value
            #expect(before != (try ViewerAttention(changed)))
        }
        for (key, value): (String, Any) in [("focusObserved", false), ("pointerObserved", false),
                                          ("frontPID", 0), ("frontWindow", 0), ("pointerX", Double.nan)] {
            var missing = original
            missing[key] = value
            #expect(throws: ViewerSaveError.attentionUnavailable) { try ViewerAttention(missing) }
        }
    }

    @Test func realPipeProcessesOneRequestWhileWriterRemainsOpen() throws {
        let pipe = Pipe()
        defer { try? pipe.fileHandleForReading.close(); try? pipe.fileHandleForWriting.close() }
        let request = Data("{\"method\":\"viewer_cancel\"}\n".utf8)
        try pipe.fileHandleForWriting.write(contentsOf: request)
        // Close later only to free a regressed blocking reader and report a
        // normal test failure instead of hanging the suite indefinitely.
        let rescue = DispatchWorkItem { try? pipe.fileHandleForWriting.close() }
        DispatchQueue.global().asyncAfter(deadline: .now() + 1, execute: rescue)
        defer { rescue.cancel() }
        var reader = BoundedRequestReader {
            try BoundedRequestReader.readAvailable(from: pipe.fileHandleForReading.fileDescriptor, count: $0)
        }
        let start = ContinuousClock.now
        #expect(try reader.next() == Data(request.dropLast()))
        #expect(start.duration(to: .now) < .milliseconds(500))
    }

    @Test func observedNativePanelButtonsWithoutRelationsRemainBounded() {
        // 2026-09-23 held Firefox fixture panel: both relation values absent,
        // unique Save/Cancel controls have these native identifiers.
        #expect(ViewerPanelButton.accepts(identifier: "OKButton", kind: .save, relation: .absent))
        #expect(ViewerPanelButton.accepts(identifier: "CancelButton", kind: .cancel, relation: .absent))
        #expect(ViewerPanelButton.accepts(identifier: "OKButton", kind: .save, relation: .matches))
        #expect(ViewerPanelButton.accepts(identifier: "CancelButton", kind: .cancel, relation: .matches))
        // Do not turn missing relations into a generic label-only Save fallback.
        for identifier: String? in [nil, "", "Save", "downloadButton", "CancelButton"] {
            #expect(!ViewerPanelButton.accepts(identifier: identifier, kind: .save, relation: .absent))
            #expect(!ViewerPanelButton.accepts(identifier: identifier, kind: .save, relation: .matches))
        }
        #expect(!ViewerPanelButton.accepts(identifier: "OKButton", kind: .save, relation: .conflicts))
        #expect(!ViewerPanelButton.accepts(identifier: "CancelButton", kind: .cancel, relation: .conflicts))
        #expect(!ViewerPanelButton.accepts(identifier: "OKButton", kind: .cancel, relation: .absent))
    }
}
