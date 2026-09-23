// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
import AppKit
import ApplicationServices
import AXorcist
import Foundation

enum ViewerSaveError: String, Error {
    case invalidRequest = "viewer_invalid_request"
    case expired = "viewer_expired"
    case unavailable = "viewer_unavailable"
    case ambiguous = "viewer_ambiguous"
    case changed = "viewer_changed"
    case unsupported = "viewer_unsupported"
    case actionFailed = "viewer_action_failed"
    case terminal = "viewer_terminal"
    case applicationChanged = "viewer_application_changed"
    case documentChanged = "viewer_document_changed"
    case saveControlChanged = "viewer_save_control_changed"
    case attentionUnavailable = "viewer_attention_unavailable"
    case attentionChanged = "viewer_attention_changed"
}

struct ViewerSaveRequest {
    let sourceURL: String
    let filename: String
    let expiresAtMS: Int64

    init(_ input: [String: Any], nowMS: Int64) throws {
        guard Set(input.keys) == ["method", "source_url", "filename", "expires_at_ms"],
              input["method"] as? String == "viewer_prepare",
              let source = input["source_url"] as? String, source.utf8.count <= 8192,
              !source.contains("\\"), !source.unicodeScalars.contains(where: { $0.value <= 32 || (127...159).contains($0.value) }),
              source.range(of: #"%(?![a-fA-F0-9]{2})"#, options: .regularExpression) == nil,
              let url = URLComponents(string: source), ["http", "https"].contains(url.scheme ?? ""),
              let host = url.host, !host.isEmpty, url.user == nil, url.password == nil,
              let filename = input["filename"] as? String, filename.utf8.count <= 128,
              filename.range(of: #"\Apapio-viewer-[a-z0-9_-]+\.pdf\z"#, options: .regularExpression) != nil,
              let expiry = input["expires_at_ms"] as? NSNumber,
              CFGetTypeID(expiry) != CFBooleanGetTypeID(),
              expiry.doubleValue.isFinite, expiry.doubleValue == Double(expiry.int64Value),
              expiry.int64Value > nowMS, expiry.int64Value <= nowMS + 120_000
        else { throw ViewerSaveError.invalidRequest }
        self.sourceURL = source
        self.filename = filename
        self.expiresAtMS = expiry.int64Value
    }

    func checkDeadline(_ nowMS: Int64) throws {
        guard nowMS < expiresAtMS else { throw ViewerSaveError.expired }
    }
}

// Captured Firefox resident-viewer signature (2026-09-21). These roles and the
// ordered labels are observed, not guessed DOM identifiers. A lone webpage Save
// or a partial imitation is refused. AX does NOT attest a privileged PDF.js
// principal: hostile HTML can reproduce the entire signature. This is a bounded
// native affordance check, not proof of MIME, byte identity, or entitlement.
struct ViewerToolbar {
    // The editing/sidebar controls vary with PDF.js version and mode. Require
    // the captured navigation/zoom/print/Save cluster, preserving its order and
    // uniqueness, without treating the total button count as authentication.
    static let required = ["Previous", "Next", "Zoom Out", "Zoom In", "Print", "Save"]
    static let labels = ["Manage pages", "Previous", "Next", "Zoom Out", "Zoom In", "Print", "Save",
                         "Tools", "Comment", "Add signature", "Highlight", "Text", "Draw", "Add or edit images"]

    static func validates(buttonLabels: [String], pagination: [String], nestedWebArea: Bool) -> Bool {
        !nestedWebArea && buttonLabels.filter { required.contains($0) } == required && pagination.contains {
            $0.range(of: #"\Aof [1-9][0-9]*\z"#, options: .regularExpression) != nil
        }
    }
}

// Endpoint observations stop subsequent effects when attention changed. They
// cannot prove there was no transient change between samples, attribute a change
// to this helper, or promise takeover detection; the acceptance monitor measures
// that interval independently. Never activate an app or restore user focus.
struct ViewerAttention: Equatable {
    let pid: Int
    let window: Int
    let x: Double
    let y: Double

    init(_ snapshot: [String: Any]) throws {
        guard snapshot["focusObserved"] as? Bool == true,
              snapshot["pointerObserved"] as? Bool == true,
              let pid = snapshot["frontPID"] as? Int, pid > 0,
              let window = snapshot["frontWindow"] as? Int, window > 0,
              let x = snapshot["pointerX"] as? Double, x.isFinite,
              let y = snapshot["pointerY"] as? Double, y.isFinite else { throw ViewerSaveError.attentionUnavailable }
        self.pid = pid; self.window = window; self.x = x; self.y = y
    }
}

// The observed Firefox NSSavePanel advertises AXDefaultButton/AXCancelButton
// but returns no value for either. Native button identifiers are present. A
// missing relation is compatible with that capture; a conflicting one is not.
struct ViewerPanelButton {
    enum Kind: String { case save = "OKButton", cancel = "CancelButton" }
    enum Relation { case absent, matches, conflicts }

    static func accepts(identifier: String?, kind: Kind, relation: Relation) -> Bool {
        identifier == kind.rawValue && relation != .conflicts
    }
}

// Pure effect ledger. An effect is consumed BEFORE attempting AX delivery;
// uncertainty or an AX error can never turn into a replay of that effect.
struct ViewerSaveMachine {
    enum Phase { case prepared, documentSave, renamed, downloads, saved, failed, cancelled }
    enum Effect: String {
        case documentSave = "document_save", awaitDialog = "awaiting_dialog", rename
        case downloads, awaitDownloads = "awaiting_downloads", finalSave = "final_save"
    }
    struct Observation {
        var identityMatches = true
        var hasPanel = false
        var filename = ""
        var folder = ""
        var downloadsSelected = false
    }
    let request: ViewerSaveRequest
    private(set) var phase = Phase.prepared

    mutating func next(_ observation: Observation, nowMS: Int64) throws -> Effect {
        do {
            try request.checkDeadline(nowMS)
            guard observation.identityMatches else { throw ViewerSaveError.changed }
            switch phase {
            case .prepared:
                guard !observation.hasPanel else { throw ViewerSaveError.changed }
                phase = .documentSave
                return .documentSave
            case .documentSave:
                guard observation.hasPanel else { return .awaitDialog }
                // The native panel's initial filename is deliberately arbitrary.
                phase = .renamed
                return .rename
            case .renamed:
                guard observation.hasPanel, observation.filename == request.filename else { throw ViewerSaveError.changed }
                phase = .downloads
                return .downloads
            case .downloads:
                guard observation.hasPanel, observation.filename == request.filename else { throw ViewerSaveError.changed }
                guard observation.folder == "Downloads", observation.downloadsSelected else { return .awaitDownloads }
                phase = .saved
                return .finalSave
            case .saved, .failed, .cancelled: throw ViewerSaveError.terminal
            }
        } catch {
            phase = .failed
            throw error
        }
    }

    mutating func fail() { phase = .failed }
    mutating func cancel() { phase = .cancelled }
}

@MainActor
final class NativeViewerSave {
    private var attempted = false
    private var machine: ViewerSaveMachine?
    private var application: NSRunningApplication?
    private var launchDate: Date?
    private var app: Element?
    private var window: Element?
    private var area: Element?
    private var documentSave: Element?
    private var panel: Element?
    private var nameField: Element?
    private var whereField: Element?
    private var downloadsRow: Element?
    private var saveButton: Element?
    private var cancelButton: Element?
    private var cancelAttempted = false
    private var finalSaveAttempted = false

    private var nowMS: Int64 { Int64(Date().timeIntervalSince1970 * 1000) }

    func request(_ input: [String: Any]) throws -> [String: Any] {
        do {
            switch input["method"] as? String {
            case "viewer_prepare": return try prepare(input)
            case "viewer_advance":
                guard Set(input.keys) == ["method"] else { throw ViewerSaveError.invalidRequest }
                return try advance()
            case "viewer_cancel":
                guard Set(input.keys) == ["method"] else { throw ViewerSaveError.invalidRequest }
                cancelRetainedPanel()
                return ["status": "cancelled", "step": "cancelled"]
            default: throw ViewerSaveError.invalidRequest
            }
        } catch {
            machine?.fail()
            // Do not fold cleanup into an advancing call: even a failed AX
            // action may have taken effect. Cancel is a separate bounded RPC.
            throw error as? ViewerSaveError ?? ViewerSaveError.actionFailed
        }
    }

    private func check() throws {
        guard let machine else { throw ViewerSaveError.terminal }
        try machine.request.checkDeadline(nowMS)
    }

    private func walk(_ root: Element, limit: Int = 5000, stopAtWebArea: Bool = false, dialog: Bool = false) throws -> [Element] {
        var queue = [root], result: [Element] = [], seen = Set<Element>(), offset = 0
        while offset < queue.count {
            try check()
            let node = queue[offset]; offset += 1
            guard seen.insert(node).inserted else { continue }
            guard result.count < limit else { throw ViewerSaveError.unsupported }
            AXUIElementSetMessagingTimeout(node.underlyingElement, 0.2)
            result.append(node)
            if stopAtWebArea && node.role() == "AXWebArea" { continue }
            // Never traverse file listings or read arbitrary filenames.
            if dialog && ["AXOutline", "AXTable", "AXBrowser"].contains(node.role() ?? "") && node.descriptionText() != "sidebar" { continue }
            let children = node.children() ?? []
            guard queue.count + children.count <= limit * 2 else { throw ViewerSaveError.unsupported }
            queue.append(contentsOf: children)
        }
        return result
    }

    private func prepare(_ input: [String: Any]) throws -> [String: Any] {
        guard !attempted else { throw ViewerSaveError.terminal }
        attempted = true
        let request = try ViewerSaveRequest(input, nowMS: nowMS)
        machine = ViewerSaveMachine(request: request)
        guard AXIsProcessTrusted() else { throw ViewerSaveError.unavailable } // Never prompts.
        let matches = try matchingSurfaces()
        guard matches.count == 1, let match = matches.first else { throw ViewerSaveError.ambiguous }
        application = match.0; launchDate = match.0.launchDate
        app = match.1; window = match.2; area = match.3
        guard launchDate != nil, try attachedPanels().isEmpty else { throw ViewerSaveError.changed }
        documentSave = try viewerSave(in: match.3, requireEnabled: true)
        try check()
        return ["status": "pending", "step": "prepared"] // Observation only.
    }

    private func matchingSurfaces() throws -> [(NSRunningApplication, Element, Element, Element)] {
        guard let machine else { throw ViewerSaveError.terminal }
        var matches: [(NSRunningApplication, Element, Element, Element)] = []
        let applications = NSRunningApplication.runningApplications(withBundleIdentifier: "org.mozilla.firefox")
        guard applications.count <= 8 else { throw ViewerSaveError.ambiguous }
        for application in applications {
            try check()
            let app = Element(AXUIElementCreateApplication(application.processIdentifier))
            AXUIElementSetMessagingTimeout(app.underlyingElement, 0.2)
            guard let windows = app.windows(), windows.count <= 100 else { throw ViewerSaveError.unavailable }
            for window in windows {
                for area in try walk(window, stopAtWebArea: true, dialog: true) where area.role() == "AXWebArea" {
                    // Compare the raw control URL, including signed query and
                    // fragment. Redacted display text is never an authority.
                    if area.url()?.absoluteString == machine.request.sourceURL {
                        matches.append((application, app, window, area))
                    }
                }
            }
        }
        return matches
    }

    private func label(_ node: Element) -> String {
        [node.title(), node.descriptionText()].compactMap { $0 }.first { !$0.isEmpty } ?? ""
    }

    private func viewerSave(in area: Element, requireEnabled: Bool) throws -> Element {
        let nodes = try walk(area)
        let buttons = nodes.filter { $0.role() == "AXButton" }
        let pagination = nodes.filter { $0.role() == "AXStaticText" }.map {
            let visibleLabel = label($0)
            return visibleLabel.isEmpty ? ($0.rawAttributeValue(named: "AXValue") as? String ?? "") : visibleLabel
        }
        guard ViewerToolbar.validates(buttonLabels: buttons.map(label), pagination: pagination,
                                      nestedWebArea: nodes.contains { $0 != area && $0.role() == "AXWebArea" }),
              let save = buttons.first(where: { label($0) == "Save" }), save.isActionSupported("AXPress"),
              !requireEnabled || save.isEnabled() == true else { throw ViewerSaveError.unsupported }
        return save
    }

    private func refreshIdentity() throws {
        try check()
        guard AXIsProcessTrusted(), let application, !application.isTerminated,
              application.bundleIdentifier == "org.mozilla.firefox", application.launchDate == launchDate,
              let app, let window, let area, let documentSave else { throw ViewerSaveError.applicationChanged }
        let matches = try matchingSurfaces()
        guard matches.count == 1, let match = matches.first else { throw ViewerSaveError.documentChanged }
        guard match.0.processIdentifier == application.processIdentifier, match.0.launchDate == launchDate,
              match.1 == app else { throw ViewerSaveError.applicationChanged }
        guard match.2 == window, match.3 == area else { throw ViewerSaveError.documentChanged }
        guard try viewerSave(in: area, requireEnabled: machine?.phase == .prepared) == documentSave
        else { throw ViewerSaveError.saveControlChanged }
    }

    private func attachedPanels() throws -> [Element] {
        guard let window, let app else { throw ViewerSaveError.changed }
        // Firefox's NSSavePanel can be absent from AXChildren while exposed as
        // AXFocusedWindow. Attachment, never global focus, establishes ownership.
        var sheets = try walk(window, stopAtWebArea: true, dialog: true).filter { $0.role() == "AXSheet" }
        if let focused = app.focusedWindow(), focused.role() == "AXSheet", focused.parent() == window, !sheets.contains(focused) {
            sheets.append(focused)
        }
        guard sheets.count <= 1 else { throw ViewerSaveError.ambiguous }
        for sheet in sheets {
            guard sheet.identifier() == "save-panel", sheet.parent() == window else { throw ViewerSaveError.changed }
        }
        return sheets
    }

    private func unique(_ nodes: [Element], _ predicate: (Element) -> Bool) throws -> Element {
        let matches = nodes.filter(predicate)
        guard matches.count == 1, let first = matches.first else { throw ViewerSaveError.ambiguous }
        return first
    }

    private func buttonRelation(_ node: Element, _ attribute: String, _ button: Element) -> ViewerPanelButton.Relation {
        guard let value = node.rawAttributeValue(named: attribute) else { return .absent }
        guard CFGetTypeID(value as CFTypeRef) == AXUIElementGetTypeID() else { return .conflicts }
        return Element(value as! AXUIElement) == button ? .matches : .conflicts
    }

    private func readPanel(_ sheet: Element) throws -> ViewerSaveMachine.Observation {
        guard panel == nil || panel == sheet else { throw ViewerSaveError.changed }
        guard machine?.phase != .prepared else { throw ViewerSaveError.changed }
        let nodes = try walk(sheet, limit: 1500, dialog: true)
        let name = try unique(nodes) { $0.identifier() == "saveAsNameTextField" && $0.role() == "AXTextField" }
        let location = try unique(nodes) { $0.identifier() == "where popup" }
        let save = try unique(nodes) { $0.role() == "AXButton" && label($0) == "Save" && $0.isActionSupported("AXPress") }
        let cancel = try unique(nodes) { $0.role() == "AXButton" && label($0) == "Cancel" && $0.isActionSupported("AXPress") }
        guard ViewerPanelButton.accepts(identifier: save.identifier(), kind: .save, relation: buttonRelation(sheet, "AXDefaultButton", save)),
              ViewerPanelButton.accepts(identifier: cancel.identifier(), kind: .cancel, relation: buttonRelation(sheet, "AXCancelButton", cancel)),
              name.isAttributeSettable(named: "AXValue"), let value = name.rawAttributeValue(named: "AXValue") as? String,
              let folder = location.rawAttributeValue(named: "AXValue") as? String else { throw ViewerSaveError.unsupported }
        var rows: [Element] = []
        for cell in nodes where cell.role() == "AXCell" {
            guard let row = cell.parent(), row.role() == "AXRow", row.isAttributeSettable(named: "AXSelected"),
                  let outline = row.parent(), outline.role() == "AXOutline", outline.descriptionText() == "sidebar" else { continue }
            if try walk(cell, limit: 20).contains(where: { $0.role() == "AXStaticText" && $0.rawAttributeValue(named: "AXValue") as? String == "Downloads" }) {
                if !rows.contains(row) { rows.append(row) }
            }
        }
        guard rows.count == 1, let row = rows.first else { throw ViewerSaveError.ambiguous }
        if panel != nil {
            guard nameField == name, whereField == location, saveButton == save, cancelButton == cancel, downloadsRow == row else { throw ViewerSaveError.changed }
        }
        // Retain only after the panel and its controls have been fully proved.
        panel = sheet; nameField = name; whereField = location; saveButton = save; cancelButton = cancel; downloadsRow = row
        return .init(hasPanel: true, filename: value, folder: folder,
                     downloadsSelected: row.rawAttributeValue(named: "AXSelected") as? Bool == true)
    }

    private func advance() throws -> [String: Any] {
        guard let phase = machine?.phase, ![.failed, .cancelled, .saved].contains(phase) else { throw ViewerSaveError.terminal }
        try refreshIdentity()
        let sheets = try attachedPanels()
        guard panel == nil || sheets.first == panel else { throw ViewerSaveError.changed }
        let observation = try sheets.first.map(readPanel) ?? ViewerSaveMachine.Observation()
        guard let effect = try machine?.next(observation, nowMS: nowMS) else { throw ViewerSaveError.terminal }
        // All paths above are reads. Exactly one AX effect follows, with a fresh
        // deadline and exact value/attachment checks immediately before delivery.
        try check()
        if effect == .awaitDialog || effect == .awaitDownloads {
            return ["status": "pending", "step": effect.rawValue]
        }
        try withAttentionCheck {
            try verifyDocument()
            switch effect {
            case .awaitDialog, .awaitDownloads: break // Observation-only paths returned above.
            case .documentSave:
                guard let documentSave, documentSave.isEnabled() == true, try attachedPanels().isEmpty else { throw ViewerSaveError.changed }
                try press(documentSave)
            case .rename:
                try verifyAttachment()
                guard let nameField, nameField.isAttributeSettable(named: "AXValue"), let request = machine?.request,
                      nameField.rawAttributeValue(named: "AXValue") as? String == observation.filename,
                      whereField?.rawAttributeValue(named: "AXValue") as? String == observation.folder else { throw ViewerSaveError.changed }
                try set(nameField, "AXValue", request.filename as CFString)
            case .downloads:
                try verifyName()
                guard let downloadsRow, downloadsRow.isAttributeSettable(named: "AXSelected"),
                      whereField?.rawAttributeValue(named: "AXValue") as? String == observation.folder,
                      (downloadsRow.rawAttributeValue(named: "AXSelected") as? Bool == true) == observation.downloadsSelected else { throw ViewerSaveError.changed }
                try set(downloadsRow, "AXSelected", kCFBooleanTrue)
            case .finalSave:
                try verifyName()
                guard whereField?.rawAttributeValue(named: "AXValue") as? String == "Downloads",
                      downloadsRow?.rawAttributeValue(named: "AXSelected") as? Bool == true,
                      let saveButton, saveButton.isEnabled() == true else { throw ViewerSaveError.changed }
                finalSaveAttempted = true
                try press(saveButton)
            }
        }
        return ["status": effect == .finalSave ? "saved" : "pending", "step": effect.rawValue]
    }

    private func verifyDocument() throws {
        try check()
        guard let application, !application.isTerminated, application.launchDate == launchDate,
              let app, let window, app.windows()?.contains(window) == true else { throw ViewerSaveError.applicationChanged }
        guard let area, area.role() == "AXWebArea", area.url()?.absoluteString == machine?.request.sourceURL else { throw ViewerSaveError.documentChanged }
        guard let documentSave, documentSave.role() == "AXButton", label(documentSave) == "Save" else { throw ViewerSaveError.saveControlChanged }
    }

    private func withAttentionCheck(_ effect: () throws -> Void) throws {
        let observer = NativeSpike()
        let before = try ViewerAttention(observer.status(timeout: 0.2))
        var deliveryError: Error?
        do { try effect() } catch { deliveryError = error }
        let after = try ViewerAttention(observer.status(timeout: 0.2))
        guard before == after else { throw ViewerSaveError.attentionChanged }
        if let deliveryError { throw deliveryError }
    }

    private func verifyAttachment() throws {
        try check()
        guard let panel, panel.parent() == window, panel.identifier() == "save-panel", try attachedPanels() == [panel] else { throw ViewerSaveError.changed }
    }

    private func verifyName() throws {
        try verifyAttachment()
        guard let request = machine?.request, nameField?.rawAttributeValue(named: "AXValue") as? String == request.filename else { throw ViewerSaveError.changed }
    }

    private func press(_ node: Element) throws {
        try check()
        guard node.isEnabled() == true, node.isActionSupported("AXPress") else { throw ViewerSaveError.changed }
        guard AXUIElementPerformAction(node.underlyingElement, kAXPressAction as CFString) == .success else { throw ViewerSaveError.actionFailed }
    }

    private func set(_ node: Element, _ attribute: String, _ value: CFTypeRef) throws {
        try check()
        guard AXUIElementSetAttributeValue(node.underlyingElement, attribute as CFString, value) == .success else { throw ViewerSaveError.actionFailed }
    }

    private func cancelRetainedPanel() {
        guard !cancelAttempted else { return }
        cancelAttempted = true
        defer { machine?.cancel() }
        // Never discover/adopt a new panel during cleanup, never after final Save,
        // and never touch an expired or replaced attachment.
        guard !finalSaveAttempted, machine?.phase != .saved, let application, !application.isTerminated,
              application.launchDate == launchDate, let app, let window, let panel, let cancelButton,
              (try? check()) != nil, app.windows()?.contains(window) == true,
              panel.parent() == window, panel.identifier() == "save-panel",
              ViewerPanelButton.accepts(identifier: cancelButton.identifier(), kind: .cancel,
                                        relation: buttonRelation(panel, "AXCancelButton", cancelButton)),
              (try? attachedPanels()) == [panel],
              (try? walk(panel, limit: 1500, dialog: true).contains(cancelButton)) == true,
              label(cancelButton) == "Cancel" else { return }
        try? withAttentionCheck { try press(cancelButton) }
    }
}
