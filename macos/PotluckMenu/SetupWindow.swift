// The "Set up and check…" window, drawn from the `view` of `potluck --json
// host status`: a heading, what to do, its one button, and the lines under
// them. Every word is the command's. It asks every few seconds while it is
// open, with --probe only when ProbeClock says. It runs only that status
// and a do's arguments (runAction, with potluck's admin step rule), or
// opens the do's page.

import AppKit

final class SetupWindow: NSObject, NSTextFieldDelegate {
    let args: [String]
    let window: NSWindow
    let heading = NSTextField(wrappingLabelWithString: "Checking this Mac…")
    let lead = NSTextField(wrappingLabelWithString: "")
    let lines = NSStackView() // the view's sections
    let progress = NSStackView()
    let check = NSButton(title: "Check again", target: nil, action: nil)
    let copy = NSButton(title: "Copy details", target: nil, action: nil)
    let inviteNote = NSTextField(wrappingLabelWithString: "")
    let note = NSTextField(wrappingLabelWithString: "")
    let invite = NSTextField()
    let button = NSButton(title: "", target: nil, action: nil)
    let result = NSTextField(wrappingLabelWithString: "")
    let again = NSButton(title: "Try again", target: nil, action: nil)
    var timer: Timer?
    var asking = false
    var askAnew = false // Check again was pressed while an ask went on
    var asks = SetupAsks()
    /// Does one of this app's own acts; then gets why it did not, or nil.
    var appAct: (String, @escaping (Trouble?) -> Void) -> Void = { _, done in done(nil) }
    var actProblem = "" // an action's failure as the program or system put it, for Copy details
    var given: String? // the last invite a run was given
    var givenOver: String? // the run the last answer told of when it was (inviteSpent)
    var lastRun: String? // the run the last answer told of
    var linked: String? // the field's text, when a potluck:// link put it there
    var offered: String? // a link's invite whose question is up
    var current: SetupDo? // the button's do, as last drawn
    var rowButtons: [RowButton] = []
    var details = ""
    var problem = "" // why the last ask failed, for Copy details ("": it did not)
    var drawn = false // an answer has been drawn, and its Potluck is still there
    var drawnBy: String? // the program whose answer it was
    var shownSections: [SetupSection] = []
    var shownProgress: SetupSection?
    var acting = false // an action is running
    var updated = false // Update Potluck ran
    /// What Try again runs: the cancelled action as it was run (its invite too), kept only
    /// while its do is still the next one and the window is open.
    var cancelled: (from: SetupDo, run: PotluckRun)?
    var failure: SetupFailure? // the one shown
    var screen: SetupScreen? // the last drawn

    init(args: [String]) {
        self.args = args
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 640, height: 590), styleMask: [.titled, .closable, .resizable, .miniaturizable], backing: .buffered, defer: false)
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
        invite.delegate = self
        inviteNote.font = .systemFont(ofSize: 12)
        inviteNote.isHidden = true
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
        let welcome = setupWelcome()
        let divider = NSBox()
        divider.boxType = .separator
        let column = NSStackView(views: [welcome, divider, heading, lead, invite, inviteNote, buttons, result, again, progress, lines, note])
        column.orientation = .vertical
        column.alignment = .leading
        column.spacing = 10
        column.setCustomSpacing(16, after: welcome)
        column.setCustomSpacing(20, after: divider)
        let gutter: CGFloat = 28
        column.edgeInsets = NSEdgeInsets(top: 20, left: gutter, bottom: 24, right: gutter)
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
        ] + [welcome, divider, heading, lead, invite, inviteNote, result, progress, lines, note].map {
            $0.widthAnchor.constraint(equalTo: column.widthAnchor, constant: -2 * gutter)
        })
        NotificationCenter.default.addObserver(self, selector: #selector(closed), name: NSWindow.willCloseNotification, object: window)
    }

    func show() {
        // The last picture may be old.
        asks.changed()
        drawButton()
        window.center()
        Dock.show(window)
        load()
        timer?.invalidate()
        timer = Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { [weak self] _ in self?.load() }
    }

    @objc func closed() {
        timer?.invalidate()
        timer = nil
        // An old result does not outlive the window; the invite does (in
        // memory only), until setup went through.
        actProblem = ""
        cancelled = nil
        failure = nil
        updated = false
        askAnew = false
        if !acting { say("", error: false) }
        check.title = "Check again"
        drawButton()
    }

    /// The person changed the field: it is theirs again.
    func controlTextDidChange(_ n: Notification) { drawInvite() }

    /// Asks potluck and draws the answer. again: the person asked, so a
    /// refused release in this app is checked once more.
    func load(again: Bool = false) {
        askAnew = askAnew || again
        if asking || Bundled.app.preparing { return } // the timer's next ask is made in its place
        // The app's own copy can do nothing once the app is gone from where it was.
        if Bundled.app.moved && noPotluckHere() {
            check.title = "Check again"
            return drawNone(nil)
        }
        asking = true
        let again = askAnew
        askAnew = false
        let probe = asks.clock.probes(at: Date())
        // A probe reads Tart and Softnet as well as what a plain ask reads.
        let limit: TimeInterval = probe ? 60 : 25
        let args = probe ? self.args : self.args.filter { $0 != "--probe" }
        var asked = asks.turn
        DispatchQueue.global(qos: .userInitiated).async {
            // Potluck went since the app started (a removal in Terminal, a
            // cleared cache): the release the app carries is used again.
            var prepared = false
            if noPotluckHere(), Bundled.app.wanted(again: again) {
                prepared = true
                Bundled.app.preparing = true
                DispatchQueue.main.sync { // it takes a few seconds: what is drawn is of the one that went
                    self.asks.changed()
                    asked = self.asks.turn
                    self.drawn = false
                    self.current = nil
                    self.heading.stringValue = "Checking this Mac…"
                    self.note.stringValue = ""
                    self.drawNoSteps()
                    self.drawButton()
                }
                Bundled.app.prepare()
            }
            let program = potluckPath()
            var (code, out, err) = runPotluck(args, limit: limit, program: program)
            if let plain = withoutProbe(args, after: code) { (code, out, err) = runPotluck(plain, limit: limit, program: program) }
            let canUpdate = program != nil && program == installedPotluck() && (program! as NSString).lastPathComponent == "potluck"
            var st: Status?
            var why: String?
            if code == 0 {
                do { st = try JSONDecoder().decode(Status.self, from: out) } catch { why = "potluck host status: \(error.localizedDescription)" }
            } else {
                why = err.isEmpty ? "potluck host status exited \(code)" : err
            }
            DispatchQueue.main.async {
                self.asking = false
                if prepared { Bundled.app.vanished() }
                // It went while it was asked, and the app carries another.
                if noPotluckHere(), Bundled.app.wanted(again: false), self.timer != nil {
                    self.asks.missed(probed: probe)
                    return self.load()
                }
                var current = false
                if st != nil { current = self.asks.answered(st?.setup?.view?.key, probed: probe, asked: asked) } else { self.asks.missed(probed: probe) }
                self.draw(st, why, whole: probe, program: program, canUpdate: canUpdate)
                if probe { self.check.title = "Check again" }
                if st != nil && current && probe { self.ageFailure() } // only a probe draws the steps
                if st != nil && potluckPath() != nil && (!current || self.asks.clock.needed) && self.timer != nil { self.load() } // a failed ask, or no potluck, waits for the timer
            }
        }
    }

    /// Draws an answer: whole when it was a probe, else only the progress of
    /// the window the last probe drew.
    func draw(_ st: Status?, _ why: String?, whole: Bool, program: String?, canUpdate: Bool) {
        if potluckPath() == nil || Bundled.app.moved && noPotluckHere() { return drawNone(why) }
        guard let st = st else {
            // Keep the last good picture; say why it is not current.
            problem = "potluck host status: " + (why ?? "no answer")
            note.stringValue = "Trying again… This status may be out of date. If the problem continues, copy the details and send them to the person who invited you."
            note.textColor = .systemRed
            if !drawn {
                current = nil
                drawNoSteps()
                heading.stringValue = "Potluck isn't answering"
                note.stringValue = "Trying again… If the problem continues, copy the details and send them to the person who invited you."
            }
            drawButton()
            return
        }
        problem = ""
        note.stringValue = ""
        lastRun = st.setup?.started
        let s = setupScreen(st, canUpdate: canUpdate, updated: updated)
        // Another key: the probe that follows at once draws all of it together.
        if whole || st.setup?.view?.key == asks.clock.key { drawProgress(s.progress) }
        guard whole else { return drawButton() }
        screen = s
        heading.stringValue = s.heading
        lead.stringValue = s.lead
        lead.isHidden = s.lead.isEmpty
        details = s.details
        current = s.act
        if inviteSpent(given: given, over: givenOver, field: invite.stringValue, st) { clearInvite() }
        drawSections(s.sections)
        drawn = true
        drawnBy = program
        dropStaleCancel()
        drawButton()
    }

    /// Draws that there is no potluck to ask, and the way on from there.
    func drawNone(_ why: String?) {
        let n = noPotluckScreen(refused: Bundled.app.refused, moved: Bundled.app.moved, records: daemonRecords())
        drawn = false // the last picture is of a Potluck that is gone
        heading.stringValue = n.heading
        note.stringValue = n.words
        note.textColor = .labelColor
        problem = Bundled.app.refused ?? why.map { "potluck host status: " + $0 } ?? n.words
        current = n.act
        drawNoSteps()
        asks.nothingToAsk()
        dropStaleCancel()
        drawButton()
    }

    /// Takes the last answer's words and steps away.
    func drawNoSteps() {
        screen = nil
        details = ""
        lead.isHidden = true
        drawSections([])
        drawProgress(nil)
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

    /// Clears a failure's red text, and its details, once it is old.
    func ageFailure() {
        guard !acting, let f = failure, let s = screen, !f.stands(on: s) else { return }
        failure = nil
        actProblem = ""
        say("", error: false)
        drawButton()
    }

    /// Shows why f's action failed, with the raw words for Copy details.
    func failed(_ f: SetupFailure, _ t: Trouble, _ text: String) {
        failure = f
        actProblem = t.detail.isEmpty ? "" : f.act.label + ": " + t.detail
        say(text, error: true)
    }

    /// The button from the next check's do: its label; an invite field when
    /// it takes one. A do this app does not know gets no button.
    func drawButton() {
        let d = current
        let runnable = d?.known ?? false
        button.title = d?.label ?? ""
        button.isHidden = !runnable
        button.isEnabled = !acting && !asks.settling
        again.isEnabled = button.isEnabled
        for b in rowButtons { b.isEnabled = button.isEnabled }
        invite.isEnabled = !acting
        check.isEnabled = !acting
        copy.isHidden = details.isEmpty && problem.isEmpty && actProblem.isEmpty
        invite.isHidden = !(runnable && d?.args != nil && d?.invite_args != nil)
        again.isHidden = acting || cancelled == nil
        drawInvite()
    }

    /// Check again: ask with --probe now.
    @objc func checkAgain() {
        asks.clock.needed = true
        check.title = "Checking…"
        load(again: true)
    }

    @objc func copyDetails() {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString([details, problem, actProblem].filter { !$0.isEmpty }.joined(separator: "\n"), forType: .string)
        copy.title = "Copied"
        DispatchQueue.main.asyncAfter(deadline: .now() + 2) { [weak self] in self?.copy.title = "Copy details" }
    }

    /// The button: its do.
    @objc func press() {
        if let d = current { perform(d) }
    }

    @objc func pressRow(_ b: RowButton) { perform(b.does) }

    /// Opens the do's page, or runs it with the pasted invite when it
    /// takes one.
    func perform(_ d: SetupDo) {
        guard !acting, !asks.settling else { return }
        if let u = d.url {
            cancelled = nil
            failure = nil
            actProblem = ""
            say("", error: false)
            drawButton()
            guard let url = SetupWindow.openable(u) else {
                let t = Trouble(words: "Potluck named a page this app doesn't open. Press Copy details and send what it copies to the person who invited you.", detail: "potluck named a page this app does not open: " + u)
                failed(SetupFailure(d, on: screen), t, t.words)
                return drawButton()
            }
            NSWorkspace.shared.open(url)
            return
        }
        if let what = d.app { return actHere(d, what) }
        guard let run = d.run(pasted: invite.stringValue), sameProgram() else { return }
        act(d, run)
    }

    /// Whether the potluck that would run now is the one that drew what was
    /// pressed. Another one draws its own first: what one offers may be
    /// wrong for another (the app's own copy starts no daemon to stay).
    func sameProgram() -> Bool {
        if potluckPath() == drawnBy { return true }
        asks.changed()
        drawButton()
        load()
        return false
    }

    /// One of this app's own acts.
    func actHere(_ d: SetupDo, _ what: String) {
        guard d.known else { return }
        let pressed = SetupFailure(d, on: screen)
        acting = true
        cancelled = nil
        failure = nil
        actProblem = ""
        say("", error: false)
        drawButton()
        appAct(what) { [weak self] e in
            guard let self = self else { return }
            self.acting = false
            self.asks.changed()
            if let e = e { self.failed(pressed, e, e.words) }
            self.drawButton()
            self.load()
        }
    }

    /// An invite from a potluck:// link, which any web page can open: it is
    /// put in the field, over other text only after a yes, and nothing runs.
    func offer(_ link: String) {
        show()
        if acting { return say("Wait for the current step to finish, then open your invite again.", error: false) }
        guard offered == nil, window.attachedSheet == nil else { return } // a question is up: it stands
        switch inviteFill(field: invite.stringValue, invite: link) {
        case .fill: fill(link)
        case .ask:
            offered = link
            let a = NSAlert()
            a.messageText = "Use the invite from the link instead?"
            a.informativeText = "There's already an invite here. Keep it unless you opened the new link yourself."
            a.addButton(withTitle: "Keep the one that's here")
            a.addButton(withTitle: "Use the link's invite").keyEquivalent = ""
            a.beginSheetModal(for: window) { r in
                self.offered = nil
                if r == .alertSecondButtonReturn { self.fill(link) }
            }
        }
    }

    func clearInvite() {
        invite.stringValue = ""
        linked = nil
        given = nil
        givenOver = nil
    }

    func fill(_ link: String) {
        invite.stringValue = link
        linked = link
        drawButton()
    }

    /// What a link's invite in the field means now. While it is there Return
    /// does not press the button: a page may open the link while the person
    /// is typing, so it takes a click.
    func drawInvite() {
        let has = linked.map(invite.stringValue.contains) ?? false // a space typed after it changes nothing
        if !has { linked = nil }
        button.keyEquivalent = has ? "" : "\r"
        // One a run was given is in use, or was: nothing to say of it while the field is hidden.
        inviteNote.isHidden = !has || !drawn || invite.isHidden && sameInvite(given, invite.stringValue)
        if !invite.isHidden {
            inviteNote.stringValue = "Use this invite only if you know who sent it."
        } else if screen?.inviteLater == true {
            inviteNote.stringValue = "Potluck will keep this invite until setup needs it."
        } else {
            inviteNote.stringValue = "You don’t need an invite right now."
        }
    }

    /// Try again: the action whose admin step was cancelled, run the very
    /// way it was (its invite too, whatever the field holds now).
    @objc func askAgain() {
        guard !acting, !asks.settling, let c = cancelled, sameProgram() else { return }
        act(c.from, c.run)
    }

    func act(_ d: SetupDo, _ run: PotluckRun) {
        let label = d.label
        let pressed = SetupFailure(d, on: screen) // the screen changes while it runs
        let over = lastRun
        acting = true
        cancelled = nil
        failure = nil
        actProblem = ""
        drawButton()
        say("\(label)…", error: false)
        runAction(run.args, stdin: run.stdin, admin: runAdmin(over: window), ask: { [weak self] step, answer in
            guard let self = self else { return answer(false) }
            askAdmin(step, over: self.window, answer)
        }) { [weak self] r in
            guard let self = self else { return }
            self.acting = false
            self.asks.changed()
            if d == updatePotluck, case .done = r { self.updated = true }
            switch r {
            case .done:
                if let i = run.stdin { (self.given, self.givenOver) = (i, over) } // else no run began with it
                self.say("", error: false)
            case .failed(let why, let p):
                self.failed(pressed, why, failText(label, why.words, p))
            case .cancelled(let p):
                self.say(cancelText(p), error: false)
                if self.timer != nil { self.cancelled = (d, run) } // closed meanwhile: nothing to ask again from
            }
            self.drawButton()
            self.load()
        }
    }

    func say(_ text: String, error: Bool) {
        result.stringValue = text
        result.textColor = error ? .systemRed : .labelColor
        result.isHidden = text.isEmpty
        if text.isEmpty { return }
        // A line's own button may be far below these words.
        window.layoutIfNeeded()
        result.scrollToVisible(result.bounds)
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
        if let d = r.do, d.known {
            let b = RowButton(d, target: self, action: #selector(pressRow))
            b.isEnabled = !acting && !asks.settling
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
