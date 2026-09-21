// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import CryptoKit
import Foundation
import Testing
@testable import PapioNativeSpike

// Exercise only the projection of a chosen root, with no live AX observation.
struct NativeSurfaceTests {
    private var page: [String: Any] {
        ["url": "http://127.0.0.1:34567/papio-native-test/paper.pdf", "title": "Fixture", "text": "Save"]
    }
    private var controls: [[String: Any]] {
        [["id": "c1", "role": "AXButton", "label": "Save", "disabled": false]]
    }

    @MainActor @Test func sameSaveLabelOnDifferentRootsChangesRevision() throws {
        let helper = NativeSpike()
        let document = try helper.projectObservation(page: page, controls: controls)
        let documentRevision = helper.revision
        #expect(document["native_surface"] as? String == "document")

        helper.nativeDialog = true // Existing observe branch selected an AXSheet.
        let dialog = try helper.projectObservation(page: page, controls: controls)
        #expect(dialog["native_surface"] as? String == "save-dialog")
        #expect(helper.revision != documentRevision)
        #expect((dialog["provenance"] as? [String: String])?["revision"] == helper.revision)

        helper.nativeDialog = false
        _ = try helper.projectObservation(page: page, controls: controls)
        #expect(helper.revision == documentRevision)
    }

    @MainActor @Test func revisionUsesCanonicalPageControlsAndSurface() throws {
        let helper = NativeSpike()
        helper.goal = "Save fixture PDF"
        let observation = try helper.projectObservation(page: page, controls: controls)
        let state: [String: Any] = ["native_surface": "document", "controls": controls, "page": page]
        let encoded = try JSONSerialization.data(withJSONObject: state, options: [.sortedKeys])
        let expected = SHA256.hash(data: encoded).map { String(format: "%02x", $0) }.joined()
        #expect(helper.revision == expected)
        #expect(observation["goal"] as? String == helper.goal)
        #expect((observation["provenance"] as? [String: String])?["kind"] == "native-axorcist")

        let reorderedPage: [String: Any] = ["text": "Save", "title": "Fixture", "url": page["url"]!]
        helper.goal = "Another goal" // Goal remains outside the AX-state revision.
        _ = try helper.projectObservation(page: reorderedPage, controls: controls)
        #expect(helper.revision == expected)
    }
}
