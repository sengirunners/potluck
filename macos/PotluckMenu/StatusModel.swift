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
    let cores: String
    let memory: String?
    let items: [Item]
    let dials: [Dial]?
    let windows: [Item]?
}

/// A check's action for the app (menubar.Do): exactly one of `args`
/// (potluck's arguments, run as the user; with `invite`, the pasted invite is
/// appended as the last one), `url` (open it) and `app` (an act only the app
/// can do). `auto`: it may be done without a click.
struct SetupDo: Decodable, Equatable {
    let label: String
    let args: [String]?
    let invite: Bool?
    let url: String?
    let app: String?
    let auto: Bool?
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

/// The setup checklist (menubar.Setup). `state` is the last host run's:
/// none, running, succeeded, blocked, interrupted or unreadable.
/// `stage_order` is every stage in order, reached or not (absent from a
/// potluck older than it); `next_id` is the check `next` is the action of,
/// or, with no `next`, a blocked check nothing Potluck runs fixes.
struct Setup: Decodable {
    let state: String
    let stages: [SetupStage]?
    let stage_order: [StageName]?
    let checks: [SetupCheck]
    let next: String?
    let next_id: String?

    /// The stages the window draws: stage_order, or for an older potluck
    /// the stages its run reached.
    var stageList: [StageName] {
        stage_order ?? (stages ?? []).map { StageName(name: $0.name, label: $0.name) }
    }
}

/// A host run in the background (menubar.Background): state is running, or
/// ended with the last line it wrote.
struct Background: Decodable {
    let state: String
    let last: String?
}

struct Status: Decodable {
    let menu: Menu
    let setup: Setup?
    let background: Background?
}
