// Decodes menubar/testdata/status-*.json (Go's Build output) with the app's own
// structs (StatusModel.swift) and checks what the setup window reads from them;
// decodes action answers (test/answers/*.json, the shapes of cli/adminstep.go and
// uninstall --json) for the admin step rule (Admin.swift, Run.swift), which it runs
// against a fake potluck; and checks that an
// admin step's argv survives the shell and AppleScript quoting word for word.
// Run by macos/PotluckMenu/test.sh: argument 1 is the testdata directory,
// argument 2 the answers directory.

import Foundation

var failed = 0
func expect(_ ok: Bool, _ what: String) {
    if !ok { failed += 1; print("FAIL: \(what)") }
}

let dir = CommandLine.arguments[1]
let names = ((try? FileManager.default.contentsOfDirectory(atPath: dir)) ?? []).filter { $0.hasPrefix("status-") && $0.hasSuffix(".json") }.sorted()
expect(names.count >= 5, "found \(names.count) fixtures in \(dir)")

var all: [String: Status] = [:]
for n in names {
    guard let data = FileManager.default.contents(atPath: dir + "/" + n) else { expect(false, "read \(n)"); continue }
    do {
        let st = try JSONDecoder().decode(Status.self, from: data)
        all[n] = st
        expect(st.setup != nil, "\(n): has a setup object")
        let setup = st.setup!
        expect(!setup.checks.isEmpty, "\(n): has checks")
        expect(setup.stage_order?.map { $0.name } == ["preflight", "images", "validation", "join", "capacity"], "\(n): stage_order \(setup.stage_order?.map { $0.name } ?? [])")
        for s in setup.stages ?? [] {
            expect(setup.stageList.contains { $0.name == s.name }, "\(n): stage \(s.name) is one the window draws")
        }
        for c in setup.checks {
            expect((c.action == nil) == (c.do == nil), "\(n): \(c.id): an action has a do, and only then")
        }
        expect(setup.next == nil || setup.next_id != nil, "\(n): next comes with next_id")
        if let id = setup.next_id {
            let c = setup.checks.first { $0.id == id }
            expect(c?.action == setup.next, "\(n): next_id \(id) is the check next is from")
            expect(setup.next != nil || (c?.state == "blocked" && c?.do == nil), "\(n): next_id \(id) with no next is blocked with nothing to run")
        }
        let win = st.menu.windows?.first { $0.id == "setup" }
        expect(win != nil && win!.args == ["--json", "host", "status", "--probe"], "\(n): the menu lists the setup window with its args")
    } catch {
        expect(false, "\(n): decode: \(error)")
    }
}

// The window's words and steps.
func view(_ n: String) -> SetupView? { all[n]?.setup?.view }
for n in names {
    expect(view(n)?.heading.isEmpty == false && view(n)?.sections.first?.rows?.isEmpty == false, "\(n): the window has a heading and lines")
}
expect(view("status-probe-ready.json")?.do?.invite == true, "ready: the button takes the invite")
expect(view("status-probe-no-helper.json")?.do == nil && view("status-probe-no-helper.json")?.details?.isEmpty == false, "no network helper: nothing to press, details to copy")
expect(view("status-join-sign-in.json")?.do?.url == "https://login.example/a/1", "sign in: the button opens the page")
expect(view("status-lending.json")?.do == nil, "lending: nothing to press")
expect(view("status-setup-running.json")?.progress.rows?.map { $0.state } == ["done", "running", "", "", ""], "running: the stages' lines")

// The probe's clock: on open, after an action, about every 30 s, and on
// another key; the 3-second asks between are plain.
var clock = ProbeClock()
let t0 = Date(timeIntervalSince1970: 1_000_000)
var probes = 0
for i in 0..<100 { // 5 minutes
    if clock.probes(at: t0.addingTimeInterval(Double(i) * 3)) { probes += 1; clock.answered("a", probed: true) } else { clock.answered("a", probed: false) }
}
expect(probes == 10, "\(probes) probes in 5 minutes")
clock.needed = true
expect(clock.probes(at: t0.addingTimeInterval(298)) && !clock.probes(at: t0.addingTimeInterval(299)), "an action's end probes once")
clock.answered("a", probed: true)
clock.answered("b", probed: false)
expect(clock.probes(at: t0.addingTimeInterval(300)), "another key probes")
clock.answered("b", probed: true)
expect(!clock.probes(at: t0.addingTimeInterval(303)), "and only once")
clock.needed = true
expect(clock.probes(at: t0.addingTimeInterval(306)), "needed: a probe")
clock.missed()
expect(clock.probes(at: t0.addingTimeInterval(309)), "a probe that failed is asked again at the next ask")
clock.answered("b", probed: true)
expect(!clock.probes(at: t0.addingTimeInterval(312)), "and only until one answers")

func stages(_ n: String) -> [String] { (all[n]?.setup?.stages ?? []).map { $0.name + ":" + $0.result } }

if let s = all["status-never-set-up.json"]?.setup {
    expect(s.state == "none" && (s.stages ?? []).isEmpty, "never set up: state none, no stages")
    expect(s.next != nil, "never set up: a next step")
    let d = s.checks.first { $0.id == "setup" }?.do
    expect(d?.args == ["--json", "host", "--yes", "--no-sudo", "--detach", "--"] && d?.invite == true, "never set up: the setup button runs host in the background with the invite")
}
if let s = all["status-setup-running.json"] {
    expect(s.setup?.state == "running", "running: state")
    expect(stages("status-setup-running.json") == ["preflight:succeeded", "images:running"], "running: stages \(stages("status-setup-running.json"))")
    expect(s.background?.state == "running" && s.background?.last == "images: image 1 of 2: pull a", "running: background last")
}
if let s = all["status-blocked-validation.json"] {
    expect(s.setup?.state == "blocked", "blocked: state")
    expect(s.setup?.stages?.last?.detail == "conformance failed at boot", "blocked: the stage's detail")
    expect(s.setup?.checks.first { $0.id == "setup" }?.state == "blocked", "blocked: the setup check is blocked")
}
if let s = all["status-join-sign-in.json"]?.setup {
    expect(s.next_id == "setup" && s.checks.first { $0.id == "setup" }?.do?.url == "https://login.example/a/1", "join: the sign-in page is the next button")
}
if let s = all["status-probe-fresh-mac.json"]?.setup {
    expect(Array(s.checks.map { $0.id }.prefix(5)) == ["mac", "potluck", "tart", "softnet", "disk"], "fresh Mac: the probe's checks come first: \(s.checks.map { $0.id })")
    let p = s.checks.first { $0.id == "potluck" }?.do
    expect(s.next_id == "potluck" && p?.args?.prefix(2) == ["upgrade", "--release"], "fresh Mac: installing Potluck is the next button")
    expect(s.checks.first { $0.id == "tart" }?.do?.args == ["--json", "tart", "install"], "fresh Mac: Tart's button installs it")
    expect(s.checks.first { $0.id == "softnet" }?.state == "blocked", "fresh Mac: macOS 15 has no Softnet to install")
}
if let s = all["status-lending.json"] {
    expect(s.setup?.state == "succeeded" && (s.setup?.stages ?? []).count == s.setup?.stageList.count, "lending: every stage the window draws (\(s.setup?.stageList.count ?? 0))")
    expect(s.setup?.checks.first { $0.id == "lending" }?.state == "done", "lending: the lending check is done")
    expect(s.background == nil, "lending: no background run")
}

if let d = all["status-lending.json"]?.setup?.checks.first(where: { $0.id == "updates" })?.do {
    expect(d.args == ["--json", "upgrade", "enable"] && d.invite == nil && d.url == nil, "lending: the updates button runs upgrade enable \(d.args ?? [])")
}

// Action answers: the admin step rule reads admin_step and nothing else.
let answers = CommandLine.arguments[2]
func answer(_ n: String) -> Data { FileManager.default.contents(atPath: answers + "/" + n) ?? Data() }
let install = adminStep(in: answer("softnet-install-admin-step.json"))
expect(install?.argv.first == "/Users/bailey/.local/bin/potluck" && install?.argv.count == 5, "softnet install: admin_step argv \(install?.argv ?? [])")
expect(install?.why.hasPrefix("The admin step installs Softnet") == true, "softnet install: admin_step why")
expect(adminStep(in: answer("softnet-install-done.json")) == nil, "softnet install done: no admin step")
expect(adminStep(in: answer("uninstall-admin-step.json"))?.argv == ["/Users/bailey/.local/bin/potluck", "softnet", "uninstall", "--admin-step"], "uninstall: admin_step argv")
expect(adminStep(in: answer("uninstall-done.json")) == nil, "uninstall done: no admin step")
expect(adminStep(in: Data("not json".utf8)) == nil && adminStep(in: Data()) == nil && adminStep(in: Data("[1]".utf8)) == nil, "an answer that is not an object holds no admin step")

// uninstall's answers: the plan, the admin step, a refusal and the end.
if let plan = actionAnswer(in: answer("uninstall-plan.json")) {
    expect(plan.done == [] && plan.left?.count == 6 && plan.pools_run == ["friends"], "plan: left and pools_run")
    expect(plan.warning?.contains("friends") == true && plan.remove?.detail == plan.warning, "plan: the warning names the pools")
    expect(plan.remove?.args == ["--json", "uninstall", "--yes", "--no-sudo"] && plan.force == nil, "plan: remove's args")
    expect(plan.not_removed?.first?.hasPrefix("Tart") == true, "plan: what stays")
} else { expect(false, "decode uninstall-plan.json") }
if let r = actionAnswer(in: answer("uninstall-refused.json")) {
    expect(r.error?.contains("1 sandbox is still running") == true && r.force?.args.last == "--force" && r.force?.confirm.isEmpty == false, "refused: the error and what force offers")
} else { expect(false, "decode uninstall-refused.json") }
expect(actionAnswer(in: answer("uninstall-done.json"))?.left == [], "done: nothing left")

// What a cancelled or failed action says. "Nothing was changed" only when the
// action says it did nothing.
var told = Progress()
expect(!cancelText("Why.", told).contains("Nothing was changed"), "cancel: an action that does not say what it did")
told.take(actionAnswer(in: Data("{\"done\":[],\"left\":[\"a\"]}".utf8)))
expect(cancelText("Why.", told) == "Nothing was changed. Why.", "cancel: nothing done")
told.take(actionAnswer(in: answer("uninstall-admin-step.json")))
let cancelledWords = cancelText("Why.", told)
expect(!cancelledWords.contains("Nothing was changed") && cancelledWords.contains("• stop sharing mac-c4m8"), "cancel: \(cancelledWords)")
expect(failText("Uninstall", "no.", told).contains("may be partly done"), "fail: a step may be partly done")

// The action rule, against a fake potluck: each run appends its arguments to
// a log and answers the next file. No password dialog: admin is a stand-in.
let tmp = NSTemporaryDirectory() + "potluck-menu-test-\(getpid())"
try! FileManager.default.createDirectory(atPath: tmp, withIntermediateDirectories: true)
defer { try? FileManager.default.removeItem(atPath: tmp) }
func fake(_ runs: [(String, Int)], _ body: (ActionResult, [String]) -> Void, admin: AdminResult = .ran(""), go: Bool = true) {
    let bin = tmp + "/potluck", log = tmp + "/log"
    try? FileManager.default.removeItem(atPath: log)
    var script = "#!/bin/sh\necho \"$*\" >> \(shellQuote(log))\ncase $(wc -l < \(shellQuote(log)) | tr -d ' ') in\n"
    for (i, r) in runs.enumerated() { script += "\(i + 1)) cat \(shellQuote(answers + "/" + r.0)); exit \(r.1);;\n" }
    script += "esac\necho unexpected run >&2; exit 9\n"
    try! script.write(toFile: bin, atomically: true, encoding: .utf8)
    try! FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: bin)
    setenv("POTLUCK_BIN", bin, 1)
    var got: ActionResult?
    runAction(["--json", "uninstall", "--yes", "--no-sudo"], admin: { argv in
        // Another program in its place now: the re-run keeps to the first.
        setenv("POTLUCK_BIN", "/nonexistent/potluck", 1)
        expect(argv.last == "--admin-step", "the admin step's argv \(argv)")
        return admin
    }, ask: { _, answer in answer(go) }, done: { got = $0 })
    let end = Date().addingTimeInterval(20)
    while got == nil && Date() < end { RunLoop.main.run(until: Date().addingTimeInterval(0.02)) }
    let ran = ((try? String(contentsOfFile: log, encoding: .utf8)) ?? "").split(separator: "\n").map(String.init)
    if let r = got { body(r, ran) } else { expect(false, "the action did not end") }
}
let once = "--json uninstall --yes --no-sudo"
fake([("uninstall-admin-step.json", 0), ("uninstall-done.json", 0)], { r, ran in
    guard case .done(let p) = r else { return expect(false, "admin step then done: \(r)") }
    expect(ran == [once, once], "run, admin step, the same once more: \(ran)")
    expect(p.left.isEmpty && p.done?.contains { $0.hasPrefix("remove the managed Softnet") } == true && p.done?.first?.contains("mac-c4m8") == true, "done is both runs' and the admin step: \(p.done ?? [])")
})
for (name, admin, go) in [("the app's question", AdminResult.ran(""), false), ("the password dialog", .cancelled, true)] {
    fake([("uninstall-admin-step.json", 0)], { r, ran in
        guard case .cancelled(_, let p) = r else { return expect(false, "cancelled at \(name): \(r)") }
        expect(ran == [once] && p.done?.count == 2 && p.left.first?.hasPrefix("remove the managed Softnet") == true, "cancelled at \(name): \(p)")
    }, admin: admin, go: go)
}
fake([("uninstall-admin-step.json", 0)], { r, ran in
    guard case .failed(let why, let p) = r else { return expect(false, "admin step failed: \(r)") }
    expect(why == "no" && ran == [once] && p.done?.count == 2, "admin step failed keeps what was done: \(p)")
}, admin: .failed("no"))
fake([("uninstall-admin-step.json", 0), ("uninstall-admin-step.json", 0)], { r, ran in
    guard case .failed(_, let p) = r else { return expect(false, "asked again: \(r)") }
    expect(ran == [once, once] && p.left.first?.hasPrefix("remove the managed Softnet") == true && p.done?.contains { $0.contains("Softnet") } == false, "an admin step asked for twice is a failure, not done: \(p)")
})
fake([("uninstall-refused.json", 1)], { r, ran in
    guard case .failed(_, let p) = r else { return expect(false, "refused: \(r)") }
    expect(ran == [once] && p.done == [] && p.force?.args.last == "--force", "refused: never forced by itself")
})

// The Trash takes only this app's own bundle, and nothing is deleted.
func bundle(_ name: String, _ id: String) -> URL {
    let u = URL(fileURLWithPath: tmp + "/" + name)
    try! FileManager.default.createDirectory(at: u.appendingPathComponent("Contents"), withIntermediateDirectories: true)
    try! PropertyListSerialization.data(fromPropertyList: ["CFBundleIdentifier": id], format: .xml, options: 0).write(to: u.appendingPathComponent("Contents/Info.plist"))
    return u
}
var trashed: [String] = []
let mine = bundle("Potluck Menu.app", "sh.potluck.menu")
expect(trashApp(mine, trash: { trashed.append($0.lastPathComponent) }) == nil && trashed == ["Potluck Menu.app"], "trash: this app")
for u in [bundle("Other.app", "com.example.other"), bundle("plain folder", "sh.potluck.menu"), bundle("AppTranslocation/x/Potluck Menu.app", "sh.potluck.menu"), URL(fileURLWithPath: tmp)] {
    expect(trashApp(u, trash: { trashed.append($0.path) }) != nil && trashed.count == 1, "trash: refuses \(u.path)")
}
struct NoTrash: Error {}
expect(trashApp(mine, trash: { _ in throw NoTrash() }) != nil, "trash: a failed move says so")

// A failed action shows potluck's words, not its JSON.
let failure = String(decoding: answer("error.txt"), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
expect(errorText(failure, 1) == "this is not a whole invite\ncopy the whole invite again: potluck invite <you> --host", "error: \(errorText(failure, 1))")
expect(errorText("potluck: plain words", 1) == "potluck: plain words" && errorText("", 2) == "potluck exited 2", "an error that is not JSON is shown as it is")

// Quoting: each word reaches the program as itself, through sh and through the
// very AppleScript runAdmin uses (here without administrator privileges).
expect(shellQuote("a b") == "'a b'", "quote: spaces \(shellQuote("a b"))")
expect(shellQuote("it's") == "'it'\\''s'", "quote: a single quote \(shellQuote("it's"))")
let hard = ["/usr/bin/printf", "%s\\n", "two words", "it's", "$HOME", "`id`", "$(id)", "back\\slash", "\"quoted\"", "semi;colon && rm -rf /x", "*", "", "line\nbreak", "ünï"]
let sh = Process()
sh.executableURL = URL(fileURLWithPath: "/bin/sh")
sh.arguments = ["-c", shellLine(hard)]
let pipe = Pipe()
sh.standardOutput = pipe
try! sh.run()
let shOut = String(decoding: pipe.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
sh.waitUntilExit()
let want = hard.dropFirst(2).map { $0 + "\n" }.joined()
expect(shOut == want, "sh: argv round trip, got \(shOut.debugDescription)")
switch runScript(hard, privileged: false) {
case .ran(let out): expect(out == want, "AppleScript: argv round trip, got \(out.debugDescription)")
case let other: expect(false, "AppleScript: \(other)")
}
expect(adminScript(["/x"], privileged: true).hasSuffix("with administrator privileges without altering line endings"), "the admin script asks for administrator privileges")
expect(runScript(["printf", "x"], privileged: false) == .failed("potluck's admin step names no absolute program: printf"), "a relative program is refused")

if failed > 0 { print("\(failed) failed"); exit(1) }
print("ok: decoded \(names.count) fixtures and the action answers; quoting round-trips")
