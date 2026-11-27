// What the setup window shows of an answer, when a failure it shows is old,
// and when its buttons may be pressed: its rules, apart from its drawing
// (SetupWindow.swift). Plain Foundation, so macos/PotluckMenu/test.sh
// compiles and tests it.

import Foundation

/// The window, from one answer of `host status`.
struct SetupScreen {
    var heading: String
    var lead: String
    var act: SetupDo?
    var sections: [SetupSection]
    var progress: SetupSection?
    var details: String
    var checks: [SetupCheck]
    /// A step still to come takes an invite: one from a link is kept for it.
    var inviteLater = false

    /// What can be pressed on it.
    var offers: [SetupDo] { [act].compactMap { $0 } + sections.flatMap { $0.rows ?? [] }.compactMap(\.do) }
}

/// Gets the newest Potluck: the one action that is this app's own choice,
/// only for a Potluck too old to say what to do.
let updatePotluck = SetupDo(label: "Update Potluck", args: ["--json", "upgrade"], invite_args: nil, invite: nil, url: nil, app: nil, auto: nil)

/// The button of a window with no Potluck to ask or run.
let getPotluck = SetupDo(label: "Get Potluck", args: nil, invite_args: nil, invite: nil, url: "https://potluck.sh/", app: nil, auto: nil)

/// The window when there is no Potluck to run and the app's own could not be
/// used: why, in its words, and the one way on it can open.
func noPotluckScreen(refused: String?, moved: Bool, records: [DaemonRecord]) -> (heading: String, words: String, act: SetupDo?) {
    if !records.isEmpty { return ("Potluck isn't running", noProgramWords(records), nil) } // the app's own is not used beside another program's
    if moved { return ("Potluck can't go on", movedWords, getPotluck) }
    if refused != nil { return ("Potluck can't be set up from this app", bundledRefusedWords, nil) }
    return ("Potluck isn't on this Mac", "Get Potluck, or ask the person who invited you. This window follows by itself.", getPotluck)
}

/// Whether the field's invite is spent: a run was given it (as it went to
/// stdin), and setup went through and asks for none, or it was refused where
/// it is from. Until then the field keeps it, so a setup that stops needs no
/// second paste. over is the run the status told of at that press (its
/// `started`): what st says of that one is no verdict on this invite. A
/// Potluck older than `started` tells of none: setup that went through is one.
func inviteSpent(given: String?, over: String?, field: String, _ st: Status) -> Bool {
    guard sameInvite(given, field), let v = st.setup?.view else { return false }
    if let run = st.setup?.started, run == over { return false }
    return st.setup?.state == "succeeded" && v.do?.invite_args == nil || v.invite_refused == true
}

/// Whether the field holds the invite a run was given (nil: none was).
func sameInvite(_ given: String?, _ field: String) -> Bool {
    given?.trimmingCharacters(in: .whitespacesAndNewlines) == field.trimmingCharacters(in: .whitespacesAndNewlines)
}

/// Whether st is from a Potluck older than this app can draw: its setup has
/// no view, or takes the invite as an argument, which this app never gives.
func needsUpdating(_ st: Status) -> Bool {
    guard let v = st.setup?.view else { return true }
    return v.do?.argvInvite == true
}

/// The window st draws. canUpdate: the program asked is potluck itself,
/// which has `upgrade`; updated: it was run from here already.
func setupScreen(_ st: Status, canUpdate: Bool, updated: Bool) -> SetupScreen {
    let checks = st.setup?.checks ?? []
    if let v = st.setup?.view, !needsUpdating(st) {
        return SetupScreen(heading: v.heading, lead: v.lead ?? "", act: v.do, sections: v.sections, progress: v.progress, details: v.details ?? "", checks: checks,
                           inviteLater: v.invite_later ?? (v.do != nil)) // a Potluck from before invite_later: as this app said then
    }
    var lead = "This window needs a newer Potluck than the one on this Mac. "
    switch (canUpdate, updated) {
    case (false, _): lead += "Press Copy details and send what it copies to the person who invited you."
    case (true, false): lead += "Press the button to get the newest one. It takes a minute or two."
    case (true, true): lead = "Potluck looked for its newest version, and this window still needs a newer one. Try again later, or press Copy details and send what it copies to the person who invited you."
    }
    let why = "The Potluck on this Mac is older than its menu bar item (host status has no setup.view, or its setup takes the invite as an argument): update potluck."
    var sections = (st.setup?.view?.sections ?? []).map { s in
        SetupSection(title: s.title, rows: s.rows?.map { $0.do?.argvInvite == true ? SetupRow(state: $0.state, text: $0.text, do: nil) : $0 })
    }
    if st.setup?.view == nil, let d = checks.first(where: { $0.id == "updates" })?.do, d.args != nil, !d.argvInvite {
        sections = [SetupSection(title: "Optional", rows: [SetupRow(state: "info", text: "Potluck isn't updating itself", do: d)])]
    }
    return SetupScreen(heading: "Potluck needs updating", lead: lead, act: canUpdate ? updatePotluck : nil, sections: sections, progress: nil,
                       details: [why, st.setup?.view?.details ?? ""].filter { !$0.isEmpty }.joined(separator: "\n"), checks: checks)
}

/// A failure the window shows, and what it follows: the check its action
/// belongs to, or, for an action no check has, the action itself.
struct SetupFailure {
    let act: SetupDo
    let id: String?
    /// Its check's state when it was pressed.
    let state: String?

    /// act, pressed on screen s, should it fail.
    init(_ act: SetupDo, on s: SetupScreen?) {
        self.act = act
        let c = s?.checks.first { $0.do?.sameAct(as: act) == true }
        (id, state) = (c?.id, c?.state)
    }

    /// Whether it still stands on s, an answer asked after it: while its
    /// button is offered, or its check is as it was when pressed (whatever
    /// step is next, and whatever its button is by then). A check that has
    /// changed (done, going on, or stopped in a way it now says itself) ends
    /// it; one that could not be read is no verdict.
    func stands(on s: SetupScreen) -> Bool {
        if s.offers.contains(where: { $0.sameAct(as: act) }) { return true }
        guard let id = id else { return false }
        guard let c = s.checks.first(where: { $0.id == id }), c.state != "unknown" else { return true }
        return c.state == state
    }
}

/// When the window asks, and whether its buttons may be pressed: not from
/// the end of an action (or the window's opening) until a full answer asked
/// after it is drawn.
struct SetupAsks {
    var clock = ProbeClock()
    private(set) var turn = 0 // counts those moments
    private(set) var settling = false

    /// What was drawn may be old.
    mutating func changed() {
        turn += 1
        settling = true
        clock.needed = true
    }

    /// An answer came to an ask made in turn asked; whether it is current.
    mutating func answered(_ key: String?, probed: Bool, asked: Int) -> Bool {
        guard asked == turn else {
            if probed { clock.missed() }
            return false
        }
        clock.answered(key, probed: probed)
        // A step that may have changed is not pressed before the probe that follows.
        settling = probed ? false : settling || clock.needed
        return true
    }

    /// There is no potluck to ask: what the window offers is the app's own,
    /// and the first answer after it is drawn whole.
    mutating func nothingToAsk() {
        settling = false
        clock.needed = true
    }

    /// An ask got no answer.
    mutating func missed(probed: Bool) {
        if probed { clock.missed() }
    }
}

/// `host status --probe` as a Potluck older than --probe is asked again: it
/// refused the flag (exit 2, usage).
func withoutProbe(_ args: [String], after code: Int32) -> [String]? {
    code == 2 && args.contains("--probe") ? args.filter { $0 != "--probe" } : nil
}
