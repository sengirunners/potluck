// When the app is in the Dock: while one of its windows or questions is open,
// so it can be switched to, hidden and quit like any app. With none it is in
// the menu bar only.

import AppKit

enum Dock {
    /// Questions up: one counts before its window shows, too.
    private static var asking = 0

    /// The app's windows that are open: shown, or minimized in the Dock. The
    /// menu bar's own window has no title bar.
    static var open: [NSWindow] {
        NSApp.windows.filter { $0.styleMask.contains(.titled) && ($0.isVisible || $0.isMiniaturized) }
    }

    /// Follows the windows as they close. Called once, at the start.
    static func follow() {
        for n in [NSWindow.willCloseNotification, NSApplication.didUnhideNotification] {
            NotificationCenter.default.addObserver(forName: n, object: nil, queue: .main) { _ in leaveIfEmpty() }
        }
    }

    /// Puts the app in the Dock and in front of the others.
    static func enter() {
        NSApp.setActivationPolicy(.regular)
        activate()
    }

    /// Takes the app to the front. Since macOS 14 activation is cooperative,
    /// and ignoringOtherApps is no longer honoured.
    static func activate() {
        if #available(macOS 14, *) { NSApp.activate() } else { NSApp.activate(ignoringOtherApps: true) }
    }

    static func show(_ w: NSWindow) {
        enter()
        w.makeKeyAndOrderFront(nil)
    }

    /// Asks a question that has no window of its own.
    static func ask(_ a: NSAlert) -> NSApplication.ModalResponse {
        asking += 1
        enter()
        // Again once the question runs: macOS may not have taken the app into
        // the Dock yet, and then leaves it behind the app in front.
        // Its window is key then, so Return presses its default button.
        RunLoop.main.perform(inModes: [.modalPanel]) {
            activate()
            a.window.makeKeyAndOrderFront(nil)
        }
        defer {
            asking -= 1
            leaveIfEmpty()
        }
        return a.runModal()
    }

    /// Brings what is open to the front: false when nothing is.
    static func bringUp() -> Bool {
        guard let first = open.first(where: { $0.isVisible }) ?? open.first else { return false }
        if first.isMiniaturized { first.deminiaturize(nil) }
        enter()
        return true
    }

    /// Leaves the Dock if nothing is open once this turn is over: a closing
    /// window is open until then, and one question or window may follow
    /// another. A hidden app's windows read as not shown: it stays till it is
    /// shown again.
    private static func leaveIfEmpty() {
        DispatchQueue.main.async {
            if asking == 0 && open.isEmpty && !NSApp.isHidden { NSApp.setActivationPolicy(.accessory) }
        }
    }
}
