// The "Set up and check…" window: the setup checklist and the last host
// run's stages, as `potluck --json host status` answers them, and one
// button: the `do` of the check the next step is from (`next_id`). It asks
// again every few seconds while it is open. Every word is the command's; the
// window runs only that status and the do's arguments (runAction, with
// potluck's admin step rule), or opens the do's page.

import AppKit

final class SetupWindow: NSObject {
    let args: [String]
    let window: NSWindow
    let summary = NSTextField(wrappingLabelWithString: "reading…")
    let next = NSTextField(wrappingLabelWithString: "")
    let checks = NSStackView()
    let stages = NSStackView()
    let last = NSTextField(wrappingLabelWithString: "")
    let note = NSTextField(wrappingLabelWithString: "")
    let invite = NSTextField()
    let button = NSButton(title: "", target: nil, action: nil)
    let result = NSTextField(wrappingLabelWithString: "")
    let again = NSButton(title: "Ask again", target: nil, action: nil)
    var timer: Timer?
    var asking = false
    var current: SetupDo? // the next check's do, as last drawn
    var acting = false // an action is running
    var settling = false // an action ended, or the window opened, and no status asked since has been drawn
    var asks = 0 // counts those moments: a status asked before the last one is not current
    /// What Ask again runs: the cancelled action as it was run, kept only
    /// while its do is still the next one and the window is open.
    var cancelled: (from: SetupDo, args: [String])?
    /// A failure is shown: unseen until the first status asked after it is
    /// drawn, then the next do at that time. Another do by then is the
    /// failed action's own doing; one after that makes the failure old.
    enum Failure { case none, unseen, seen(SetupDo?) }
    var failure = Failure.none

    init(args: [String]) {
        self.args = args
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 640, height: 560), styleMask: [.titled, .closable, .resizable, .miniaturizable], backing: .buffered, defer: false)
        super.init()
        window.title = "Set up and check"
        window.isReleasedWhenClosed = false
        window.minSize = NSSize(width: 480, height: 320)
        summary.font = .boldSystemFont(ofSize: 13)
        next.font = .systemFont(ofSize: 12)
        last.font = .monospacedSystemFont(ofSize: 11, weight: .regular)
        last.textColor = .secondaryLabelColor
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
        for s in [checks, stages] {
            s.orientation = .vertical
            s.alignment = .leading
            s.spacing = 6
        }
        let heading = { (t: String) -> NSTextField in
            let f = NSTextField(labelWithString: t)
            f.font = .boldSystemFont(ofSize: 11)
            f.textColor = .secondaryLabelColor
            return f
        }
        let column = NSStackView(views: [summary, next, invite, button, result, again, heading("THIS MAC"), checks, heading("THE LAST SETUP RUN"), stages, last, note])
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
            checks.widthAnchor.constraint(equalTo: column.widthAnchor, constant: -32), stages.widthAnchor.constraint(equalTo: column.widthAnchor, constant: -32),
            invite.widthAnchor.constraint(equalTo: column.widthAnchor, constant: -32), result.widthAnchor.constraint(equalTo: column.widthAnchor, constant: -32),
        ])
        NotificationCenter.default.addObserver(self, selector: #selector(closed), name: NSWindow.willCloseNotification, object: window)
    }

    func show() {
        if timer == nil { // reopened: the last picture may be old
            asks += 1
            settling = true
            drawButton()
        }
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
        let args = self.args
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
                if st != nil && asked == self.asks { self.settling = false }
                self.draw(st, why)
                if st != nil && asked == self.asks { self.ageFailure() }
                if asked != self.asks && self.timer != nil { self.load() } // a failed ask waits for the timer
            }
        }
    }

    func draw(_ st: Status?, _ why: String?) {
        guard let st = st else {
            // Keep the last good picture; say why it is not current.
            note.stringValue = "not current: " + (why ?? "no answer")
            note.textColor = .systemRed
            drawButton()
            return
        }
        note.stringValue = why.map { "not current: " + $0 } ?? ""
        note.textColor = .systemRed
        summary.stringValue = st.menu.title
        guard let setup = st.setup else {
            current = nil
            dropStaleCancel()
            drawButton()
            next.stringValue = "This potluck does not report a setup checklist; update it."
            next.isHidden = false
            return
        }
        next.stringValue = setup.next.map { "Next: " + $0 } ?? ""
        next.isHidden = next.stringValue.isEmpty
        current = setup.next_id.flatMap { id in setup.checks.first { $0.id == id }?.do }
        dropStaleCancel()
        drawButton()
        fill(checks, setup.checks.map { row(mark: SetupWindow.mark($0.state), color: SetupWindow.color($0.state), text: $0.detail, sub: $0.action) })
        let reached = Dictionary((setup.stages ?? []).map { ($0.name, $0) }, uniquingKeysWith: { _, b in b })
        fill(stages, setup.stageList.map { s in
            guard let r = reached[s.name] else { return row(mark: "·", color: .tertiaryLabelColor, text: s.label, sub: nil) }
            let line = r.detail.map { $0.isEmpty ? s.label : s.label + ": " + $0 } ?? s.label
            return row(mark: SetupWindow.stageMark(r.result), color: SetupWindow.stageColor(r.result), text: line, sub: nil)
        })
        last.stringValue = st.background?.last ?? ""
        last.isHidden = last.stringValue.isEmpty
    }

    /// Forgets a cancelled action, with its message, once its do is no
    /// longer the next one.
    func dropStaleCancel() {
        guard let c = cancelled, c.from != current else { return }
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
        invite.isEnabled = button.isEnabled
        invite.isHidden = !(runnable && d?.args != nil && d?.invite == true)
        if invite.isHidden { invite.stringValue = "" }
        again.isHidden = acting || cancelled == nil
    }

    /// The button (or Ask again): open the do's page, or run its arguments
    /// with the pasted invite as the last one when it takes one.
    @objc func press() {
        guard !acting, !settling, let d = current else { return }
        if let u = d.url {
            cancelled = nil
            failure = .none
            say("", error: false)
            drawButton()
            guard let url = SetupWindow.openable(u) else {
                failure = .unseen
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
            self.invite.stringValue = "" // the invite is not kept past its run
            switch r {
            case .done:
                self.say("", error: false)
            case .failed(let why, let p):
                self.say(failText(label, why, p), error: true)
                self.failure = .unseen
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

    /// A mark, the potluck words, and under them the command it names.
    func row(mark: String, color: NSColor, text: String, sub: String?) -> NSView {
        let m = NSTextField(labelWithString: mark)
        m.font = .boldSystemFont(ofSize: 13)
        m.textColor = color
        m.alignment = .center
        m.widthAnchor.constraint(equalToConstant: 16).isActive = true
        let t = NSTextField(wrappingLabelWithString: text)
        t.font = .systemFont(ofSize: 12)
        var views: [NSView] = [t]
        if let s = sub, !s.isEmpty {
            let c = NSTextField(wrappingLabelWithString: s)
            c.font = .monospacedSystemFont(ofSize: 11, weight: .regular)
            c.textColor = .secondaryLabelColor
            c.isSelectable = true
            views.append(c)
        }
        let words = NSStackView(views: views)
        words.orientation = .vertical
        words.alignment = .leading
        words.spacing = 2
        let r = NSStackView(views: [m, words])
        r.orientation = .horizontal
        r.alignment = .top
        r.spacing = 8
        words.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
        return r
    }

    static func mark(_ state: String) -> String {
        switch state {
        case "done": return "✓"
        case "blocked": return "✕"
        case "running": return "…"
        case "todo": return "○"
        case "info": return "i"
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

    static func stageMark(_ result: String) -> String {
        switch result {
        case "succeeded": return "✓"
        case "blocked": return "✕"
        case "running": return "…"
        default: return "?"
        }
    }

    static func stageColor(_ result: String) -> NSColor {
        switch result {
        case "succeeded": return .systemGreen
        case "blocked": return .systemRed
        case "running": return .systemBlue
        default: return .secondaryLabelColor
        }
    }
}

/// A view whose origin is at the top, so a scroll view starts at the top.
final class FlippedView: NSView {
    override var isFlipped: Bool { true }
}
