// Finding and running the potluck program: the menu's and windows' asks,
// with a time limit, and an action a person pressed (runAction), with none,
// which follows potluck's admin step rule (Admin.swift).

import Foundation

/// The program to run: $POTLUCK_BIN, else potluck in the first of the
/// usual places, else the program of the machine's Potluck daemon (its
/// record, recordedProgram), which must have potluck's `host status` and
/// `host set` commands; when it has not, running it fails and the menu says so.
/// With none of these, the one this app carries (Bootstrap.swift), if any.
func potluckPath() -> String? { installedPotluck() ?? Bundled.app.potluck }

/// Whether this Mac has no Potluck of any kind, so the release the app
/// carries may be used: no program found, and no record of one that is there.
func noPotluckHere() -> Bool { installedPotluck() == nil && daemonRecords().isEmpty }

/// potluckPath without the one this app carries.
func installedPotluck() -> String? {
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

/// The machine's daemon records ($POTLUCK_DAEMONS_DIR, else
/// ~/.local/state/potluck/daemons) that are this user's alone and whose
/// program is there. The daemon itself may have stopped.
func daemonRecords() -> [DaemonRecord] {
    let env = ProcessInfo.processInfo.environment
    let fm = FileManager.default
    var dir = fm.homeDirectoryForCurrentUser.path + "/.local/state/potluck/daemons"
    if let d = env["POTLUCK_DAEMONS_DIR"], !d.isEmpty { dir = d }
    guard isPrivate(dir), let names = try? fm.contentsOfDirectory(atPath: dir) else { return [] }
    return names.filter { $0.hasSuffix(".json") && !$0.hasPrefix(".") }.compactMap { name in
        let path = dir + "/" + name
        guard isPrivate(path), let data = fm.contents(atPath: path),
              let r = try? JSONDecoder().decode(DaemonRecord.self, from: data),
              name == r.program + ".json", fm.isExecutableFile(atPath: r.exe)
        else { return nil }
        return r
    }
}

/// The program records name: the one other than potluck whose daemon's
/// process is this user's and alive; else potluck's own (installed in a
/// place of its own), whether or not its daemon runs. Unlike the potluck
/// command, this does not ask the daemon's socket.
func recordedProgram(_ records: [DaemonRecord] = daemonRecords()) -> String? {
    let live = records.filter { $0.program != "potluck" && $0.pid > 0 && kill($0.pid, 0) == 0 }
    return live.count == 1 ? live[0].exe : records.first { $0.program == "potluck" }?.exe
}

/// Why there is no program to run, from the records when they name one.
func noProgramWords(_ records: [DaemonRecord] = daemonRecords()) -> String {
    let names = records.map(\.program).sorted()
    switch names.count {
    case 0: return "Potluck isn't on this Mac, or this app can't find it. Install Potluck, or ask the person who invited you; this follows by itself."
    case 1: return "Potluck on this Mac is part of \(names[0]), which isn't running. Start \(names[0]); this menu follows by itself."
    default: return "Potluck on this Mac is part of \(names.joined(separator: " and ")). Run just one of them; this menu follows by itself."
    }
}

/// Why nothing can be run, with no program found, from the records and the
/// release the app carries.
func noRunWords(_ records: [DaemonRecord], _ app: Bundled) -> String {
    if records.isEmpty, app.moved { return movedWords }
    if records.isEmpty, app.refused != nil { return bundledRefusedWords }
    return noProgramWords(records)
}

/// What the app tells every potluck it runs, beside its own environment:
/// its login item (POTLUCK_LOGIN_ITEM). Set once, before any run.
var appEnvironment: () -> [String: String] = { [:] }

/// Runs potluck with args; its exit code, stdout and stderr. Both pipes are
/// read at once (a full stderr cannot block it). It is ended after limit
/// seconds (the menu's and windows' asks); nil is no limit, for an action a
/// person pressed (runAction), which may download or wait on a sign-in.
/// program is the one to run, when the caller keeps to one over several runs.
/// stdin is written to it whole, then ended; nil is an empty stdin. An invite
/// goes there, never in args; nothing of the run is logged.
func runPotluck(_ args: [String], stdin: String? = nil, limit: TimeInterval? = 25, program: String? = potluckPath()) -> (Int32, Data, String) {
    guard let path = program else { return (notRun, Data(), noRunWords(daemonRecords(), Bundled.app)) }
    if let why = Bundled.app.refusal(args, with: path) { return (notRun, Data(), why) }
    let p = Process()
    p.executableURL = URL(fileURLWithPath: path)
    p.arguments = args
    p.environment = ProcessInfo.processInfo.environment.merging(appEnvironment()) { $1 }.merging(Bundled.app.environment(for: path)) { $1 }
    let out = Pipe(), err = Pipe()
    p.standardOutput = out
    p.standardError = err
    let input = stdin.map { _ in Pipe() }
    if let input = input { p.standardInput = input } else { p.standardInput = FileHandle.nullDevice }
    do { try p.run() } catch { return (126, Data(), "\(path): \(error.localizedDescription)") }
    let timer = DispatchWorkItem { if p.isRunning { p.terminate() } }
    if let limit = limit { DispatchQueue.global().asyncAfter(deadline: .now() + limit, execute: timer) }
    if let input = input, let text = stdin {
        // A run that ends without reading it must not end this app (SIGPIPE).
        let to = input.fileHandleForWriting
        _ = fcntl(to.fileDescriptor, F_SETNOSIGPIPE, 1)
        DispatchQueue.global().async {
            try? to.write(contentsOf: Data(text.utf8))
            try? to.close()
        }
    }
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
        let code: String?
        let message: String
        let hint: String?
        let plain: String?
    }
    let error: Body
}

/// What went wrong: words for the person, and detail, the program's or the
/// system's own, which a window only copies ("": the words are all of it).
struct Trouble: Error, Equatable {
    let words: String
    var detail = ""

    var raw: String { detail.isEmpty ? words : detail }
}

/// What runPotluck gives for an exit code when it ran nothing and says why
/// itself: one no program exits with.
let notRun: Int32 = -1

/// The words for an error that has none of its own for a person.
let unexplained = "Try again. If this stays, press Copy details and send what it copies to the person who invited you."

/// A failed command's error: from the JSON error on the last line of stderr
/// (progress may come before it), its plain form, else the message and hint
/// of one about what was asked (an older potluck's has no plain form). All
/// of it is detail: stderr, else the exit code.
func commandTrouble(_ err: String, _ code: Int32) -> Trouble {
    if code == notRun, !err.isEmpty { return Trouble(words: err) }
    let last = err.split(whereSeparator: { $0.isNewline }).last.map(String.init) ?? ""
    if let e = (try? JSONDecoder().decode(CommandError.self, from: Data(last.utf8)))?.error {
        let said = [e.message, e.hint ?? ""].filter { !$0.isEmpty }.joined(separator: "\n")
        let before = err.dropLast(last.count).trimmingCharacters(in: .whitespacesAndNewlines)
        let raw = before.isEmpty ? said : before + "\n" + said
        if let plain = e.plain, !plain.isEmpty { return Trouble(words: plain, detail: raw) }
        if e.code != "internal", e.code != "unavailable" { return Trouble(words: said, detail: raw) }
        return Trouble(words: unexplained, detail: raw)
    }
    return Trouble(words: unexplained, detail: err.isEmpty ? "potluck exited \(code)" : err)
}

/// How far an action got, from its answers: what they say was done (nil:
/// the action does not say), what is left, what it does not remove, and what
/// a failed one offers to run instead.
struct Progress {
    var done: [String]?
    var left: [String] = []
    var notRemoved: [String] = []
    var force: OfferedAction?
    var view: UninstallView? // uninstall's, with what every run did

    /// Takes in a run's answer (nil: it gave none, which says nothing new).
    mutating func take(_ a: ActionAnswer?) {
        guard let a = a else { return }
        if let d = a.done { done = (done ?? []) + d.filter { !(done ?? []).contains($0) } }
        left = a.left ?? []
        notRemoved = a.not_removed ?? []
        force = a.force
        view = a.view.map { $0.after(view) } ?? view
    }

    /// The admin step ran: it is the first of what was left.
    mutating func adminRan() {
        guard !left.isEmpty else { return }
        done = (done ?? []) + [left.removeFirst()]
        view?.adminRan()
    }

    /// What was done, for a person ("" when nothing is known to be).
    var words: String {
        guard let d = done, !d.isEmpty else { return "" }
        return "Already done:\n" + d.map { "• " + $0 }.joined(separator: "\n")
    }
}

/// What a cancelled password step leaves. "Nothing was changed" only when
/// the action itself says it did nothing. The step's reason is not said
/// again: the person has just read it in the question they said no to.
func cancelText(_ p: Progress) -> String {
    let head = p.done?.isEmpty == true ? "Nothing was changed." : "You cancelled the password step, so this isn't finished."
    return [head, p.words, "Choose Try again when you're ready to enter your password."].filter { !$0.isEmpty }.joined(separator: "\n")
}

/// What a failed action leaves: why, and how far it got. The step that
/// failed is the first left, and may be partly done.
func failText(_ label: String, _ why: String, _ p: Progress) -> String {
    let partly = p.left.first.map { "This step may be partly done: " + $0 } ?? ""
    return ["“\(label)” didn't work. \(why)", p.words, partly].filter { !$0.isEmpty }.joined(separator: "\n")
}

/// How an action a person pressed ended.
enum ActionResult {
    case done(Progress)
    case failed(Trouble, Progress)
    /// The person said no to the admin step, at the app's question or at the
    /// system's password dialog. The
    /// step did not run; the action's own part before it did.
    case cancelled(Progress)
}

/// The one rule for an action (docs/menubar.md "Set up and check"): run
/// args with no time limit; if its --json answer holds admin_step, ask
/// (on the main thread: ask shows why and calls back true for Continue),
/// run the step's argv as root (admin, on the main thread, so the app
/// waits while the system's password dialog is up and the short root step
/// runs), then run the same once more (args and stdin), with the same program. done is
/// called on the main thread. Off the main thread itself.
func runAction(_ args: [String], stdin: String? = nil, admin: @escaping ([String]) -> AdminResult = runAdmin,
               ask: @escaping (AdminStep, @escaping (Bool) -> Void) -> Void, done: @escaping (ActionResult) -> Void) {
    let finish = { (r: ActionResult) in DispatchQueue.main.async { done(r) } }
    let program = potluckPath()
    DispatchQueue.global(qos: .userInitiated).async {
        var p = Progress()
        let (code, out, err) = runPotluck(args, stdin: stdin, limit: nil, program: program)
        p.take(actionAnswer(in: out))
        if code != 0 { return finish(.failed(commandTrouble(err, code), p)) }
        guard let step = adminStep(in: out) else { return finish(.done(p)) }
        let before = p
        DispatchQueue.main.async {
            ask(step) { go in
                guard go else { return done(.cancelled(before)) }
                switch admin(step.argv) {
                case .cancelled: return done(.cancelled(before))
                case .failed(let why): return done(.failed(Trouble(words: "The step that needs this Mac's password didn't finish. " + unexplained, detail: "the admin step: " + why), before))
                case .ran: break
                }
                DispatchQueue.global(qos: .userInitiated).async {
                    var p = before
                    p.adminRan()
                    let (code, out, err) = runPotluck(args, stdin: stdin, limit: nil, program: program)
                    let again = adminStep(in: out)
                    if again != nil { p = before } // it did not take
                    p.take(actionAnswer(in: out))
                    if code != 0 { return finish(.failed(commandTrouble(err, code), p)) }
                    if let again = again {
                        return finish(.failed(Trouble(words: "The password step ran, but Potluck still asks for it. " + unexplained, detail: "the admin step ran, but potluck still asks for it: " + again.why), p))
                    }
                    finish(.done(p))
                }
            }
        }
    }
}
