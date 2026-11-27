// The app's main menu: on screen while the app is in the Dock (Dock.swift).
// AppKit also finds the shortcuts of its text fields (⌘V to paste an invite)
// and windows (⌘W) through it.

import AppKit

func mainMenu() -> NSMenu {
    func item(_ title: String, _ action: Selector, _ key: String = "", _ mask: NSEvent.ModifierFlags = .command) -> NSMenuItem {
        let mi = NSMenuItem(title: title, action: action, keyEquivalent: key)
        mi.keyEquivalentModifierMask = mask
        return mi
    }
    func menu(_ title: String, _ items: [NSMenuItem]) -> NSMenu {
        let m = NSMenu(title: title)
        m.items = items
        return m
    }
    let window = menu("Window", [
        item("Minimize", #selector(NSWindow.performMiniaturize(_:)), "m"),
        item("Zoom", #selector(NSWindow.performZoom(_:))),
        item("Close", #selector(NSWindow.performClose(_:)), "w"),
        .separator(),
        item("Bring All to Front", #selector(NSApplication.arrangeInFront(_:))),
    ])
    let main = NSMenu()
    for sub in [
        menu("Potluck", [
            item("Set up and check…", #selector(App.showSetup)),
            .separator(),
            item("Hide Potluck", #selector(NSApplication.hide(_:)), "h"),
            item("Hide Others", #selector(NSApplication.hideOtherApplications(_:)), "h", [.command, .option]),
            item("Show All", #selector(NSApplication.unhideAllApplications(_:))),
            .separator(),
            item("Quit Potluck", #selector(NSApplication.terminate(_:)), "q"),
        ]),
        menu("Edit", [
            item("Undo", Selector(("undo:")), "z"),
            item("Redo", Selector(("redo:")), "Z"),
            .separator(),
            item("Cut", #selector(NSText.cut(_:)), "x"),
            item("Copy", #selector(NSText.copy(_:)), "c"),
            item("Paste", #selector(NSText.paste(_:)), "v"),
            item("Select All", #selector(NSText.selectAll(_:)), "a"),
        ]),
        window,
    ] {
        let mi = NSMenuItem(title: sub.title, action: nil, keyEquivalent: "")
        mi.submenu = sub
        main.addItem(mi)
    }
    NSApp.windowsMenu = window // AppKit lists the open windows there
    return main
}
