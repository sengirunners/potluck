// A root step a potluck command left for the app (`--no-sudo --json`
// answers {"admin_step": {"why", "argv"}}, cli/adminstep.go), and running
// it through macOS's own administrator password dialog. The app never sees
// the password. Plain Foundation, so macos/PotluckMenu/test.sh compiles it.

import Foundation

/// A root step: potluck's reason for it, in its words, and the program
/// (an absolute path) and arguments to run as root.
struct AdminStep: Decodable {
    let why: String
    let argv: [String]
}

/// Something potluck offers to run (uninstall's `remove` and `force`): its
/// arguments, and the question to ask the person first.
struct OfferedAction: Decodable {
    let confirm: String
    let detail: String?
    let args: [String]
}

/// The parts of a command's --json answer the app reads. Only uninstall
/// (cli/uninstall.go) says what it did (`done`), what is left (`left`, the
/// admin step first when there is one; a dry run's whole plan) and what it
/// does not remove; the fields from pools_run on are its dry run's, and
/// `force` a failed run's.
struct ActionAnswer: Decodable {
    let admin_step: AdminStep?
    let done: [String]?
    let left: [String]?
    let not_removed: [String]?
    let error: String?
    let pools_run: [String]?
    let warning: String?
    let remove: OfferedAction?
    let force: OfferedAction?
}

/// A command's --json answer. One that is not a JSON object is none.
func actionAnswer(in out: Data) -> ActionAnswer? {
    try? JSONDecoder().decode(ActionAnswer.self, from: out)
}

/// The admin step in a command's --json answer, if it holds one.
func adminStep(in out: Data) -> AdminStep? {
    actionAnswer(in: out)?.admin_step
}

/// word quoted for a POSIX shell: in single quotes, each ' as '\''. Syntax
/// only: inside single quotes the shell expands nothing ($, `, \, spaces).
func shellQuote(_ word: String) -> String {
    "'" + word.replacingOccurrences(of: "'", with: "'\\''") + "'"
}

/// The shell command line that runs argv, each word quoted.
func shellLine(_ argv: [String]) -> String {
    argv.map(shellQuote).joined(separator: " ")
}

/// s as an AppleScript string literal: in double quotes, \ and " escaped.
func appleScriptString(_ s: String) -> String {
    "\"" + s.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"") + "\""
}

/// The AppleScript that runs argv through `do shell script`, as root when
/// privileged (the system asks for an administrator's password). Its result
/// is the command's output, line endings unchanged.
func adminScript(_ argv: [String], privileged: Bool) -> String {
    "do shell script " + appleScriptString(shellLine(argv)) + (privileged ? " with administrator privileges" : "") + " without altering line endings"
}

enum AdminResult: Equatable {
    case ran(String)
    /// The person cancelled the password dialog: nothing ran.
    case cancelled
    /// The step could not run, or ran and failed: the system's or the
    /// command's error text.
    case failed(String)
}

/// AppleScript's "User canceled." error number.
let appleScriptUserCanceled = -128

/// Runs argv through `do shell script`, as root when privileged. Call it on
/// the main thread (NSAppleScript is not thread-safe); the password dialog is
/// the system's.
func runScript(_ argv: [String], privileged: Bool) -> AdminResult {
    guard !argv.isEmpty, argv[0].hasPrefix("/") else {
        return .failed("potluck's admin step names no absolute program: \(argv.first ?? "nothing")")
    }
    guard let script = NSAppleScript(source: adminScript(argv, privileged: privileged)) else {
        return .failed("the admin step could not be made into a script")
    }
    var err: NSDictionary?
    let out = script.executeAndReturnError(&err)
    if let err = err {
        if (err[NSAppleScript.errorNumber] as? Int) == appleScriptUserCanceled { return .cancelled }
        return .failed((err[NSAppleScript.errorMessage] as? String) ?? "the admin step failed")
    }
    return .ran(out.stringValue ?? "")
}

/// Runs potluck's admin step as root, behind the system's password dialog.
func runAdmin(_ argv: [String]) -> AdminResult {
    runScript(argv, privileged: true)
}
