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
    NSApp.activate(ignoringOtherApps: true)
    if window.isVisible {
        a.beginSheetModal(for: window) { answer($0 == .alertFirstButtonReturn) }
    } else { // closed while the action ran: the question still shows
        answer(a.runModal() == .alertFirstButtonReturn)
    }
}
