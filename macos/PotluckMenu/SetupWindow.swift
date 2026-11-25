// The "Set up and check…" window, drawn from the `view` of `potluck --json
// host status`: a heading, what to do, its one button, and the lines under
// them. Every word is the command's. It asks every few seconds while it is
// open, with --probe only when ProbeClock says. It runs only that status
// and a do's arguments (runAction, with potluck's admin step rule), or
// opens the do's page.

import AppKit

final class SetupWindow: NSObject {
    let args: [String]
    let window: NSWindow
    let heading = NSTextField(wrappingLabelWithString: "Checking this Mac…")
    let lead = NSTextField(wrappingLabelWithString: "")
    let lines = NSStackView() // the view's sections
    let progress = NSStackView()
    let check = NSButton(title: "Check again", target: nil, action: nil)
    let copy = NSButton(title: "Copy details", target: nil, action: nil)
    let note = NSTextField(wrappingLabelWithString: "")
    let invite = NSTextField()
    let button = NSButton(title: "", target: nil, action: nil)
    let result = NSTextField(wrappingLabelWithString: "")
    let again = NSButton(title: "Ask again", target: nil, action: nil)
    var timer: Timer?
    var asking = false
    var clock = ProbeClock()
    var current: SetupDo? // the button's do, as last drawn
    var rowButtons: [RowButton] = []
    var details = ""
    var problem = "" // why the last ask failed, for Copy details ("": it did not)
    var drawn = false // an answer has been drawn
    var shownSections: [SetupSection] = []
    var shownProgress: SetupSection?
    var acting = false // an action is running
    var settling = false // an action ended, or the window opened, and no status asked since has been drawn
    var asks = 0 // counts those moments: a status asked before the last one is not current
    /// What Ask again runs: the cancelled action as it was run, kept only
    /// while its do is still the next one and the window is open.
    var cancelled: (from: SetupDo, args: [String])?
    /// A failure is shown: unseen until the first status asked after it is
    /// drawn, then the next do at that time. Another do by then is the
    /// failed action's own doing; one after that makes the failure old.
    /// A line's own button (row) is not the next step: its failure is old
    /// once that button is gone.
    enum Failure { case none, unseen, seen(SetupDo?), row(SetupDo) }
    var failure = Failure.none

    init(args: [String]) {
        self.args = args
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 640, height: 560), styleMask: [.titled, .closable, .resizable, .miniaturizable], backing: .buffered, defer: false)
        super.init()
        window.title = "Set up and check"
        window.isReleasedWhenClosed = false
        window.minSize = NSSize(width: 480, height: 320)
        heading.font = .boldSystemFont(ofSize: 20)
        lead.font = .systemFont(ofSize: 13)
        lead.isHidden = true
        note.font = .systemFont(ofSize: 11)
        note.textColor = .secondaryLabelColor
        invite.placeholderString = "Paste your invite link here"
        invite.font = .systemFont(ofSize: 12)
        invite.isHidden = true
        button.target = self
        button.action = #selector(press)
        button.bezelStyle = .rounded
        button.keyEquivalent = "\r"
        button.isHidden = true
        result.font = .systemFont(ofSize: 12)
        result.isSelectable = true
        result.isHidden = true
        again.target = self
        again.action = #selector(askAgain)
        again.bezelStyle = .rounded
        again.isHidden = true
        check.target = self
        check.action = #selector(checkAgain)
        check.bezelStyle = .rounded
        copy.target = self
        copy.action = #selector(copyDetails)
        copy.bezelStyle = .rounded
        copy.isHidden = true
        for s in [lines, progress] {
            s.orientation = .vertical
            s.alignment = .leading
            s.spacing = 6
        }
        let buttons = NSStackView(views: [button, check, copy])
        buttons.orientation = .horizontal
        buttons.spacing = 8
        let column = NSStackView(views: [heading, lead, invite, buttons, result, again, progress, lines, note])
        column.orientation = .vertical
        column.alignment = .leading
        column.spacing = 10
        column.edgeInsets = NSEdgeInsets(top: 16, left: 16, bottom: 16, right: 16)
        column.translatesAutoresizingMaskIntoConstraints = false
        // The column is the scroll view's document: a long checklist scrolls.
        let flipped = FlippedView()
        flipped.addSubview(column)
        let scroll = NSScrollView()
        scroll.documentView = flipped
        scroll.hasVerticalScroller = true
        scroll.drawsBackground = false
        flipped.translatesAutoresizingMaskIntoConstraints = false
        window.contentView = scroll
        NSLayoutConstraint.activate([
            flipped.leadingAnchor.constraint(equalTo: scroll.contentView.leadingAnchor),
            flipped.trailingAnchor.constraint(equalTo: scroll.contentView.trailingAnchor),
            flipped.topAnchor.constraint(equalTo: scroll.contentView.topAnchor),
            column.leadingAnchor.constraint(equalTo: flipped.leadingAnchor), column.trailingAnchor.constraint(equalTo: flipped.trailingAnchor),
            column.topAnchor.constraint(equalTo: flipped.topAnchor), column.bottomAnchor.constraint(equalTo: flipped.bottomAnchor),
            lines.widthAnchor.constraint(equalTo: column.widthAnchor, constant: -32), progress.widthAnchor.constraint(equalTo: column.widthAnchor, constant: -32),
            lead.widthAnchor.constraint(equalTo: column.widthAnchor, constant: -32),
            invite.widthAnchor.constraint(equalTo: column.widthAnchor, constant: -32), result.widthAnchor.constraint(equalTo: column.widthAnchor, constant: -32),
        ])
        NotificationCenter.default.addObserver(self, selector: #selector(closed), name: NSWindow.willCloseNotification, object: window)
    }

    func show() {
        // The last picture may be old.
        asks += 1
        settling = true
        clock.needed = true
        drawButton()
        window.center()
        NSApp.activate(ignoringOtherApps: true)
        window.makeKeyAndOrderFront(nil)
        load()
        timer?.invalidate()
        timer = Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { [weak self] _ in self?.load() }
    }

    @objc func closed() {
        timer?.invalidate()
        timer = nil
        // Nothing pasted outlives the window, nor does an old result.
        invite.stringValue = ""
        cancelled = nil
        failure = .none
        if !acting { say("", error: false) }
        drawButton()
    }

    func load() {
        if asking { return }
        asking = true
        let probe = clock.probes(at: Date())
        let args = probe ? self.args : self.args.filter { $0 != "--probe" }
        let asked = asks
        DispatchQueue.global(qos: .userInitiated).async {
            let (code, out, err) = runPotluck(args)
            var st: Status?
            var why: String?
            if code == 0 {
                do { st = try JSONDecoder().decode(Status.self, from: out) } catch { why = "potluck host status: \(error.localizedDescription)" }
            } else {
                why = err.isEmpty ? "potluck host status exited \(code)" : err
            }
            DispatchQueue.main.async {
                self.asking = false
                let current = asked == self.asks
                if st != nil && current {
                    self.clock.answered(st?.setup?.view?.key, probed: probe)
                    // A step that may have changed is not pressed before the probe that follows.
                    self.settling = probe ? false : self.settling || self.clock.needed
                } else if probe {
                    self.clock.missed()
                }
                self.draw(st, why, whole: probe)
                if probe { self.check.title = "Check again" }
                if st != nil && current && probe { self.ageFailure() } // only a probe draws the next step
                if st != nil && (!current || self.clock.needed) && self.timer != nil { self.load() } // a failed ask waits for the timer
            }
        }
    }

    /// Draws an answer: whole when it was a probe, else only the progress of
    /// the window the last probe drew.
    func draw(_ st: Status?, _ why: String?, whole: Bool) {
        guard let st = st else {
            // Keep the last good picture; say why it is not current.
            problem = "potluck host status: " + (why ?? "no answer")
            note.stringValue = "Potluck didn't answer just now, so this may be out of date. Trying again… If this stays, press Copy details and send what it copies to the person who invited you."
            note.textColor = .systemRed
            if !drawn {
                heading.stringValue = "Potluck isn't answering"
                note.stringValue = "Trying again… If this stays, press Copy details and send what it copies to the person who invited you."
            }
            drawButton()
            return
        }
        problem = ""
        note.stringValue = ""
        guard let v = st.setup?.view else {
            guard whole else { return }
            heading.stringValue = "Potluck needs updating"
            lead.stringValue = "This window needs a newer Potluck than the one on this Mac. Press Copy details and send what it copies to the person who invited you."
            lead.isHidden = false
            details = "The Potluck on this Mac is older than its menu bar item (host status has no setup.view): update potluck."
            drawSections([])
            drawProgress(nil)
            current = nil
            drawn = true
            dropStaleCancel()
            drawButton()
            return
        }
        // Another key: the probe that follows at once draws all of it together.
        if whole || v.key == clock.key { drawProgress(v.progress) }
        guard whole else { return drawButton() }
        heading.stringValue = v.heading
        lead.stringValue = v.lead ?? ""
        lead.isHidden = lead.stringValue.isEmpty
        details = v.details ?? ""
        current = v.do
        drawSections(v.sections)
        drawn = true
        dropStaleCancel()
        drawButton()
    }

    /// Draws the view's sections, and progress, only when they changed: a
    /// redraw takes the keyboard's and VoiceOver's place in them away.
    func drawSections(_ s: [SetupSection]) {
        if s == shownSections { return }
        shownSections = s
        rowButtons = []
        fill(lines, s.flatMap { section($0) })
    }

    func drawProgress(_ s: SetupSection?) {
        if s == shownProgress { return }
        shownProgress = s
        fill(progress, s.map { section($0) } ?? [])
    }

    /// Forgets a cancelled action, with its message, once its do is no
    /// longer offered.
    func dropStaleCancel() {
        guard let c = cancelled, c.from != current, !rowButtons.contains(where: { $0.does == c.from }) else { return }
        cancelled = nil
        say("", error: false)
    }

    /// Clears a failure's red text once the next step is no longer the one
    /// it was shown beside.
    func ageFailure() {
        switch failure {
        case .none: return
        case .unseen: failure = .seen(current)
        case .seen(let d):
            guard d != current, !acting else { return }
            failure = .none
            say("", error: false)
        case .row(let d):
            guard !acting, !rowButtons.contains(where: { $0.does == d }) else { return }
            failure = .none
            say("", error: false)
        }
    }

    /// The button from the next check's do: its label; an invite field when
    /// it takes one. A do only the app can act on (`app`) gets no button yet.
    func drawButton() {
        let d = current
        let runnable = d.map { $0.args != nil || $0.url != nil } ?? false
        button.title = d?.label ?? ""
        button.isHidden = !runnable
        button.isEnabled = !acting && !settling
        again.isEnabled = button.isEnabled
        for b in rowButtons { b.isEnabled = button.isEnabled }
        invite.isEnabled = !acting
        check.isEnabled = !acting
        copy.isHidden = details.isEmpty && problem.isEmpty
        invite.isHidden = !(runnable && d?.args != nil && d?.invite == true)
        again.isHidden = acting || cancelled == nil
    }

    /// Check again: ask with --probe now.
    @objc func checkAgain() {
        clock.needed = true
        check.title = "Checking…"
        load()
    }

    @objc func copyDetails() {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString([details, problem].filter { !$0.isEmpty }.joined(separator: "\n"), forType: .string)
        copy.title = "Copied"
        DispatchQueue.main.asyncAfter(deadline: .now() + 2) { [weak self] in self?.copy.title = "Copy details" }
    }

    /// The button: its do, with the pasted invite when it takes one.
    @objc func press() {
        if let d = current { perform(d) }
    }

    @objc func pressRow(_ b: RowButton) { perform(b.does) }

    /// Opens the do's page, or runs its arguments with the pasted invite as
    /// the last one when it takes one.
    func perform(_ d: SetupDo) {
        guard !acting, !settling else { return }
        if let u = d.url {
            cancelled = nil
            failure = .none
            say("", error: false)
            drawButton()
            guard let url = SetupWindow.openable(u) else {
                failure = d == current ? .unseen : .row(d)
                return say("potluck named a page this app does not open: " + u, error: true)
            }
            NSWorkspace.shared.open(url)
            return
        }
        guard var args = d.args else { return }
        if d.invite == true {
            let text = invite.stringValue.trimmingCharacters(in: .whitespacesAndNewlines)
            if !text.isEmpty { args.append(text) } // after the do's "--": never read as a flag
        }
        act(d, args)
    }

    /// Ask again: the action whose admin step was cancelled, with the very
    /// arguments it ran with (its invite too, though the field is empty).
    @objc func askAgain() {
        guard !acting, !settling, let c = cancelled else { return }
        act(c.from, c.args)
    }

    func act(_ d: SetupDo, _ args: [String]) {
        let label = d.label
        let row = d != current
        acting = true
        cancelled = nil
        failure = .none
        drawButton()
        say("\(label)…", error: false)
        runAction(args, ask: { [weak self] step, answer in
            guard let self = self else { return answer(false) }
            askAdmin(step, over: self.window, answer)
        }) { [weak self] r in
            guard let self = self else { return }
            self.acting = false
            self.asks += 1
            self.settling = true
            self.clock.needed = true
            if d.invite == true { self.invite.stringValue = "" } // the invite is not kept past its run
            switch r {
            case .done:
                self.say("", error: false)
            case .failed(let why, let p):
                self.say(failText(label, why, p), error: true)
                self.failure = row ? .row(d) : .unseen
            case .cancelled(let why, let p):
                self.say(cancelText(why, p), error: false)
                if self.timer != nil { self.cancelled = (d, args) } // closed meanwhile: nothing to ask again from
            }
            self.drawButton()
            self.load()
        }
    }

    func say(_ text: String, error: Bool) {
        result.stringValue = text
        result.textColor = error ? .systemRed : .labelColor
        result.isHidden = text.isEmpty
    }

    /// The URL a do may open: an https page, or a file URL of an app (the
    /// menu bar item potluck found installed).
    static func openable(_ s: String) -> URL? {
        guard let u = URL(string: s) else { return nil }
        if u.scheme == "https", u.host != nil { return u }
        if u.isFileURL, u.path.hasSuffix(".app") { return u }
        return nil
    }

    func fill(_ stack: NSStackView, _ rows: [NSView]) {
        stack.arrangedSubviews.forEach { stack.removeArrangedSubview($0); $0.removeFromSuperview() }
        for r in rows {
            stack.addArrangedSubview(r)
            r.widthAnchor.constraint(equalTo: stack.widthAnchor).isActive = true // the words wrap to the window
        }
    }

    /// A section's title and lines; none when it has no title.
    func section(_ s: SetupSection) -> [NSView] {
        if s.title.isEmpty { return [] }
        let t = NSTextField(labelWithString: s.title)
        t.font = .boldSystemFont(ofSize: 12)
        t.textColor = .secondaryLabelColor
        let title = NSStackView(views: [t])
        title.alignment = .bottom
        title.heightAnchor.constraint(equalToConstant: 26).isActive = true
        return [title] + (s.rows ?? []).map { row($0) }
    }

    /// A line: its mark, its words, and its own button.
    func row(_ r: SetupRow) -> NSView {
        let m = NSTextField(labelWithString: SetupWindow.mark(r.state))
        m.font = .boldSystemFont(ofSize: 13)
        m.textColor = SetupWindow.color(r.state)
        m.alignment = .center
        m.widthAnchor.constraint(equalToConstant: 16).isActive = true
        m.cell?.setAccessibilityElement(false) // the words say it
        let t = NSTextField(wrappingLabelWithString: r.text)
        t.font = .systemFont(ofSize: 13)
        t.cell?.setAccessibilityValue("\(r.text), \(SetupWindow.spoken(r.state))")
        if r.state.isEmpty { t.textColor = .secondaryLabelColor }
        t.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
        var views: [NSView] = [m, t]
        if let d = r.do, d.args != nil || d.url != nil {
            let b = RowButton(d, target: self, action: #selector(pressRow))
            b.isEnabled = !acting && !settling
            rowButtons.append(b)
            views.append(b)
        }
        let line = NSStackView(views: views)
        line.orientation = .horizontal
        line.alignment = .firstBaseline
        line.spacing = 8
        return line
    }

    static func spoken(_ state: String) -> String {
        switch state {
        case "done": return "done"
        case "blocked": return "needs attention"
        case "running": return "going on"
        case "todo", "": return "not done yet"
        case "info": return "for your information"
        default: return "not known"
        }
    }

    static func mark(_ state: String) -> String {
        switch state {
        case "done": return "✓"
        case "blocked": return "✕"
        case "running": return "…"
        case "todo": return "○"
        case "info": return "–"
        case "": return "·"
        default: return "?"
        }
    }

    static func color(_ state: String) -> NSColor {
        switch state {
        case "done": return .systemGreen
        case "blocked": return .systemRed
        case "running": return .systemBlue
        case "todo": return .systemOrange
        default: return .secondaryLabelColor
        }
    }
}

/// A line's own button, with the do it is for.
final class RowButton: NSButton {
    let does: SetupDo

    init(_ d: SetupDo, target: AnyObject, action: Selector) {
        does = d
        super.init(frame: .zero)
        title = d.label
        bezelStyle = .rounded
        controlSize = .small
        self.target = target
        self.action = action
    }

    required init?(coder: NSCoder) { nil }
}

/// A view whose origin is at the top, so a scroll view starts at the top.
final class FlippedView: NSView {
    override var isFlipped: Bool { true }
}
