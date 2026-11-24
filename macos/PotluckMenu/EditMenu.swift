// The app's main menu. A menu bar item shows none, but AppKit finds the
// shortcuts of its text fields (⌘V to paste an invite) and windows (⌘W)
// through it.

import AppKit

func editMenu() -> NSMenu {
    let main = NSMenu()
    for (title, items) in [
        ("Potluck", []),
        ("Edit", [("Undo", Selector(("undo:")), "z"), ("Cut", #selector(NSText.cut(_:)), "x"), ("Copy", #selector(NSText.copy(_:)), "c"),
                  ("Paste", #selector(NSText.paste(_:)), "v"), ("Select All", #selector(NSText.selectAll(_:)), "a")]),
        ("Window", [("Close", #selector(NSWindow.performClose(_:)), "w")]),
    ] as [(String, [(String, Selector, String)])] {
        let sub = NSMenu(title: title)
        for (t, action, key) in items { sub.addItem(NSMenuItem(title: t, action: action, keyEquivalent: key)) }
        let mi = NSMenuItem(title: title, action: nil, keyEquivalent: "")
        mi.submenu = sub
        main.addItem(mi)
    }
    return main
}
