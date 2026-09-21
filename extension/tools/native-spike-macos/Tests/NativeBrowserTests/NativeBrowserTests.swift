// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import Foundation
import Testing
@testable import PapioNativeSpike

// Configuration is pure; these tests never call request/status/observe/act or AX.
struct NativeBrowserTests {
    @Test func exactBrowserMappingAndDefault() throws {
        #expect(try NativeBrowser.configured(nil, surfaceBound: false) == .chrome)
        #expect(try NativeBrowser.configured("chrome", surfaceBound: false).bundleIdentifier == "com.google.Chrome")
        #expect(try NativeBrowser.configured("firefox", surfaceBound: false).bundleIdentifier == "org.mozilla.firefox")
    }

    @Test(arguments: ["", "Chrome", "Firefox", " firefox", "firefox ", "safari",
                      "com.google.Chrome", "org.mozilla.firefox", "org.mozilla.firefoxdeveloperedition",
                      "http://127.0.0.1/papio-native-test"])
    func rejectsOtherStrings(_ value: String) {
        #expect(throws: SpikeError.invalidRequest) {
            try NativeBrowser.configured(value, surfaceBound: false)
        }
    }

    @Test func rejectsNonStringsInsteadOfDefaulting() {
        for value: Any in [NSNull(), true, 1, ["firefox"], ["browser": "firefox"]] {
            #expect(throws: SpikeError.invalidRequest) {
                try NativeBrowser.configured(value, surfaceBound: false)
            }
        }
    }

    @Test func rejectsEveryReconfigurationAfterBinding() {
        for value: Any? in [nil, "chrome", "firefox", "safari", NSNull()] {
            #expect(throws: SpikeError.stale) {
                try NativeBrowser.configured(value, surfaceBound: true)
            }
        }
    }

    private var fixture: [String: Any] {
        ["prefix": "http://127.0.0.1:34567/papio-native-test-nonce", "goal": "Save fixture PDF"]
    }

    @MainActor @Test func legacyChromeDefaultsAndPrebindingSelection() throws {
        let helper = NativeSpike()
        try helper.configure(fixture)
        #expect(helper.browser.bundleIdentifier == "com.google.Chrome")
        #expect(helper.prefix == fixture["prefix"] as? String)
        #expect(helper.goal == "Save fixture PDF")
        #expect(helper.delivery == "ax" && helper.attention == "background")
        #expect(helper.window == nil && helper.targets.isEmpty && helper.revision.isEmpty)
        #expect(!helper.nativeDialog)

        var input = fixture
        input["browser"] = "firefox"
        try helper.configure(input)
        #expect(helper.browser.bundleIdentifier == "org.mozilla.firefox")
        try helper.configure(fixture) // Omitted browser always defaults to Chrome.
        #expect(helper.browser == .chrome)
    }

    @MainActor @Test func invalidConfigurationDoesNotPartiallyChangeRuntime() throws {
        let helper = NativeSpike()
        var initial = fixture
        initial["browser"] = "firefox"
        initial["delivery"] = "cua-window"
        initial["attention"] = "owned"
        try helper.configure(initial)

        let invalidFields: [(String, Any)] = [
            ("browser", "safari"), ("browser", NSNull()),
            ("prefix", "https://127.0.0.1/papio-native-test"),
            ("prefix", "http://localhost/papio-native-test"),
            ("prefix", "http://example.com/papio-native-test"),
            ("prefix", "http://127.0.0.1/not-the-fixture"),
            ("goal", String(repeating: "x", count: 1001)),
            ("delivery", "global"), ("attention", "activate")
        ]
        for (key, value) in invalidFields {
            var input = fixture
            input[key] = value
            #expect(throws: SpikeError.invalidRequest) { try helper.configure(input) }
            #expect(helper.browser == .firefox)
            #expect(helper.prefix == initial["prefix"] as? String)
            #expect(helper.goal == "Save fixture PDF")
            #expect(helper.delivery == "cua-window" && helper.attention == "owned")
        }
    }
}
