// The "Uninstall Potluck…" window: what `potluck --json uninstall --dry-run`
// answers (the plan, what stays, what is lost for good), and a Remove button
// that asks and then runs the answer's own arguments (runAction, with
// potluck's admin step rule). At the end it offers to move this app to the
// Trash. The plan, warnings and errors are the command's words.

import AppKit

final class UninstallWindow: NSObject, NSWindowDelegate {
    let args: [String]
    let window: NSWindow
    /// Why a removal cannot start now (another window's action runs), or nil.
    let blocked: () -> String?
    /// Told true when a removal starts (the app must not ask potluck
    /// anything: an ask could meet the files going away) and again when it
    /// removed Potluck, false when it stopped short.
    let holding: (Bool) -> Void
    /// No ask of the app's is still running.
    let quiet: () -> Bool
    let title = NSTextField(wrappingLabelWithString: "")
    let warning = NSTextField(wrappingLabelWithString: "")
    let result = NSTextField(wrappingLabelWithString: "")
    let lists = NSStackView()
    let removeButton = NSButton(title: "Remove…", target: nil, action: nil)
    let forceButton = NSButton(title: "Remove anyway…", target: nil, action: nil)
    let checkButton = NSButton(title: "Check again", target: nil, action: nil)
    let trashButton = NSButton(title: "Move the app to the Trash and quit", target: nil, action: nil)
    var plan: ActionAnswer? // the last dry run's answer; nil: none, or not current
    var force: OfferedAction? // what a refused run offers
    var asking = false
    var acting = false // a question is up, or a removal runs
    var running = false // a removal runs
    var begun = false // a removal stopped short: Remove goes on with it
    var removed = false // Potluck is off this Mac
    /// Does the move to the Trash.
    var trash: (URL) throws -> Void = { try FileManager.default.trashItem(at: $0, resultingItemURL: nil) }

    init(args: [String], blocked: @escaping () -> String?, holding: @escaping (Bool) -> Void, quiet: @escaping () -> Bool) {
        self.quiet = quiet
        self.args = args
        self.blocked = blocked
        self.holding = holding
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 640, height: 600), styleMask: [.titled, .closable, .resizable, .miniaturizable], backing: .buffered, defer: false)
        super.init()
        window.title = "Uninstall Potluck"
        window.isReleasedWhenClosed = false
        window.minSize = NSSize(width: 520, height: 360)
        window.delegate = self
        title.font = .boldSystemFont(ofSize: 13)
        warning.font = .systemFont(ofSize: 12, weight: .medium)
        warning.textColor = .systemRed
        result.font = .systemFont(ofSize: 12)
        result.isSelectable = true
        lists.orientation = .vertical
        lists.alignment = .leading
        lists.spacing = 6
        for (b, act) in [(removeButton, #selector(pressRemove)), (forceButton, #selector(pressForce)), (checkButton, #selector(load)), (trashButton, #selector(pressTrash))] {
            b.target = self
            b.action = act
            b.bezelStyle = .rounded
        }
        removeButton.hasDestructiveAction = true
        forceButton.hasDestructiveAction = true
        let column = NSStackView(views: [title, warning, result, lists])
        column.orientation = .vertical
        column.alignment = .leading
        column.spacing = 10
        column.edgeInsets = NSEdgeInsets(top: 16, left: 16, bottom: 16, right: 16)
        column.translatesAutoresizingMaskIntoConstraints = false
        let flipped = FlippedView()
        flipped.translatesAutoresizingMaskIntoConstraints = false
        flipped.addSubview(column)
        let scroll = NSScrollView()
        scroll.documentView = flipped
        scroll.hasVerticalScroller = true
        scroll.drawsBackground = false
        scroll.translatesAutoresizingMaskIntoConstraints = false
        // The buttons stay in sight below a long plan.
        let buttons = NSStackView(views: [checkButton, forceButton, removeButton, trashButton])
        buttons.orientation = .horizontal
        buttons.spacing = 10
        buttons.translatesAutoresizingMaskIntoConstraints = false
        let content = NSView()
        content.addSubview(scroll)
        content.addSubview(buttons)
        window.contentView = content
        NSLayoutConstraint.activate([
            scroll.leadingAnchor.constraint(equalTo: content.leadingAnchor), scroll.trailingAnchor.constraint(equalTo: content.trailingAnchor),
            scroll.topAnchor.constraint(equalTo: content.topAnchor), scroll.bottomAnchor.constraint(equalTo: buttons.topAnchor, constant: -12),
            buttons.trailingAnchor.constraint(equalTo: content.trailingAnchor, constant: -16), buttons.bottomAnchor.constraint(equalTo: content.bottomAnchor, constant: -16),
            flipped.leadingAnchor.constraint(equalTo: scroll.contentView.leadingAnchor), flipped.trailingAnchor.constraint(equalTo: scroll.contentView.trailingAnchor),
            flipped.topAnchor.constraint(equalTo: scroll.contentView.topAnchor),
            column.leadingAnchor.constraint(equalTo: flipped.leadingAnchor), column.trailingAnchor.constraint(equalTo: flipped.trailingAnchor),
            column.topAnchor.constraint(equalTo: flipped.topAnchor), column.bottomAnchor.constraint(equalTo: flipped.bottomAnchor),
            lists.widthAnchor.constraint(equalTo: column.widthAnchor, constant: -32),
            warning.widthAnchor.constraint(equalTo: column.widthAnchor, constant: -32), result.widthAnchor.constraint(equalTo: column.widthAnchor, constant: -32),
        ])
        drawButtons()
    }

    func show() {
        let reopened = !window.isVisible && !window.isMiniaturized
        if reopened { window.center() }
        NSApp.activate(ignoringOtherApps: true)
        window.makeKeyAndOrderFront(nil)
        guard reopened, !acting, !removed else { return }
        plan = nil
        force = nil
        begun = false
        say("", error: false)
        load()
    }

    /// A removal goes on to its end in the window that shows it.
    func windowShouldClose(_ w: NSWindow) -> Bool { !acting }

    @objc func load() {
        guard !acting, !removed else { return }
        force = nil
        title.stringValue = "Reading what Potluck put on this Mac…"
        ask { _ in }
    }

    /// Asks for the plan and draws it; then gets it, or nil when potluck did
    /// not answer one (shown).
    func ask(_ then: @escaping (ActionAnswer?) -> Void) {
        if asking { return }
        asking = true
        drawButtons()
        let args = self.args
        DispatchQueue.global(qos: .userInitiated).async {
            let (code, out, err) = runPotluck(args)
            // Only a plan says left: another program's answer is none.
            let a = code == 0 ? actionAnswer(in: out).flatMap { $0.left == nil ? nil : $0 } : nil
            let why = code == 0 ? "This potluck does not say what it would remove; update it." : errorText(err, code)
            DispatchQueue.main.async {
                self.asking = false
                self.plan = a
                if let a = a { self.draw(a) } else { self.drawUnknown(why) }
                self.drawButtons()
                then(a)
            }
        }
    }

    func draw(_ a: ActionAnswer) {
        let left = a.left ?? []
        title.stringValue = left.isEmpty ? "Nothing of Potluck's is on this Mac but this app." : "This removes Potluck from this Mac."
        warn(a.warning ?? "")
        fill([("WILL BE REMOVED, IN THIS ORDER", numbered(left)), ("STAYS ON THIS MAC", a.not_removed ?? [])])
    }

    func drawUnknown(_ why: String) {
        title.stringValue = "Potluck did not say what it would remove."
        warn("")
        fill([("WHY", [why])])
    }

    func drawDone(_ p: Progress) {
        title.stringValue = "Potluck is removed from this Mac."
        warn("")
        say("", error: false)
        fill([("DONE", p.done ?? []), ("STAYS ON THIS MAC, FOR YOU TO REMOVE IF YOU LIKE", p.notRemoved)])
    }

    func drawButtons() {
        let here = !removed
        let nothing = plan?.left?.isEmpty == true
        removeButton.title = begun ? "Continue removing…" : "Remove…"
        removeButton.isHidden = !here || nothing
        removeButton.isEnabled = !acting && !asking && plan?.remove != nil
        forceButton.isHidden = !here || force == nil
        forceButton.isEnabled = !acting && !asking
        checkButton.isHidden = !here
        checkButton.isEnabled = !acting && !asking
        trashButton.isHidden = !(removed || nothing)
        trashButton.isEnabled = !acting && !asking
    }

    /// Remove: asks for the plan again (what is shown may be old), then asks
    /// the person with that plan's words.
    @objc func pressRemove() {
        start { [weak self] in
            self?.ask { a in
                guard let self = self else { return }
                guard let r = a?.remove else { return self.stopped() }
                self.confirm(r, button: "Remove Potluck")
            }
        }
    }

    /// Remove anyway: what a refused run offers, after its own question,
    /// with what the plan as it is now loses.
    @objc func pressForce() {
        guard let f = force else { return }
        start { [weak self] in
            self?.ask { a in
                guard let self = self else { return }
                guard let a = a else { return self.stopped() }
                let detail = [f.detail, a.warning].compactMap { $0 }.joined(separator: " ")
                self.confirm(OfferedAction(confirm: f.confirm, detail: detail, args: f.args), button: "Remove anyway")
            }
        }
    }

    func start(_ then: () -> Void) {
        guard !acting, !asking, !removed else { return }
        if let why = blocked() { return say(why, error: true) }
        acting = true
        drawButtons()
        then()
    }

    func stopped() {
        acting = false
        drawButtons()
    }

    func confirm(_ o: OfferedAction, button: String) {
        let a = NSAlert()
        a.alertStyle = .critical
        a.messageText = o.confirm
        a.informativeText = [o.detail ?? "", "This cannot be undone."].filter { !$0.isEmpty }.joined(separator: "\n\n")
        // Escape is Cancel and Return nothing: removal takes a click.
        a.addButton(withTitle: "Cancel")
        let go = a.addButton(withTitle: button)
        go.hasDestructiveAction = true
        go.keyEquivalent = ""
        a.beginSheetModal(for: window) { r in
            if r != .alertSecondButtonReturn { return self.stopped() }
            if let why = self.blocked() { // started while the question was up
                self.say(why, error: true)
                return self.stopped()
            }
            self.run(o.args)
        }
    }

    func run(_ args: [String]) {
        force = nil
        holding(true)
        say("Removing Potluck…", error: false)
        drawButtons()
        whenQuiet { self.remove(args) }
    }

    /// Runs go once the asks already under way have ended (runPotluck ends
    /// each at its limit). Until then nothing was removed: Quit still works.
    func whenQuiet(_ go: @escaping () -> Void) {
        if quiet() { return go() }
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.2) { self.whenQuiet(go) }
    }

    func remove(_ args: [String]) {
        running = true
        runAction(args, ask: { step, answer in askAdmin(step, over: self.window, answer) }) { r in
            self.acting = false
            self.running = false
            var offer: OfferedAction?
            switch r {
            case .done(let p) where p.done != nil && p.left.isEmpty:
                self.removed = true
                self.drawDone(p)
                self.drawButtons()
                return self.holding(true) // for good: nothing is left to ask
            case .done(let p):
                self.begun = p.done?.isEmpty == false
                self.say(failText("Uninstall", "potluck did not say it finished", p), error: true)
            case .failed(let why, let p):
                self.begun = p.done?.isEmpty == false
                offer = p.force
                self.say(failText("Uninstall", why, p), error: true)
            case .cancelled(let why, let p):
                self.begun = p.done?.isEmpty == false
                self.say(cancelText(why, p), error: false)
            }
            self.holding(false)
            self.load()
            self.force = offer
            self.drawButtons()
        }
    }

    @objc func pressTrash() {
        guard !acting, !asking else { return }
        if let why = blocked() { return say(why, error: true) }
        if let why = trashApp(Bundle.main.bundleURL, trash: trash) { return say(why, error: true) }
        NSApp.terminate(nil)
    }

    func say(_ text: String, error: Bool) {
        result.stringValue = text
        result.textColor = error ? .systemRed : .labelColor
        result.isHidden = text.isEmpty
    }

    func warn(_ text: String) {
        warning.stringValue = text
        warning.isHidden = text.isEmpty
    }

    func numbered(_ xs: [String]) -> [String] { xs.enumerated().map { "\($0.offset + 1). \($0.element)" } }

    func fill(_ sections: [(String, [String])]) {
        lists.arrangedSubviews.forEach { lists.removeArrangedSubview($0); $0.removeFromSuperview() }
        for (head, lines) in sections where !lines.isEmpty {
            let h = NSTextField(labelWithString: head)
            h.font = .boldSystemFont(ofSize: 11)
            h.textColor = .secondaryLabelColor
            lists.addArrangedSubview(h)
            for l in lines {
                let t = NSTextField(wrappingLabelWithString: l)
                t.font = .systemFont(ofSize: 12)
                t.isSelectable = true // commands to copy
                lists.addArrangedSubview(t)
                t.widthAnchor.constraint(equalTo: lists.widthAnchor).isActive = true
            }
        }
    }
}
