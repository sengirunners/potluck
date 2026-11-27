// Potluck's menu bar item (docs/menubar.md). It draws what
// `potluck --json host status` answers and, on a click, runs the item's
// arguments (`potluck host set <mode>`, or a dial's `potluck host lend ...`), or opens a window drawn from what
// they answer (`potluck --json host jobs`; the setup window draws the setup object of
// `potluck --json host status`, SetupWindow.swift; UninstallWindow.swift removes Potluck). Every word and rule is the potluck command's (package
// menubar); this file only draws and runs.

import AppKit

/// The job window's facts (`potluck --json host jobs`, menubar.JobsView).
struct JobDay: Decodable {
    let label: String
    let jobs: Int
    let energy: String
}

struct Energy: Decodable {
    let line: String
    let how: String
}

struct JobLine: Decodable {
    let when: String
    let took: String
    let size: String
    let pool: String
    let ended: String
    let energy: String
}

struct JobsView: Decodable {
    let title: String
    let summary: String
    let days: [JobDay]
    let energy: Energy
    let jobs: [JobLine]
    let note: String?
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
    /// A removal runs, or Potluck is removed: the app asks potluck nothing
    /// and the menu offers nothing.
    var held = false
    var clicks = 0 // menu choices running
    var login = LoginItem.system
    var launch = Launch()

    /// A removal is not left without the app it answers to.
    func applicationShouldTerminate(_ a: NSApplication) -> NSApplication.TerminateReply {
        guard uninstall?.running == true else { return .terminateNow }
        uninstall?.show()
        return .terminateCancel
    }

    func applicationDidFinishLaunching(_ n: Notification) {
        appEnvironment = { [login] in ["POTLUCK_LOGIN_ITEM": login.state()] }
        menu.delegate = self
        menu.autoenablesItems = false
        item.menu = menu
        draw()
        Dock.follow()
        launch.places.record(Bundle.main.bundlePath)
        // After a link this start came with is in.
        DispatchQueue.main.async { self.start() }
    }

    /// Begins asking potluck, and settles where the app is (Launch.swift).
    func start() {
        if offerMove(asked: false) { return } // another copy goes on
        guard noPotluckHere(), Bundled.app.carried else {
            Bundled.app.clear()
            return begin()
        }
        busy = true // nothing is asked of potluck before there is one
        Bundled.app.preparing = true
        UserDefaults.standard.set(true, forKey: "setupShown")
        setupWindow(setupArgs).show()
        DispatchQueue.global(qos: .userInitiated).async {
            Bundled.app.prepare()
            DispatchQueue.main.async {
                self.busy = false
                self.begin()
                self.setup?.load()
            }
        }
    }

    func begin() {
        launch.settled = true
        refresh()
        takeInvite()
        // Each ask has the daemon ask every shared size's driver, so not often.
        Timer.scheduledTimer(withTimeInterval: 15, repeats: true) { [weak self] _ in self?.refresh() }
    }

    func menuWillOpen(_ m: NSMenu) {
        if Date().timeIntervalSince(asked) > 5 { refresh() }
    }

    /// Asks potluck for the status off the main thread, then redraws.
    func refresh() {
        if busy || held { return }
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
                if Bundled.app.potluck != nil, installedPotluck() != nil, self.clicks == 0, self.setup?.acting != true { Bundled.app.clear() }
                self.problem = why
                self.draw()
                // From the run loop, not this block: a question it asks would
                // hold up every answer still to come.
                if let st = st { RunLoop.main.perform { self.settle(st) } }
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
        let glyph = menuBarGlyph(attention: !held && needsAttention(status, problem: problem))
        item.button?.image = glyph
        item.button?.toolTip = glyph.accessibilityDescription
        menu.removeAllItems()
        if held {
            menu.addItem(line(uninstall?.removed == true ? "Potluck is removed from this Mac" : "Removing Potluck…", size: 13, bold: true))
            let mi = NSMenuItem(title: "Show Uninstall Window", action: #selector(showUninstall), keyEquivalent: "")
            mi.target = self
            menu.addItem(mi)
        } else if let m = status?.menu {
            menu.addItem(line(m.title, size: 13, bold: true))
            if !m.detail.isEmpty { menu.addItem(line(m.detail, size: 12, color: .secondaryLabelColor)) }
            if let n = m.note, !n.isEmpty { menu.addItem(line(n, size: 11, color: .secondaryLabelColor)) }
            if !m.cores.isEmpty { menu.addItem(line(m.cores, size: 11, color: .secondaryLabelColor, mono: true)) }
            if let mem = m.memory, !mem.isEmpty { menu.addItem(line(mem, size: 11, color: .secondaryLabelColor)) }
            if problem != nil { menu.addItem(line("Couldn’t refresh. Showing the last status.", size: 11, color: .systemRed)) }
            menu.addItem(.separator())
            for it in m.items { menu.addItem(choice(it)) }
            if let ds = m.dials, !ds.isEmpty {
                menu.addItem(.separator())
                for d in ds {
                    let mi = NSMenuItem(title: d.label, action: nil, keyEquivalent: "")
                    let sub = NSMenu(title: d.label)
                    sub.autoenablesItems = false
                    for c in d.choices { sub.addItem(choice(c)) }
                    mi.submenu = sub
                    menu.addItem(mi)
                }
            }
            if let ws = m.windows, !ws.isEmpty {
                menu.addItem(.separator())
                for w in ws {
                    let mi = NSMenuItem(title: w.label, action: #selector(openWindow(_:)), keyEquivalent: "")
                    mi.target = self
                    mi.toolTip = w.sub
                    mi.representedObject = w.id
                    menu.addItem(mi)
                }
            }
        } else {
            menu.addItem(line("Potluck", size: 13, bold: true))
            menu.addItem(line(problem == nil ? "Checking this Mac…" : "Couldn’t check this Mac.", size: 11, color: .secondaryLabelColor))
        }
        menu.addItem(.separator())
        let quit = NSMenuItem(title: "Quit Potluck", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        menu.addItem(quit)
    }

    /// A clickable line: checked when in force; a click runs its arguments.
    func choice(_ it: Item) -> NSMenuItem {
        let mi = NSMenuItem(title: it.label, action: #selector(click(_:)), keyEquivalent: "")
        mi.target = self
        mi.state = it.checked ? .on : .off
        let text = NSMutableAttributedString(string: it.label, attributes: [.font: NSFont.menuFont(ofSize: 0)])
        if let s = it.sub, !s.isEmpty {
            text.append(NSAttributedString(string: "\n" + s, attributes: [.font: NSFont.systemFont(ofSize: 11), .foregroundColor: NSColor.secondaryLabelColor]))
        }
        mi.attributedTitle = text
        if let args = it.args { mi.representedObject = Action(it, args) }
        return mi
    }

    var jobs: JobsWindow?
    var setup: SetupWindow?
    var uninstall: UninstallWindow?

    @objc func showUninstall() { uninstall?.show() }

    /// The setup window, once the app has settled where it runs; during and
    /// after a removal, the uninstall window.
    @objc func showSetup() {
        if held { return showUninstall() }
        if launch.settled || setup != nil { setupWindow(setupArgs).show() } // the first one opens while the app unpacks
    }

    /// The app was opened again (Finder, Launchpad, its Dock icon).
    func applicationShouldHandleReopen(_ a: NSApplication, hasVisibleWindows: Bool) -> Bool {
        if !Dock.bringUp() { showSetup() }
        return false
    }

    /// The uninstall window's word that a removal runs (or stopped short).
    func hold(_ on: Bool) {
        held = on
        if on {
            setup?.window.close() // it asks every few seconds too
            jobs?.window.close()
        } else {
            refresh()
        }
        draw()
    }

    func setupWindow(_ args: [String]) -> SetupWindow {
        if let w = setup { return w }
        let w = SetupWindow(args: args)
        w.appAct = { [weak self] what, done in self?.loginAct(what, done) }
        setup = w
        return w
    }

    @objc func openWindow(_ mi: NSMenuItem) {
        guard !held, let id = mi.representedObject as? String,
              let w = status?.menu.windows?.first(where: { $0.id == id }), let args = w.args else { return }
        switch id {
        case "jobs":
            if jobs == nil { jobs = JobsWindow(args: args) }
            jobs?.show()
        case "setup":
            setupWindow(args).show()
        case "uninstall":
            if uninstall == nil {
                uninstall = UninstallWindow(args: args, blocked: { [weak self] in
                    if self?.setup?.acting == true { return "Set up and check is running a step: wait for it to finish, then press Remove again." }
                    return self?.clicks == 0 ? nil : "A choice from the menu is still being made: press Remove again in a moment."
                }, holding: { [weak self] on in self?.hold(on) },
                   quiet: { [weak self] in self?.busy != true && self?.setup?.asking != true && self?.jobs?.loading != true })
            }
            uninstall?.loginOff = { [login] in if login.state() != "off" { try? login.turnOff() } }
            uninstall?.show()
        default:
            return // a window this build cannot draw
        }
    }

    @objc func click(_ mi: NSMenuItem) {
        guard !held, let act = mi.representedObject as? Action else { return } // already in force
        if let q = act.confirm, !q.isEmpty {
            let a = NSAlert()
            a.messageText = q
            a.addButton(withTitle: act.label)
            a.addButton(withTitle: "Cancel")
            if Dock.ask(a) != .alertFirstButtonReturn { return }
        }
        clicks += 1
        DispatchQueue.global(qos: .userInitiated).async {
            let (code, _, err) = runPotluck(act.args)
            DispatchQueue.main.async {
                self.clicks -= 1
                if code != 0 {
                    let a = NSAlert()
                    a.messageText = "\(act.label) did not work"
                    a.informativeText = err
                    _ = Dock.ask(a)
                }
                self.refresh()
            }
        }
    }
}

/// The window of every job this Mac ran: the counts, the electricity
/// estimate and how it is worked out, the days, then a row per job. All of
/// it is the command's words; this only lays them out.
final class JobsWindow: NSObject, NSTableViewDataSource, NSTableViewDelegate {
    let args: [String]
    let window: NSWindow
    let summary = NSTextField(wrappingLabelWithString: "reading…")
    let energy = NSTextField(wrappingLabelWithString: "")
    let how = NSTextField(wrappingLabelWithString: "")
    let days = NSTextField(wrappingLabelWithString: "")
    let note = NSTextField(wrappingLabelWithString: "")
    let table = NSTableView()
    var rows: [JobLine] = []
    var loading: Bool { loads > 0 }
    var loads = 0
    let columns: [(String, String, CGFloat)] = [("when", "Started", 120), ("took", "Up", 80), ("size", "Size", 190),
                                                ("pool", "Pool", 120), ("ended", "Ended", 220), ("energy", "Electricity", 90)]

    init(args: [String]) {
        self.args = args
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 860, height: 560), styleMask: [.titled, .closable, .resizable, .miniaturizable], backing: .buffered, defer: false)
        super.init()
        window.title = "Jobs this Mac ran"
        window.isReleasedWhenClosed = false
        summary.font = .boldSystemFont(ofSize: 13)
        for f in [how, note] { f.font = .systemFont(ofSize: 11); f.textColor = .secondaryLabelColor }
        days.font = .monospacedSystemFont(ofSize: 11, weight: .regular)
        for (id, title, width) in columns {
            let c = NSTableColumn(identifier: NSUserInterfaceItemIdentifier(id))
            c.title = title
            c.width = width
            table.addTableColumn(c)
        }
        table.dataSource = self
        table.delegate = self
        table.usesAlternatingRowBackgroundColors = true
        let scroll = NSScrollView()
        scroll.documentView = table
        scroll.hasVerticalScroller = true
        let refresh = NSButton(title: "Refresh", target: self, action: #selector(load))
        let stack = NSStackView(views: [summary, energy, how, days, note, scroll, refresh])
        stack.orientation = .vertical
        stack.alignment = .leading
        stack.spacing = 8
        stack.edgeInsets = NSEdgeInsets(top: 16, left: 16, bottom: 16, right: 16)
        stack.translatesAutoresizingMaskIntoConstraints = false
        let content = NSView()
        content.addSubview(stack)
        window.contentView = content
        NSLayoutConstraint.activate([
            stack.leadingAnchor.constraint(equalTo: content.leadingAnchor), stack.trailingAnchor.constraint(equalTo: content.trailingAnchor),
            stack.topAnchor.constraint(equalTo: content.topAnchor), stack.bottomAnchor.constraint(equalTo: content.bottomAnchor),
            scroll.widthAnchor.constraint(equalTo: stack.widthAnchor, constant: -32),
            scroll.heightAnchor.constraint(greaterThanOrEqualToConstant: 200),
        ])
        for f in [summary, energy, how, days, note] { f.preferredMaxLayoutWidth = 820 }
    }

    func show() {
        window.center()
        Dock.show(window)
        load()
    }

    @objc func load() {
        loads += 1
        let args = self.args
        DispatchQueue.global(qos: .userInitiated).async {
            let (code, out, err) = runPotluck(args)
            var v: JobsView?
            var why: String?
            if code == 0 {
                do { v = try JSONDecoder().decode(JobsView.self, from: out) } catch { why = "potluck host jobs: \(error.localizedDescription)" }
            } else {
                why = err.isEmpty ? "potluck host jobs exited \(code)" : err
            }
            DispatchQueue.main.async {
                self.loads -= 1
                self.draw(v, why)
            }
        }
    }

    func draw(_ v: JobsView?, _ why: String?) {
        guard let v = v else {
            note.stringValue = "Couldn’t refresh jobs. Try again in a moment."
            note.toolTip = why
            note.textColor = .systemRed
            return
        }
        note.toolTip = nil
        summary.stringValue = v.summary
        energy.stringValue = v.energy.line
        how.stringValue = v.energy.how
        let busy = v.days.filter { $0.jobs > 0 }
        days.stringValue = busy.isEmpty ? "No jobs in the last two weeks." :
            "Jobs per day:\n" + busy.map { "  \($0.label.padding(toLength: 12, withPad: " ", startingAt: 0)) \($0.jobs), \($0.energy)" }.joined(separator: "\n")
        note.stringValue = v.note ?? ""
        note.textColor = .secondaryLabelColor
        rows = v.jobs
        table.reloadData()
    }

    func numberOfRows(in tableView: NSTableView) -> Int { rows.count }

    func tableView(_ tableView: NSTableView, viewFor tableColumn: NSTableColumn?, row: Int) -> NSView? {
        guard let id = tableColumn?.identifier.rawValue, row < rows.count else { return nil }
        let j = rows[row]
        let text: String
        switch id {
        case "when": text = j.when
        case "took": text = j.took
        case "size": text = j.size
        case "pool": text = j.pool
        case "ended": text = j.ended
        default: text = j.energy
        }
        let f = (tableView.makeView(withIdentifier: NSUserInterfaceItemIdentifier("cell"), owner: nil) as? NSTextField) ?? {
            let f = NSTextField(labelWithString: "")
            f.identifier = NSUserInterfaceItemIdentifier("cell")
            f.lineBreakMode = .byTruncatingTail
            return f
        }()
        f.stringValue = text
        f.toolTip = text
        return f
    }
}

let app = NSApplication.shared
let delegate = App()
app.delegate = delegate
app.setActivationPolicy(.accessory)
app.mainMenu = mainMenu()
app.run()
