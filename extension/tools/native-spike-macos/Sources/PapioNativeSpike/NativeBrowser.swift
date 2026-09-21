// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Fixed browser choices for the development-only loopback fixture helper.
enum NativeBrowser: String {
    case chrome, firefox

    var bundleIdentifier: String {
        switch self {
        case .chrome: return "com.google.Chrome"
        case .firefox: return "org.mozilla.firefox"
        }
    }

    static func configured(_ value: Any?, surfaceBound: Bool) throws -> Self {
        // A retained AX window and its targets belong to the original runtime.
        // Start a new helper after binding, even for same-browser reconfiguration.
        guard !surfaceBound else { throw SpikeError.stale }
        guard let value else { return .chrome }
        guard let name = value as? String, let browser = Self(rawValue: name) else {
            throw SpikeError.invalidRequest
        }
        return browser
    }
}
