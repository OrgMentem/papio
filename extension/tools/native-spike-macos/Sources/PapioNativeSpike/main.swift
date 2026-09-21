// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Development-only AXorcist evaluation. Only operates on the nonce-scoped local
// fixture; never activates an app, posts global input, or opens a debugger.
import AppKit
import ApplicationServices
import AXorcist
import CryptoKit
import Foundation

enum SpikeError: Error, Equatable {
    case invalidRequest, permissionMissing, missingSurface, ambiguousSurface, stale, unsupported
    case saveBinding(String), nativeAction(Int32)
}

@MainActor
final class NativeSpike {
    private(set) var browser = NativeBrowser.chrome
    var window: Element?
    var targets: [String: Element] = [:]
    var revision = ""
    var prefix = ""
    var goal = ""
    var delivery = "ax"
    var nativeDialog = false
    var saveRequested = false
    var savePanel: Element?
    var targetActions: [String: String] = [:]
    var attention = "background"
    var monitor: PassiveMonitor?

    func status(timeout: Float? = nil) -> [String: Any] {
        // Query the AX server directly: NSWorkspace notifications need a run
        // loop and may otherwise leave a long-lived command helper with stale
        // foreground state.
        var focusedApplication: CFTypeRef?
        var frontPID: pid_t = 0
        let system = AXUIElementCreateSystemWide()
        if let timeout { AXUIElementSetMessagingTimeout(system, timeout) }
        let focusError = AXUIElementCopyAttributeValue(system, kAXFocusedApplicationAttribute as CFString, &focusedApplication)
        if focusError == .success,
           let focusedApplication, CFGetTypeID(focusedApplication) == AXUIElementGetTypeID() {
            AXUIElementGetPid(focusedApplication as! AXUIElement, &frontPID)
        }
        var focusSource = "system-wide-AX"
        if frontPID == 0 {
            // Some desktops return cannotComplete for the system-wide focus
            // attribute while per-app AX works. Service AppKit notifications
            // before consulting its supported foreground-app API.
            RunLoop.current.run(until: Date(timeIntervalSinceNow: 0.01))
            frontPID = NSWorkspace.shared.frontmostApplication?.processIdentifier ?? 0
            focusSource = "workspace-with-runloop"
        }
        let pointer = CGEvent(source: nil)?.location
        var focusedWindow: CFTypeRef?
        if frontPID != 0 {
            let app = AXUIElementCreateApplication(frontPID)
            if let timeout { AXUIElementSetMessagingTimeout(app, timeout) }
            AXUIElementCopyAttributeValue(app, kAXFocusedWindowAttribute as CFString, &focusedWindow)
        }
        var windowID: CGWindowID = 0
        if let focusedWindow, CFGetTypeID(focusedWindow) == AXUIElementGetTypeID() {
            if let timeout { AXUIElementSetMessagingTimeout(focusedWindow as! AXUIElement, timeout) }
            windowID = AXWindowResolver().windowID(from: focusedWindow as! AXUIElement) ?? 0
        }
        return ["trusted": AXIsProcessTrusted(), "frontPID": Int(frontPID), "focusObserved": frontPID != 0 && windowID != 0,
                "focusSource": focusSource, "globalFocusError": focusError.rawValue,
                "frontWindow": Int(windowID), "pointerObserved": pointer != nil,
                "pointerX": pointer.map { Double($0.x) } as Any? ?? NSNull(), "pointerY": pointer.map { Double($0.y) } as Any? ?? NSNull(), "backend": "AXorcist-native-only"]
    }

    // Bounded traversal is observation coverage, never an entitlement verdict.
    func walk(_ root: Element, limit: Int = 5000, omitFileListings: Bool = false) -> [Element] {
        var queue = [root], result: [Element] = [], seen = Set<Element>(), offset = 0
        while offset < queue.count && result.count < limit {
            let element = queue[offset]; offset += 1
            guard seen.insert(element).inserted else { continue }
            result.append(element)
            if omitFileListings, ["AXOutline", "AXTable", "AXBrowser"].contains(element.role() ?? ""),
               element.descriptionText() != "sidebar" { continue }
            queue.append(contentsOf: element.children() ?? [])
        }
        return result
    }

    func configure(_ input: [String: Any]) throws {
        let browser = try NativeBrowser.configured(input["browser"], surfaceBound: window != nil || !targets.isEmpty)
        guard let raw = input["prefix"] as? String, let url = URL(string: raw),
              url.scheme == "http", url.host == "127.0.0.1", url.path.hasPrefix("/papio-native-"),
              let goal = input["goal"] as? String, goal.count <= 1000 else { throw SpikeError.invalidRequest }
        let delivery = input["delivery"] as? String ?? "ax"
        guard ["ax", "pid-click", "cua-window"].contains(delivery) else { throw SpikeError.invalidRequest }
        let attention = input["attention"] as? String ?? "background"
        guard ["background", "owned"].contains(attention) else { throw SpikeError.invalidRequest }
        self.browser = browser
        self.prefix = raw
        self.goal = goal
        self.delivery = delivery
        self.attention = attention
    }

    func observe() throws -> [String: Any] {
        guard AXIsProcessTrusted() else { throw SpikeError.permissionMissing }
        guard !prefix.isEmpty,
              let application = NSRunningApplication.runningApplications(withBundleIdentifier: browser.bundleIdentifier).first
        else { throw SpikeError.missingSurface }
        let app = Element(AXUIElementCreateApplication(application.processIdentifier))
        AXUIElementSetMessagingTimeout(app.underlyingElement, 1)
        let windows = app.windows() ?? []
        var matches: [(Element, Element)] = []
        for candidate in windows {
            // Ignore other windows' contents after the first matching web area.
            let areas = walk(candidate).filter { $0.role() == "AXWebArea" && ($0.url()?.absoluteString.hasPrefix(prefix + "/") ?? false) }
            if let area = areas.first { matches.append((candidate, area)) }
        }
        guard matches.count == 1, let (boundWindow, area) = matches.first else {
            if matches.count > 1 { throw SpikeError.ambiguousSurface }
            throw SpikeError.missingSurface
        }
        if let window, window != boundWindow { throw SpikeError.saveBinding("document_window_changed") }
        window = boundWindow
        var sheets = walk(boundWindow, limit: 500).filter { $0.role() == "AXSheet" }
        // Firefox exposes its attached NSSavePanel through AXFocusedWindow,
        // while omitting it from the document window's AXChildren.
        if let focused = app.focusedWindow(), focused.role() == "AXSheet",
           focused.parent() == boundWindow, !sheets.contains(focused) {
            sheets.append(focused)
        }
        guard sheets.count <= 1 else { throw SpikeError.ambiguousSurface }
        if let sheet = sheets.first {
            guard saveRequested else { throw SpikeError.saveBinding("save_not_requested") }
            guard sheet.identifier() == "save-panel", sheet.parent() == boundWindow else { throw SpikeError.saveBinding("panel_not_attached") }
            if let savePanel, savePanel != sheet { throw SpikeError.saveBinding("panel_replaced") }
            savePanel = sheet
        } else if savePanel != nil { throw SpikeError.saveBinding("panel_disappeared") }
        let root = sheets.first ?? area
        nativeDialog = !sheets.isEmpty
        let nodes = walk(root, omitFileListings: nativeDialog)
        var controls: [[String: Any]] = [], context: [String] = [], nextTargets: [String: Element] = [:]
        var nextActions: [String: String] = [:]
        var destinationReady = true
        if nativeDialog {
            // Only inspect the two fixture-save fields, never account inputs.
            let names = nodes.filter { $0.identifier() == "saveAsNameTextField" }
            let locations = nodes.filter { $0.identifier() == "where popup" }
            let expectedName = (URL(string: prefix)?.lastPathComponent ?? "") + ".pdf"
            guard names.count == 1 else { throw SpikeError.saveBinding("filename_field_count") }
            // Avoid AXorcist's generic Any-valued accessor here: on the observed
            // NSSavePanel it lost a string that the raw AX attribute preserved.
            guard names[0].rawAttributeValue(named: "AXValue") as? String == expectedName else { throw SpikeError.saveBinding("filename_changed") }
            guard locations.count == 1, let location = locations[0].rawAttributeValue(named: "AXValue") as? String else { throw SpikeError.saveBinding("folder_field_unavailable") }
            context.append("Fixture filename: \(expectedName)\nSave folder: \(location)")
            destinationReady = location == "Downloads"
            if !destinationReady {
                // This is a fixture-only directory choice. Final proof still
                // requires the exact file under the runner's Downloads path.
                let cells = nodes.filter { element in
                    guard element.role() == "AXCell",
                          let row = element.parent(), row.role() == "AXRow",
                          row.isAttributeSettable(named: "AXSelected"),
                          let outline = row.parent(), outline.role() == "AXOutline",
                          outline.descriptionText() == "sidebar" else { return false }
                    return walk(element, limit: 20).contains { $0.role() == "AXStaticText" && $0.rawAttributeValue(named: "AXValue") as? String == "Downloads" }
                }
                guard cells.count == 1 else { throw SpikeError.ambiguousSurface }
                controls.append(["id": "c1", "role": "AXButton", "label": "Choose Downloads", "disabled": false])
                nextTargets["c1"] = cells[0].parent()
                nextActions["c1"] = "set-selected"
            }
        }
        for element in nodes {
            let role = element.role() ?? ""
            // Editable reads above are limited to the owned fixture save name.
            let label = [element.title(), element.descriptionText()].compactMap { $0 }.first { !$0.isEmpty } ?? ""
            if !nativeDialog && ["AXStaticText", "AXHeading"].contains(role) {
                let text = label.isEmpty ? (element.rawAttributeValue(named: "AXValue") as? String ?? "") : label
                if !text.isEmpty { context.append(String(text.prefix(300))) }
            }
            // Do not project unrelated filenames or other save-panel controls.
            if nativeDialog && !["Save", "Cancel"].contains(label) { continue }
            if ["AXLink", "AXButton"].contains(role), !label.isEmpty, element.isActionSupported("AXPress"), controls.count < 80 {
                let id = "c\(controls.count + 1)"
                controls.append(["id": id, "role": role, "label": String(label.prefix(240)), "disabled": element.isEnabled() == false || (nativeDialog && label == "Save" && !destinationReady)])
                nextTargets[id] = element
                if !nativeDialog || label != "Save" || destinationReady { nextActions[id] = "AXPress" }
            }
        }
        let page: [String: Any] = ["url": area.url()?.absoluteString ?? prefix,
            "title": area.title() ?? "Papio native fixture", "text": String(context.joined(separator: "\n").prefix(12000))]
        let observation = try projectObservation(page: page, controls: controls)
        targets = nextTargets
        targetActions = nextActions
        return observation
    }

    // Pure projection of the already-chosen AX root; labels never select a surface.
    func projectObservation(page: [String: Any], controls: [[String: Any]]) throws -> [String: Any] {
        let surface = nativeDialog ? "save-dialog" : "document"
        let state: [String: Any] = ["page": page, "controls": controls, "native_surface": surface]
        let encoded = try JSONSerialization.data(withJSONObject: state, options: [.sortedKeys])
        revision = SHA256.hash(data: encoded).map { String(format: "%02x", $0) }.joined()
        return ["goal": goal, "page": page, "controls": controls, "native_surface": surface,
                "provenance": ["kind": "native-axorcist", "revision": revision]]
    }

    func act(_ input: [String: Any]) throws -> [String: Any] {
        guard let id = input["choice"] as? String, let expected = input["revision"] as? String,
              expected == revision, let old = targets[id] else { throw SpikeError.stale }
        let before = status()
        guard before["focusObserved"] as? Bool == true, before["pointerObserved"] as? Bool == true else { throw SpikeError.unsupported }
        _ = try observe()
        guard revision == expected, let current = targets[id], current == old,
              let action = targetActions[id], current.isEnabled() != false,
              action == "set-selected" ? current.isAttributeSettable(named: "AXSelected") : current.isActionSupported(action) else { throw SpikeError.stale }
        let dispatchedLabel = [current.title(), current.descriptionText()].compactMap { $0 }.first { !$0.isEmpty } ?? ""
        let beforeOwnedIDs = Set(((window.map { walk($0, limit: 500).filter { ["AXWindow", "AXSheet"].contains($0.role() ?? "") } } ?? []) + (savePanel.map { [$0] } ?? []))
            .compactMap { AXWindowResolver().windowID(from: $0).map(Int.init) })
        if attention == "owned" && (before["frontPID"] as? Int != window?.pid().map(Int.init) || !beforeOwnedIDs.contains(before["frontWindow"] as? Int ?? -1)) {
            return ["status": "needs_foreground", "focusChanged": false, "pointerMoved": false]
        }
        let actualDelivery = nativeDialog ? "ax-native-dialog" : delivery
        if delivery == "ax" || nativeDialog {
            // Perform only the retained accessibility action. No input fallback.
            if action == "set-selected" {
                let result = AXUIElementSetAttributeValue(current.underlyingElement, kAXSelectedAttribute as CFString, kCFBooleanTrue)
                guard result == .success else { throw SpikeError.nativeAction(result.rawValue) }
            } else {
                try current.performAction(action)
            }
        } else {
            guard let frame = current.frame(), let window, let windowFrame = window.frame(),
                  frame.width > 4, frame.height > 4, windowFrame.contains(CGPoint(x: frame.midX, y: frame.midY)),
                  let pid = window.pid(), let wid = AXWindowResolver().windowID(from: window),
                  let source = CGEventSource(stateID: .hidSystemState) else { throw SpikeError.unsupported }
            // Native PID/window routing follows trycua's MIT-licensed public
            // post path at 9bbfa7dd, input/mouse.rs:post_mouse_event_with_mode.
            // The public variant excludes activation, SkyLight, global HID
            // delivery and Chromium's primer. cua-window separately evaluates
            // upstream's private event stream without its focus mutation.
            let point = CGPoint(x: frame.midX, y: frame.midY)
            if delivery == "cua-window" {
                try CuaWindowClick.click(pid: pid, windowID: wid, point: point, localPoint: CGPoint(x: point.x - windowFrame.minX, y: point.y - windowFrame.minY))
            } else {
                let group = Int64(Date().timeIntervalSince1970 * 1000000)
                for type in [CGEventType.mouseMoved, .leftMouseDown, .leftMouseUp] {
                    guard let event = CGEvent(mouseEventSource: source, mouseType: type, mouseCursorPosition: point, mouseButton: .left) else { throw SpikeError.unsupported }
                    for (field, value) in [(UInt32(1), Int64(type == .mouseMoved ? 0 : 1)), (3, 0), (7, 3), (40, Int64(pid)), (51, Int64(wid)), (58, group), (91, Int64(wid)), (92, Int64(wid))] {
                        if let key = CGEventField(rawValue: field) { event.setIntegerValueField(key, value: value) }
                    }
                    event.postToPid(pid)
                    Thread.sleep(forTimeInterval: type == .leftMouseDown ? 0.028 : 0.012)
                }
            }
        }
        if !nativeDialog, ["Save", "Download"].contains(dispatchedLabel) {
            saveRequested = true
        }
        Thread.sleep(forTimeInterval: 0.2)
        let after = status()
        guard after["focusObserved"] as? Bool == true, after["pointerObserved"] as? Bool == true else { throw SpikeError.unsupported }
        let owned = (window.map { walk($0, limit: 500).filter { ["AXWindow", "AXSheet"].contains($0.role() ?? "") } } ?? []) + (savePanel.map { [$0] } ?? [])
        let ownedIDs = Set(owned.compactMap { AXWindowResolver().windowID(from: $0).map(Int.init) })
        let ownedPID = window?.pid().map(Int.init)
        let staysOwned = before["frontPID"] as? Int == ownedPID && after["frontPID"] as? Int == ownedPID
            && beforeOwnedIDs.contains(before["frontWindow"] as? Int ?? -1) && ownedIDs.contains(after["frontWindow"] as? Int ?? -1)
        return ["status": "dispatched",
                "delivery": actualDelivery, "nativeAction": action, "focusWithinOwnedSurface": staysOwned,
                "focusChanged": before["frontPID"] as? Int != after["frontPID"] as? Int || before["frontWindow"] as? Int != after["frontWindow"] as? Int,
                "pointerMoved": before["pointerX"] as? Double != after["pointerX"] as? Double || before["pointerY"] as? Double != after["pointerY"] as? Double,
                "before": before, "after": after]
    }

    func request(_ input: [String: Any]) throws -> [String: Any] {
        switch input["method"] as? String {
        case "start_monitor":
            guard monitor == nil else { throw SpikeError.invalidRequest }
            let observer = try PassiveMonitor()
            monitor = observer
            return observer.started
        case "stop_monitor":
            guard let monitor else { throw SpikeError.invalidRequest }
            defer { self.monitor = nil }
            return try monitor.stop()
        case "status": return status()
        case "configure":
            try configure(input)
            return status()
        case "observe": return try observe()
        case "act": return try act(input)
        default: throw SpikeError.invalidRequest
        }
    }
}

@main struct Main {
    @MainActor static func main() {
        if CommandLine.arguments.dropFirst().first == "--passive-monitor" {
            PassiveMonitor.runObserver()
            return
        }
        let helper = NativeSpike()
        defer { _ = try? helper.monitor?.stop() }
        while let line = readLine() {
            var response: [String: Any]
            do {
                guard line.utf8.count < 16384, let data = line.data(using: .utf8),
                      let input = try JSONSerialization.jsonObject(with: data) as? [String: Any] else { throw SpikeError.invalidRequest }
                response = ["ok": true, "result": try helper.request(input)]
            } catch { response = ["ok": false, "error": String(describing: error)] }
            let encoded = (try? JSONSerialization.data(withJSONObject: response, options: [.sortedKeys])) ?? Data("{\"ok\":false}".utf8)
            FileHandle.standardOutput.write(encoded + Data([10]))
        }
    }
}
