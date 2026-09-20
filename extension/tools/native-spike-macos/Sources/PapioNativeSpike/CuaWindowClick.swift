// Adapted from trycua/cua 9bbfa7dd3e27ca7f1861ede70aaca390174493f9,
// platform-macos/src/input/{mouse,skylight}.rs. MIT, Copyright (c) 2025 Cua AI, Inc.
// See THIRD-PARTY-NOTICES.md. Development evaluation only.
import CoreGraphics
import Darwin
import Foundation

// This evaluates upstream's five-event window delivery sequence. It deliberately
// does not call prepare_background_pixel_click/activate_without_raise: those
// explicitly defocus the operator's app. Whether this subset works is measured,
// never assumed to inherit the upstream driver's background-support claims.
@MainActor
enum CuaWindowClick {
    typealias Post = @convention(c) (Int32, UnsafeMutableRawPointer) -> Void
    typealias SetField = @convention(c) (UnsafeMutableRawPointer, UInt32, Int64) -> Void
    typealias SetLocation = @convention(c) (UnsafeMutableRawPointer, Double, Double) -> Void
    static let library = dlopen("/System/Library/PrivateFrameworks/SkyLight.framework/SkyLight", RTLD_LAZY | RTLD_LOCAL)

    static func click(pid: Int32, windowID: UInt32, point: CGPoint, localPoint: CGPoint) throws {
        guard let library,
              let postSymbol = dlsym(library, "SLEventPostToPid"),
              let fieldSymbol = dlsym(library, "SLEventSetIntegerValueField"),
              let locationSymbol = dlsym(library, "CGEventSetWindowLocation"),
              let source = CGEventSource(stateID: .hidSystemState) else { throw SpikeError.unsupported }
        let post = unsafeBitCast(postSymbol, to: Post.self)
        let field = unsafeBitCast(fieldSymbol, to: SetField.self)
        let location = unsafeBitCast(locationSymbol, to: SetLocation.self)
        // Match upstream's subsecond-nanosecond field, not an epoch timestamp.
        let group = Int64(Date().timeIntervalSince1970.truncatingRemainder(dividingBy: 1) * 1000000000)
        let outside = CGPoint(x: -1, y: -1)
        let sequence: [(CGEventType, CGPoint, CGPoint, Int64, Int64, Double)] = [
            (.mouseMoved, point, localPoint, 0, 2, 0.015),
            (.leftMouseDown, outside, outside, 1, 1, 0.001),
            (.leftMouseUp, outside, outside, 1, 2, 0.100),
            (.leftMouseDown, point, localPoint, 1, 3, 0.001),
            (.leftMouseUp, point, localPoint, 1, 3, 0),
        ]
        for (type, screen, local, count, phase, delay) in sequence {
            guard let event = CGEvent(mouseEventSource: source, mouseType: type, mouseCursorPosition: screen, mouseButton: .left) else { throw SpikeError.unsupported }
            let raw = Unmanaged.passUnretained(event).toOpaque()
            for (key, value) in [(UInt32(0), phase), (1, count), (3, 0), (7, 3), (40, Int64(pid)), (51, Int64(windowID)), (58, group), (91, Int64(windowID)), (92, Int64(windowID))] {
                field(raw, key, value)
            }
            location(raw, local.x, local.y)
            post(pid, raw)
            if delay > 0 { Thread.sleep(forTimeInterval: delay) }
        }
    }
}
