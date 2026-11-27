// The app's own question before an admin step, in every window that runs an
// action (runAction's ask).

import AppKit

/// potluck's reason for an admin step, with Continue (which brings up the
/// system's password dialog) and Cancel.
func askAdmin(_ step: AdminStep, over window: NSWindow, _ answer: @escaping (Bool) -> Void) {
    let a = NSAlert()
    a.messageText = "Your Mac's password is needed once"
    a.informativeText = step.why
    a.addButton(withTitle: "Continue")
    a.addButton(withTitle: "Cancel")
    if window.isVisible {
        Dock.enter()
        a.beginSheetModal(for: window) { answer($0 == .alertFirstButtonReturn) }
    } else { // closed while the action ran: the question still shows
        answer(Dock.ask(a) == .alertFirstButtonReturn)
    }
}

/// runAdmin, then the window back in front: the system's password dialog
/// belongs to another process, and when it closes macOS fronts whatever was
/// behind it, not this app (runAction's admin, for a window that asks).
func runAdmin(over window: NSWindow) -> ([String]) -> AdminResult {
    { [weak window] argv in
        let r = runAdmin(argv)
        DispatchQueue.main.async { if let w = window, w.isVisible { Dock.show(w) } }
        return r
    }
}
