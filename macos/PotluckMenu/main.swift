// Potluck's menu bar item (docs/menubar.md). It draws what
// `potluck --json host status` answers and, on a click, runs the item's
// arguments (`potluck host set <mode>`). Every word and rule is the potluck
// command's (package menubar); this file only draws and runs.

import AppKit

struct Item: Decodable {
    let id: String
    let label: String
    let sub: String?
    let checked: Bool
    let args: [String]?
    let confirm: String?
}

struct Menu: Decodable {
    let title: String
    let detail: String
    let note: String?
    let cores: String
    let items: [Item]
}

struct Status: Decodable {
    let menu: Menu
}

/// The program to run: $POTLUCK_BIN, else potluck in the first of the
/// usual places, else the program of the machine's Potluck daemon (its
/// record, recordedProgram), which must have potluck's `host status` and
/// `host set` commands; when it has not, running it fails and the menu says so.
func potluckPath() -> String? {
    let env = ProcessInfo.processInfo.environment
    if let p = env["POTLUCK_BIN"], !p.isEmpty { return p }
    let home = FileManager.default.homeDirectoryForCurrentUser.path
    let places = [home + "/.local/bin/potluck", "/opt/homebrew/bin/potluck", "/usr/local/bin/potluck", home + "/go/bin/potluck"]
    if let p = places.first(where: { FileManager.default.isExecutableFile(atPath: $0) }) { return p }
    return recordedProgram()
}

/// A daemon's machine record (kit/daemonrecord), the fields read here.
struct DaemonRecord: Decodable {
    let program: String
    let exe: String
    let pid: Int32
}

/// isPrivate: path is this user's alone (owned by this user, no group or
/// other permission bits, not a symlink), as kit/daemonrecord requires.
func isPrivate(_ path: String) -> Bool {
    var st = stat()
    guard lstat(path, &st) == 0 else { return false }
    let kind = st.st_mode & S_IFMT
    return (kind == S_IFDIR || kind == S_IFREG) && st.st_uid == getuid() && st.st_mode & 0o077 == 0
}

/// The program of the one daemon of another program than potluck whose
/// record is in the machine's records ($POTLUCK_DAEMONS_DIR, else
/// ~/.local/state/potluck/daemons) and whose process is this user's and
/// alive. Unlike the potluck command, this does not ask the daemon's
/// socket. None, or more than one: nil.
func recordedProgram() -> String? {
    let env = ProcessInfo.processInfo.environment
    let fm = FileManager.default
    var dir = fm.homeDirectoryForCurrentUser.path + "/.local/state/potluck/daemons"
    if let d = env["POTLUCK_DAEMONS_DIR"], !d.isEmpty { dir = d }
    guard isPrivate(dir), let names = try? fm.contentsOfDirectory(atPath: dir) else { return nil }
    var found: [String] = []
    for name in names where name.hasSuffix(".json") && !name.hasPrefix(".") {
        let path = dir + "/" + name
        guard isPrivate(path), let data = fm.contents(atPath: path),
              let r = try? JSONDecoder().decode(DaemonRecord.self, from: data),
              name == r.program + ".json", r.program != "potluck", r.pid > 0, kill(r.pid, 0) == 0,
              fm.isExecutableFile(atPath: r.exe)
        else { continue }
        found.append(r.exe)
    }
    return found.count == 1 ? found[0] : nil
}

/// Runs potluck with args; its exit code, stdout and stderr. Both pipes are
/// read at once (a full stderr cannot block it), and it is ended after 25 s.
func runPotluck(_ args: [String]) -> (Int32, Data, String) {
    guard let path = potluckPath() else {
        return (127, Data(), "potluck not found, and no running Potluck daemon names its program: install potluck (~/.local/bin) or set POTLUCK_BIN")
    }
    let p = Process()
    p.executableURL = URL(fileURLWithPath: path)
    p.arguments = args
    let out = Pipe(), err = Pipe()
    p.standardOutput = out
    p.standardError = err
    do { try p.run() } catch { return (126, Data(), "\(path): \(error.localizedDescription)") }
    let timer = DispatchWorkItem { if p.isRunning { p.terminate() } }
    DispatchQueue.global().asyncAfter(deadline: .now() + 25, execute: timer)
    var e = Data()
    let group = DispatchGroup()
    group.enter()
    DispatchQueue.global().async {
        e = err.fileHandleForReading.readDataToEndOfFile()
        group.leave()
    }
    let o = out.fileHandleForReading.readDataToEndOfFile()
    group.wait()
    p.waitUntilExit()
    timer.cancel()
    return (p.terminationStatus, o, String(decoding: e, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines))
}

/// The logo: a red pot with a green handle (the site's header SVG, 32x32).
func potImage() -> NSImage {
    let img = NSImage(size: NSSize(width: 18, height: 18), flipped: true) { r in
        let s = r.width / 32
        // M4 14 h24 a12 10 0 0 1 -24 0 z: the lower half of an ellipse
        // centred at (16, 14), 12 wide and 10 deep (y grows down here).
        NSGraphicsContext.saveGraphicsState()
        NSBezierPath(rect: NSRect(x: 0, y: 14 * s, width: 32 * s, height: 18 * s)).addClip()
        let bowl = NSBezierPath(ovalIn: NSRect(x: 4 * s, y: 4 * s, width: 24 * s, height: 20 * s))
        NSColor(red: 0xC8 / 255.0, green: 0x40 / 255.0, blue: 0x2F / 255.0, alpha: 1).setFill()
        bowl.fill()
        NSGraphicsContext.restoreGraphicsState()
        let lid = NSBezierPath(roundedRect: NSRect(x: 13 * s, y: 9 * s, width: 6 * s, height: 3 * s), xRadius: 1.5 * s, yRadius: 1.5 * s)
        NSColor(red: 0x2F / 255.0, green: 0x6B / 255.0, blue: 0x34 / 255.0, alpha: 1).setFill()
        lid.fill()
        return true
    }
    img.isTemplate = false
    return img
}

/// What a click on an item runs, fixed when the item is drawn.
final class Action: NSObject {
    let label: String
    let args: [String]
    let confirm: String?
    init(_ it: Item, _ args: [String]) { label = it.label; self.args = args; confirm = it.confirm }
}

final class App: NSObject, NSApplicationDelegate, NSMenuDelegate {
    let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
    let menu = NSMenu()
    var status: Status? // the last good answer
    var problem: String? // why the last ask failed
    var asked = Date.distantPast
    var busy = false

    func applicationDidFinishLaunching(_ n: Notification) {
        item.button?.image = potImage()
        item.button?.toolTip = "Potluck"
        menu.delegate = self
        menu.autoenablesItems = false
        item.menu = menu
        draw()
        refresh()
        // Each ask has the daemon ask every shared size's driver, so not often.
        Timer.scheduledTimer(withTimeInterval: 15, repeats: true) { [weak self] _ in self?.refresh() }
    }

    func menuWillOpen(_ m: NSMenu) {
        if Date().timeIntervalSince(asked) > 5 { refresh() }
    }

    /// Asks potluck for the status off the main thread, then redraws.
    func refresh() {
        if busy { return }
        busy = true
        asked = Date()
        DispatchQueue.global(qos: .utility).async {
            let (code, out, err) = runPotluck(["--json", "host", "status"])
            var st: Status?
            var why: String?
            if code == 0 {
                do { st = try JSONDecoder().decode(Status.self, from: out) } catch { why = "potluck host status: \(error.localizedDescription)" }
            } else {
                why = err.isEmpty ? "potluck host status exited \(code)" : err
            }
            DispatchQueue.main.async {
                self.busy = false
                if let st = st { self.status = st }
                self.problem = why
                self.draw()
            }
        }
    }

    func line(_ text: String, size: CGFloat, bold: Bool = false, color: NSColor = .labelColor, mono: Bool = false) -> NSMenuItem {
        let mi = NSMenuItem(title: text, action: nil, keyEquivalent: "")
        let font = mono ? NSFont.monospacedSystemFont(ofSize: size, weight: .regular) : (bold ? NSFont.boldSystemFont(ofSize: size) : NSFont.systemFont(ofSize: size))
        mi.attributedTitle = NSAttributedString(string: text, attributes: [.font: font, .foregroundColor: color])
        mi.isEnabled = false
        return mi
    }

    func draw() {
        menu.removeAllItems()
        if let m = status?.menu {
            menu.addItem(line(m.title, size: 13, bold: true))
            if !m.detail.isEmpty { menu.addItem(line(m.detail, size: 12, color: .secondaryLabelColor)) }
            if let n = m.note, !n.isEmpty { menu.addItem(line(n, size: 11, color: .secondaryLabelColor)) }
            if !m.cores.isEmpty { menu.addItem(line(m.cores, size: 11, color: .secondaryLabelColor, mono: true)) }
            if let p = problem { menu.addItem(line("not current: " + p, size: 11, color: .systemRed)) }
            menu.addItem(.separator())
            for it in m.items {
                let mi = NSMenuItem(title: it.label, action: #selector(click(_:)), keyEquivalent: "")
                mi.target = self
                mi.state = it.checked ? .on : .off
                let text = NSMutableAttributedString(string: it.label, attributes: [.font: NSFont.menuFont(ofSize: 0)])
                if let s = it.sub, !s.isEmpty {
                    text.append(NSAttributedString(string: "\n" + s, attributes: [.font: NSFont.systemFont(ofSize: 11), .foregroundColor: NSColor.secondaryLabelColor]))
                }
                mi.attributedTitle = text
                if let args = it.args { mi.representedObject = Action(it, args) }
                menu.addItem(mi)
            }
        } else {
            menu.addItem(line("Potluck", size: 13, bold: true))
            menu.addItem(line(problem ?? "reading…", size: 11, color: .secondaryLabelColor))
        }
        menu.addItem(.separator())
        let quit = NSMenuItem(title: "Quit Potluck Menu", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        menu.addItem(quit)
    }

    @objc func click(_ mi: NSMenuItem) {
        guard let act = mi.representedObject as? Action else { return } // already in force
        if let q = act.confirm, !q.isEmpty {
            let a = NSAlert()
            a.messageText = q
            a.addButton(withTitle: act.label)
            a.addButton(withTitle: "Cancel")
            NSApp.activate(ignoringOtherApps: true)
            if a.runModal() != .alertFirstButtonReturn { return }
        }
        DispatchQueue.global(qos: .userInitiated).async {
            let (code, _, err) = runPotluck(act.args)
            DispatchQueue.main.async {
                if code != 0 {
                    let a = NSAlert()
                    a.messageText = "\(act.label) did not work"
                    a.informativeText = err
                    NSApp.activate(ignoringOtherApps: true)
                    a.runModal()
                }
                self.refresh()
            }
        }
    }
}

let app = NSApplication.shared
let delegate = App()
app.delegate = delegate
app.setActivationPolicy(.accessory)
app.run()
