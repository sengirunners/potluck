// Finding and running the potluck program: the menu's and windows' asks,
// with a time limit, and an action a person pressed (runAction), with none,
// which follows potluck's admin step rule (Admin.swift).

import Foundation

/// The program to run: $POTLUCK_BIN, else potluck in the first of the
/// usual places, else the program of the machine's Potluck daemon (its
/// record, recordedProgram), which must have potluck's `host status` and
/// `host set` commands; when it has not, running it fails and the menu says so.
func potluckPath() -> String? {
    let env = ProcessInfo.processInfo.environment
    if let p = env["POTLUCK_BIN"], !p.isEmpty { return p }
    let home = FileManager.default.homeDirectoryForCurrentUser.path
    let places = [home + "/.local/bin/potluck", "/opt/homebrew/bin/potluck", "/usr/local/bin/potluck", home + "/go/bin/potluck"]
    if let p = places.first(where: { FileManager.default.isExecutableFile(atPath: $0) }) { return p }
    return recordedProgram()
}

/// A daemon's machine record (kit/daemonrecord), the fields read here.
struct DaemonRecord: Decodable {
    let program: String
    let exe: String
    let pid: Int32
}

/// isPrivate: path is this user's alone (owned by this user, no group or
/// other permission bits, not a symlink), as kit/daemonrecord requires.
func isPrivate(_ path: String) -> Bool {
    var st = stat()
    guard lstat(path, &st) == 0 else { return false }
    let kind = st.st_mode & S_IFMT
    return (kind == S_IFDIR || kind == S_IFREG) && st.st_uid == getuid() && st.st_mode & 0o077 == 0
}

/// The program of the one daemon of another program than potluck whose
/// record is in the machine's records ($POTLUCK_DAEMONS_DIR, else
/// ~/.local/state/potluck/daemons) and whose process is this user's and
/// alive. Unlike the potluck command, this does not ask the daemon's
/// socket. None, or more than one: nil.
func recordedProgram() -> String? {
    let env = ProcessInfo.processInfo.environment
    let fm = FileManager.default
    var dir = fm.homeDirectoryForCurrentUser.path + "/.local/state/potluck/daemons"
    if let d = env["POTLUCK_DAEMONS_DIR"], !d.isEmpty { dir = d }
    guard isPrivate(dir), let names = try? fm.contentsOfDirectory(atPath: dir) else { return nil }
    var found: [String] = []
    for name in names where name.hasSuffix(".json") && !name.hasPrefix(".") {
        let path = dir + "/" + name
        guard isPrivate(path), let data = fm.contents(atPath: path),
              let r = try? JSONDecoder().decode(DaemonRecord.self, from: data),
              name == r.program + ".json", r.program != "potluck", r.pid > 0, kill(r.pid, 0) == 0,
              fm.isExecutableFile(atPath: r.exe)
        else { continue }
        found.append(r.exe)
    }
    return found.count == 1 ? found[0] : nil
}

/// Runs potluck with args; its exit code, stdout and stderr. Both pipes are
/// read at once (a full stderr cannot block it). It is ended after limit
/// seconds (the menu's and windows' asks); nil is no limit, for an action a
/// person pressed (runAction), which may download or wait on a sign-in.
/// program is the one to run, when the caller keeps to one over several runs.
/// Nothing of the run is logged: an action's last argument may be an invite.
func runPotluck(_ args: [String], limit: TimeInterval? = 25, program: String? = potluckPath()) -> (Int32, Data, String) {
    guard let path = program else {
        return (127, Data(), "potluck not found, and no running Potluck daemon names its program: install potluck (~/.local/bin) or set POTLUCK_BIN")
    }
    let p = Process()
    p.executableURL = URL(fileURLWithPath: path)
    p.arguments = args
    let out = Pipe(), err = Pipe()
    p.standardOutput = out
    p.standardError = err
    do { try p.run() } catch { return (126, Data(), "\(path): \(error.localizedDescription)") }
    let timer = DispatchWorkItem { if p.isRunning { p.terminate() } }
    if let limit = limit { DispatchQueue.global().asyncAfter(deadline: .now() + limit, execute: timer) }
    var e = Data()
    let group = DispatchGroup()
    group.enter()
    DispatchQueue.global().async {
        e = err.fileHandleForReading.readDataToEndOfFile()
        group.leave()
    }
    let o = out.fileHandleForReading.readDataToEndOfFile()
    group.wait()
    p.waitUntilExit()
    timer.cancel()
    return (p.terminationStatus, o, String(decoding: e, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines))
}

/// The error potluck writes as the last line of stderr under --json
/// (cli/cli.go), the fields read here.
struct CommandError: Decodable {
    struct Body: Decodable {
        let message: String
        let hint: String?
    }
    let error: Body
}

/// A failed command's error in potluck's words: the message and hint of the
/// JSON error on the last line of stderr (progress may come before it), else
/// all of stderr, else the exit code.
func errorText(_ err: String, _ code: Int32) -> String {
    let last = err.split(whereSeparator: { $0.isNewline }).last.map(String.init) ?? ""
    if let e = try? JSONDecoder().decode(CommandError.self, from: Data(last.utf8)) {
        guard let hint = e.error.hint, !hint.isEmpty else { return e.error.message }
        return e.error.message + "\n" + hint
    }
    return err.isEmpty ? "potluck exited \(code)" : err
}

/// How far an action got, from its answers: what they say was done (nil:
/// the action does not say), what is left, what it does not remove, and what
/// a failed one offers to run instead.
struct Progress {
    var done: [String]?
    var left: [String] = []
    var notRemoved: [String] = []
    var force: OfferedAction?

    /// Takes in a run's answer (nil: it gave none, which says nothing new).
    mutating func take(_ a: ActionAnswer?) {
        guard let a = a else { return }
        if let d = a.done { done = (done ?? []) + d.filter { !(done ?? []).contains($0) } }
        left = a.left ?? []
        notRemoved = a.not_removed ?? []
        force = a.force
    }

    /// The admin step ran: it is the first of what was left.
    mutating func adminRan() {
        guard !left.isEmpty else { return }
        done = (done ?? []) + [left.removeFirst()]
    }

    /// What was done, for a person ("" when nothing is known to be).
    var words: String {
        guard let d = done, !d.isEmpty else { return "" }
        return "Already done:\n" + d.map { "• " + $0 }.joined(separator: "\n")
    }
}

/// What a cancelled admin step leaves. "Nothing was changed" only when the
/// action itself says it did nothing.
func cancelText(_ why: String, _ p: Progress) -> String {
    let head = p.done?.isEmpty == true ? "Nothing was changed." : "The admin step did not run."
    return [head + " " + why, p.words].filter { !$0.isEmpty }.joined(separator: "\n")
}

/// What a failed action leaves: potluck's (or the dialog's) error, and how
/// far it got. The step that failed is the first left, and may be partly done.
func failText(_ label: String, _ why: String, _ p: Progress) -> String {
    let partly = p.left.first.map { "This step may be partly done: " + $0 } ?? ""
    return ["\(label) did not work: \(why)", p.words, partly].filter { !$0.isEmpty }.joined(separator: "\n")
}

/// How an action a person pressed ended.
enum ActionResult {
    case done(Progress)
    /// potluck's (or the password dialog's) error text.
    case failed(String, Progress)
    /// The person said no to the admin step, at the app's question or at the
    /// system's password dialog; why is potluck's reason for the step. The
    /// step did not run; the action's own part before it did.
    case cancelled(why: String, Progress)
}

/// The one rule for an action (docs/menubar.md "Set up and check"): run
/// args with no time limit; if its --json answer holds admin_step, ask
/// (on the main thread: ask shows why and calls back true for Continue),
/// run the step's argv as root (admin, on the main thread, so the app
/// waits while the system's password dialog is up and the short root step
/// runs), then run the same args once more, with the same program. done is
/// called on the main thread. Off the main thread itself.
func runAction(_ args: [String], admin: @escaping ([String]) -> AdminResult = runAdmin,
               ask: @escaping (AdminStep, @escaping (Bool) -> Void) -> Void, done: @escaping (ActionResult) -> Void) {
    let finish = { (r: ActionResult) in DispatchQueue.main.async { done(r) } }
    let program = potluckPath()
    DispatchQueue.global(qos: .userInitiated).async {
        var p = Progress()
        let (code, out, err) = runPotluck(args, limit: nil, program: program)
        p.take(actionAnswer(in: out))
        if code != 0 { return finish(.failed(errorText(err, code), p)) }
        guard let step = adminStep(in: out) else { return finish(.done(p)) }
        let before = p
        DispatchQueue.main.async {
            ask(step) { go in
                guard go else { return done(.cancelled(why: step.why, before)) }
                switch admin(step.argv) {
                case .cancelled: return done(.cancelled(why: step.why, before))
                case .failed(let why): return done(.failed(why, before))
                case .ran: break
                }
                DispatchQueue.global(qos: .userInitiated).async {
                    var p = before
                    p.adminRan()
                    let (code, out, err) = runPotluck(args, limit: nil, program: program)
                    let again = adminStep(in: out)
                    if again != nil { p = before } // it did not take
                    p.take(actionAnswer(in: out))
                    if code != 0 { return finish(.failed(errorText(err, code), p)) }
                    if let again = again {
                        return finish(.failed("the admin step ran, but potluck still asks for it: " + again.why, p))
                    }
                    finish(.done(p))
                }
            }
        }
    }
}
