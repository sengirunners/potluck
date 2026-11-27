// The menu bar's picture: the mark in one colour (icon/MenuBar.svg), which
// macOS draws light or dark as the menu bar needs. With a dot in its corner
// something is wrong (needsAttention), and the menu says what.

import AppKit

func menuBarGlyph(attention: Bool) -> NSImage {
    let mark = NSImage(named: "MenuBar")! // build.sh puts it in every app
    let img = !attention ? mark : NSImage(size: mark.size, flipped: false) { r in
        mark.draw(in: r)
        let d: CGFloat = 6.5, gap: CGFloat = 1.5
        let dot = NSRect(x: r.maxX - d, y: r.minY, width: d, height: d)
        NSGraphicsContext.current?.compositingOperation = .clear
        NSBezierPath(ovalIn: dot.insetBy(dx: -gap, dy: -gap)).fill()
        NSGraphicsContext.current?.compositingOperation = .sourceOver
        NSColor.black.setFill()
        NSBezierPath(ovalIn: dot).fill()
        return true
    }
    img.isTemplate = true
    img.accessibilityDescription = attention ? "Potluck needs attention" : "Potluck"
    return img
}
