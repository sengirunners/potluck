// What `potluck --json host status` answers (menubar.Status), the fields
// the app reads. Plain Foundation, so macos/PotluckMenu/test.sh can decode
// the Go test fixtures (menubar/testdata/status-*.json) with these very
// structs. The words and rules are the command's; this only names the fields.

import Foundation

struct Item: Decodable {
    let id: String
    let label: String
    let sub: String?
    let checked: Bool
    let args: [String]?
    let confirm: String?
}

/// One of the lender's dials: a submenu of choices (`potluck host lend`).
struct Dial: Decodable {
    let id: String
    let label: String
    let choices: [Item]
}

struct Menu: Decodable {
    let title: String
    let detail: String
    let note: String?
    let attention: Bool? // absent when false, and from a potluck older than it
    let cores: String
    let memory: String?
    let items: [Item]
    let dials: [Dial]?
    let windows: [Item]?
}

/// The acts (`SetupDo.app`) this app can do: its login item's
/// (menubar.ActLoginOn and the rest).
let appActs = ["login_item", "login_item_off", "login_item_settings"]

/// A check's action for the app (menubar.Do): exactly one of `args`
/// (potluck's arguments, run as the user; with `invite_args`, those instead when
/// an invite was pasted, the invite on stdin), `url` (open it) and `app` (an act only the app
/// can do). `auto`: it may be done without a click.
struct SetupDo: Decodable, Equatable {
    let label: String
    let args: [String]?
    let invite_args: [String]?
    let invite: Bool? // all an older potluck says, which takes it as an argument
    let url: String?
    let app: String?
    let auto: Bool?

    /// A potluck too old to take the invite on stdin. This app never puts
    /// one in argv, so it asks for a newer potluck instead.
    var argvInvite: Bool { invite == true && invite_args == nil }

    /// What this app can do with it: run args, open url, or its own act.
    var known: Bool { args != nil || url != nil || app.map(appActs.contains) == true }

    /// Does the same thing, whatever the label.
    func sameAct(as d: SetupDo) -> Bool { args == d.args && url == d.url && app == d.app }

    /// What a press runs (nil: nothing to run), with the invite field's text.
    func run(pasted: String) -> PotluckRun? {
        guard let args = args else { return nil }
        let invite = pasted.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let with = invite_args, !invite.isEmpty else { return PotluckRun(args: args, stdin: nil) }
        return PotluckRun(args: with, stdin: invite + "\n")
    }
}

/// One run of potluck: its arguments, and what is written to its stdin (nil:
/// nothing). An invite is only ever in stdin.
struct PotluckRun {
    let args: [String]
    let stdin: String?
}

/// One line of the setup checklist (menubar.Check). `state` is done, todo,
/// blocked, running, unknown or info; `action` is the terminal command the
/// checklist names, as words; `do` is the same action for the app, the only
/// one it may run.
struct SetupCheck: Decodable {
    let id: String
    let state: String
    let detail: String
    let action: String?
    let `do`: SetupDo?
}

/// One stage of the last host run (hostsetup.Stage). `result` is running,
/// succeeded or blocked.
struct SetupStage: Decodable {
    let name: String
    let result: String
    let detail: String?
    let at: String
}

/// A stage a host run goes through (menubar.StageName), with how a person
/// reads it.
struct StageName: Decodable {
    let name: String
    let label: String
}

/// One line of the setup window (menubar.ViewRow): `state` is a check's, or
/// "" for a stage not reached; `do` is the line's own button.
struct SetupRow: Decodable, Equatable {
    let state: String
    let text: String
    let `do`: SetupDo?
}

/// Lines under a title (menubar.ViewSection).
struct SetupSection: Decodable, Equatable {
    let title: String
    let rows: [SetupRow]?
}

/// The setup window's words (menubar.SetupView). `details` is only copied,
/// never shown; `key` is the same with and without --probe and changes when
/// the window's steps may have.
struct SetupView: Decodable {
    let heading: String
    let lead: String?
    let `do`: SetupDo?
    let invite_refused: Bool?
    let invite_later: Bool? // a step still to come takes an invite (nil: a Potluck from before it)
    let sections: [SetupSection]
    let progress: SetupSection
    let details: String?
    let key: String
}

/// When the setup window asks with --probe (which runs Tart and Softnet):
/// when needed (it opened, or an action ended) and about every 30 s. The
/// asks between leave --probe out; one whose key is not the last probe's
/// makes the next ask a probe.
struct ProbeClock {
    static let every: TimeInterval = 30
    var needed = true
    var asked = Date.distantPast
    var key: String?

    /// Whether the ask at now is a probe, which it then counts as.
    mutating func probes(at now: Date) -> Bool {
        guard needed || now.timeIntervalSince(asked) >= ProbeClock.every else { return false }
        needed = false
        asked = now
        return true
    }

    /// A probe got no answer, or one from before an action ended: the next
    /// ask is a probe too.
    mutating func missed() { needed = true }

    /// An answer came, with its window's key.
    mutating func answered(_ k: String?, probed: Bool) {
        if probed { key = k } else if k != key { needed = true }
    }
}

/// The setup checklist (menubar.Setup). `state` is the last host run's:
/// none, running, succeeded, blocked, interrupted or unreadable.
/// `started` is which run that is (when it began), absent with none.
/// `stage_order` is every stage in order, reached or not (absent from a
/// potluck older than it); `next_id` is the check `next` is the action of,
/// or, with no `next`, a blocked check nothing Potluck runs fixes.
struct Setup: Decodable {
    let state: String
    let started: String?
    let stages: [SetupStage]?
    let stage_order: [StageName]?
    let checks: [SetupCheck]
    let next: String?
    let next_id: String?
    let old_apps: [String]? // where the app ran under its old name
    let view: SetupView? // absent from a potluck older than it
}

struct Status: Decodable {
    let menu: Menu
    let setup: Setup?
}

/// Whether the menu bar's picture is marked: the last ask failed (problem), or
/// the menu it got says so.
func needsAttention(_ st: Status?, problem: String?) -> Bool {
    problem != nil || st?.menu.attention == true
}
