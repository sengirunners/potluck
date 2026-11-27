// The "Uninstall Potluck…" window: what `potluck --json uninstall --dry-run`
// answers (the plan, what stays, what is lost for good, the box to tick), and
// a Remove button that asks and then runs the answer's own arguments
// (runAction, with potluck's admin step rule). At the end it offers to move
// this app to the Trash. Its words are the answer's view (cli/
// uninstall_view.go); only what potluck cannot say (it did not answer) is
// worded here. The terminal's words are copied (Copy details), never shown.

import AppKit

final class UninstallWindow: NSObject, NSWindowDelegate {
    let args: [String]
    /// What asks for the plan now: args, or a ticked box's.
    var planArgs: [String]
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
    let lead = NSTextField(wrappingLabelWithString: "")
    let choice = NSButton(checkboxWithTitle: "", target: nil, action: nil)
    let choiceText = NSTextField(wrappingLabelWithString: "")
    let choiceRow = NSStackView()
    let lists = NSStackView()
    let copyButton = NSButton(title: "Copy details", target: nil, action: nil)
    let removeButton = NSButton(title: "Remove…", target: nil, action: nil)
    let forceButton = NSButton(title: "Remove anyway…", target: nil, action: nil)
    let checkButton = NSButton(title: "Check again", target: nil, action: nil)
    let trashButton = NSButton(title: "Move to Trash and Quit", target: nil, action: nil)
    var plan: ActionAnswer? // the last dry run's answer; nil: none, or not current
    var outcome: UninstallView? // how the last removal stopped
    var before: UninstallView? // what earlier runs in this window did
    var details = "" // what Copy details copies
    var force: OfferedAction? // what a refused run offers
    var asking = false
    var acting = false // a question is up, or a removal runs
    var running = false // a removal runs
    var removed = false // Potluck is off this Mac
    /// Turns the app's login item off, before the app goes.
    var loginOff: () -> Void = {}
    /// Does the move to the Trash.
    var trash: (URL) throws -> Void = { try FileManager.default.trashItem(at: $0, resultingItemURL: nil) }

    init(args: [String], blocked: @escaping () -> String?, holding: @escaping (Bool) -> Void, quiet: @escaping () -> Bool) {
        self.quiet = quiet
        self.args = args
        planArgs = args
        self.blocked = blocked
        self.holding = holding
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 640, height: 600), styleMask: [.titled, .closable, .resizable, .miniaturizable], backing: .buffered, defer: false)
        super.init()
        window.title = "Uninstall Potluck"
        window.isReleasedWhenClosed = false
        window.minSize = NSSize(width: 520, height: 360)
        window.delegate = self
        title.font = .systemFont(ofSize: 20, weight: .semibold)
        lead.font = .systemFont(ofSize: 13)
        choice.target = self
        choice.action = #selector(pressChoice)
        choice.setContentHuggingPriority(.required, for: .horizontal)
        choiceText.font = .systemFont(ofSize: 13)
        choiceText.addGestureRecognizer(NSClickGestureRecognizer(target: self, action: #selector(pressChoiceText)))
        choiceRow.setViews([choice, choiceText], in: .leading)
        choiceRow.orientation = .horizontal
        choiceRow.alignment = .firstBaseline
        choiceRow.spacing = 4
        warning.font = .systemFont(ofSize: 13, weight: .medium)
        warning.textColor = .systemRed
        result.font = .systemFont(ofSize: 12)
        lists.orientation = .vertical
        lists.alignment = .leading
        lists.spacing = 6
        for (b, act) in [(removeButton, #selector(pressRemove)), (forceButton, #selector(pressForce)), (checkButton, #selector(load)), (trashButton, #selector(pressTrash)), (copyButton, #selector(copyDetails))] {
            b.target = self
            b.action = act
            b.bezelStyle = .rounded
        }
        removeButton.hasDestructiveAction = true
        forceButton.hasDestructiveAction = true
        let column = NSStackView(views: [title, lead, warning, result, choiceRow, lists])
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
        let buttons = NSStackView(views: [copyButton, checkButton, forceButton, removeButton, trashButton])
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
            title.widthAnchor.constraint(equalTo: column.widthAnchor, constant: -32), lead.widthAnchor.constraint(equalTo: column.widthAnchor, constant: -32),
            choiceRow.widthAnchor.constraint(equalTo: column.widthAnchor, constant: -32),
            warning.widthAnchor.constraint(equalTo: column.widthAnchor, constant: -32), result.widthAnchor.constraint(equalTo: column.widthAnchor, constant: -32),
        ])
        drawButtons()
    }

    func show() {
        let reopened = !window.isVisible && !window.isMiniaturized
        if reopened { window.center() }
        Dock.show(window)
        guard reopened, !acting, !removed else { return }
        plan = nil
        force = nil
        outcome = nil
        before = nil
        planArgs = args // the box starts unticked
        say("", error: false)
        load()
    }

    /// A removal goes on to its end in the window that shows it.
    func windowShouldClose(_ w: NSWindow) -> Bool { !acting }

    @objc func load() {
        guard !acting, !removed else { return }
        force = nil
        outcome = nil
        head("Checking what will be removed…", "")
        ask { _ in }
    }

    /// The box was pressed: the plan its arguments ask for. Until that is
    /// drawn Remove is off (asking).
    @objc func pressChoice() {
        guard let c = plan?.view?.choice, !acting, !asking, !removed else { return }
        planArgs = c.args
        ask { _ in }
    }

    @objc func pressChoiceText() { if choice.isEnabled { choice.performClick(nil) } }

    @objc func copyDetails() {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(details, forType: .string)
        copyButton.title = "Copied"
        DispatchQueue.main.asyncAfter(deadline: .now() + 2) { [weak self] in self?.copyButton.title = "Copy details" }
    }

    /// Asks for the plan and draws it; then gets it, or nil when potluck did
    /// not answer one (shown).
    func ask(_ then: @escaping (ActionAnswer?) -> Void) {
        if asking { return }
        asking = true
        drawButtons()
        let args = planArgs
        DispatchQueue.global(qos: .userInitiated).async {
            let (code, out, err) = runPotluck(args)
            // Only a plan has a view: another program's answer is none.
            let a = code == 0 ? actionAnswer(in: out).flatMap { $0.left == nil || $0.view == nil ? nil : $0 } : nil
            let why = code == 0 ? "potluck answered no plan" : commandTrouble(err, code).raw
            DispatchQueue.main.async {
                self.asking = false
                if args != self.planArgs { return self.ask(then) } // reopened meanwhile
                self.plan = a
                if let v = a?.view { self.draw(v) } else { self.drawUnknown(why, old: code == 0) }
                self.drawButtons()
                then(a)
            }
        }
    }

    /// The plan v, under how the last removal stopped, when one did.
    func draw(_ v: UninstallView) {
        head(outcome?.heading ?? v.heading, outcome?.lead ?? v.lead ?? "")
        warn(v.warning ?? "")
        if let d = outcome == nil ? v.details : nil { details = d }
        choiceText.stringValue = v.choice?.label ?? ""
        choice.setAccessibilityLabel(v.choice?.label)
        choice.state = v.choice?.on == true ? .on : .off
        choiceRow.isHidden = v.choice == nil
        // A step repeated only because repeating it is safe is not to do.
        let did = outcome?.done ?? []
        let todo = v.left.filter { s in !did.contains { $0.id == s.id } }
        fill([("Done so far", "✓", (outcome?.done ?? []).map(\.did)), (outcome == nil ? "What will happen" : "Still to do", "•", todo.map(\.will)), ("What stays on this Mac", "•", v.stays)])
    }

    func drawUnknown(_ why: String, old: Bool) {
        if outcome != nil {
            lead.stringValue += " Press Check again first: Potluck couldn't say what is left."
            lead.isHidden = false
            details += "\n" + why
        } else {
            details = why
            head("Potluck can't say what it would remove", old ? "The Potluck on this Mac may be older than this app. Update Potluck, then press Check again." : "Try again in a moment. If it still fails, copy the details and send them to the person who invited you.")
        }
        warn("")
        choiceRow.isHidden = true
        fill([("Done so far", "✓", (outcome?.done ?? []).map(\.did))])
    }

    func drawDone(_ last: UninstallView) {
        let v = last.after(before)
        head(v.heading, v.lead ?? "")
        warn("")
        say("", error: false)
        details = v.details ?? ""
        choiceRow.isHidden = true
        fill([("Done", "✓", v.done.map(\.did)), ("What stays on this Mac", "•", v.stays)])
    }

    /// How a removal that stopped short stands: potluck's words, else ours.
    func stop(_ v: UninstallView?, failed: Bool, raw: String) {
        let stopped = UninstallView(heading: v?.heading ?? "Potluck couldn't say how the removal went",
                                lead: (failed ? v?.failed : nil) ?? v?.lead ?? "Try removing Potluck again. If it still fails, copy the details and send them to the person who invited you.",
                                failed: nil, warning: nil, choice: nil, done: v?.done ?? [], left: [], stays: [], details: nil).after(before)
        outcome = stopped
        before = stopped
        details = [v?.details ?? "", raw].filter { !$0.isEmpty }.joined(separator: "\n")
    }

    func drawButtons() {
        let here = !removed
        let nothing = plan?.left?.isEmpty == true
        choice.isEnabled = !acting && !asking && force == nil // Remove anyway runs what was refused
        copyButton.isHidden = details.isEmpty || running
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
                let detail = [f.detail, a.view?.warning].compactMap { $0 }.joined(separator: " ")
                self.confirm(OfferedAction(confirm: f.confirm, detail: detail, args: f.args), button: "Remove anyway")
            }
        }
    }

    func start(_ then: () -> Void) {
        guard !acting, !asking, !removed else { return }
        say("", error: false)
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
        outcome = nil
        head("Removing Potluck…", "This can take a few minutes.")
        warn("")
        choiceRow.isHidden = true
        details = ""
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
        runAction(args, admin: runAdmin(over: window), ask: { step, answer in askAdmin(step, over: self.window, answer) }) { r in
            self.acting = false
            self.running = false
            self.say("", error: false)
            var offer: OfferedAction?
            switch r {
            case .done(let p) where p.done != nil && p.left.isEmpty && p.view != nil:
                self.removed = true
                self.drawDone(p.view!)
                self.drawButtons()
                return self.holding(true) // for good: nothing is left to ask
            case .done:
                self.stop(nil, failed: true, raw: "potluck did not say it finished")
            case .failed(let why, let p):
                offer = p.force
                // A last answer that wanted the admin step has words for its failing.
                self.stop(p.view, failed: true, raw: why.raw)
            case .cancelled(let p):
                self.stop(p.view, failed: false, raw: "the admin step was cancelled")
            }
            self.holding(false)
            self.force = offer
            self.ask { _ in }
        }
    }

    @objc func pressTrash() {
        guard !acting, !asking else { return }
        if let why = blocked() { return say(why, error: true) }
        loginOff()
        if let why = trashApp(Bundle.main.bundleURL, trash: trash) { return say(why, error: true) }
        Bundled.app.clear()
        let files = appFiles(home: FileManager.default.homeDirectoryForCurrentUser)
        forgetApp(caches: files.caches, preferences: files.preferences, domain: appBundleID)
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

    func head(_ text: String, _ more: String) {
        title.stringValue = text
        lead.stringValue = more
        lead.isHidden = more.isEmpty
    }

    func fill(_ sections: [(String, String, [String])]) {
        lists.arrangedSubviews.forEach { lists.removeArrangedSubview($0); $0.removeFromSuperview() }
        for (head, mark, lines) in sections where !lines.isEmpty {
            let h = NSTextField(labelWithString: head)
            h.font = .boldSystemFont(ofSize: 13)
            if let last = lists.arrangedSubviews.last { lists.setCustomSpacing(14, after: last) }
            lists.addArrangedSubview(h)
            for l in lines {
                let m = NSTextField(labelWithString: mark)
                m.setContentHuggingPriority(.required, for: .horizontal)
                m.setContentCompressionResistancePriority(.required, for: .horizontal)
                let words = NSTextField(wrappingLabelWithString: l)
                let t = NSStackView(views: [m, words])
                t.orientation = .horizontal
                t.alignment = .firstBaseline
                t.spacing = 6
                for x in [m, words] { x.font = .systemFont(ofSize: 13) }
                lists.addArrangedSubview(t)
                t.widthAnchor.constraint(equalTo: lists.widthAnchor).isActive = true
            }
        }
    }
}
