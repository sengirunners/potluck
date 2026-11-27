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
expect(view("status-probe-ready.json")?.do?.invite_args?.last == "--invite-stdin", "ready: the button takes the invite")
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

// An older potluck takes the invite as an argument; this app never gives it one.
func decodeDo(_ s: String) -> SetupDo { try! JSONDecoder().decode(SetupDo.self, from: Data(s.utf8)) }
let oldDo = decodeDo(#"{"label":"Set up","args":["host","--"],"invite":true}"#)
expect(oldDo.argvInvite && oldDo.run(pasted: "secret")?.args == ["host", "--"], "an older potluck's setup is refused, its args without the invite")
expect(view("status-probe-ready.json")?.do?.argvInvite == false, "ready: not an older potluck's")
expect(decodeDo(#"{"label":"Turn on","args":["a"]}"#).sameAct(as: decodeDo(#"{"label":"Turn back on","args":["a"]}"#)), "another label is the same act")
expect(!decodeDo(#"{"label":"x","args":["a"]}"#).sameAct(as: decodeDo(#"{"label":"x","url":"https://a"}"#)), "another act is not")
expect(!decodeDo(#"{"label":"x","url":"https://b"}"#).sameAct(as: decodeDo(#"{"label":"x","url":"https://a"}"#)), "another page is not")

// A failure stays while its own check is open, whatever is next, and goes
// when the check is done or goes on.
func check(_ id: String, _ state: String, _ d: String? = nil) -> String { #"{"id":"\#(id)","state":"\#(state)","detail":"x""# + (d.map { #","do":\#($0)"# } ?? "") + "}" }
func answerOf(_ checks: [String], next: String? = nil, rows: [String] = []) -> Status {
    let s = #"{"menu":{"title":"t","detail":"","cores":"","items":[]},"setup":{"state":"none","checks":[\#(checks.joined(separator: ","))],"view":{"heading":"h","key":"k","details":"more","progress":{"title":""},"sections":[{"title":"s","rows":[\#(rows.joined(separator: ","))]}]"# + (next.map { #","do":\#($0)"# } ?? "") + "}}}"
    return try! JSONDecoder().decode(Status.self, from: Data(s.utf8))
}
func shown(_ st: Status) -> SetupScreen { setupScreen(st, canUpdate: true, updated: false) }
let start = #"{"label":"Start Potluck","args":["daemon"]}"#, tart = #"{"label":"Install Tart","args":["tart"]}"#, on = #"{"label":"Turn on","args":["enable"]}"#, back = #"{"label":"Turn back on","args":["enable"]}"#
func row(_ d: String) -> String { #"{"state":"info","text":"x","do":\#(d)}"# }
let startFailed = SetupFailure(decodeDo(start), on: shown(answerOf([check("daemon", "todo", start)], next: start)))
expect(startFailed.id == "daemon" && startFailed.stands(on: shown(answerOf([check("daemon", "todo", start)], next: start))), "failure: stands while offered")
expect(!startFailed.stands(on: shown(answerOf([check("daemon", "done")]))), "failure: done by the first full check after it")
expect(startFailed.stands(on: shown(answerOf([check("tart", "todo", tart), check("daemon", "todo", start)], next: tart))), "failure: stands behind another next step, and back again")
expect(!startFailed.stands(on: shown(answerOf([check("tart", "todo", tart), check("daemon", "done")], next: tart))), "failure: done, with another next step")
expect(startFailed.stands(on: shown(answerOf([check("daemon", "todo", tart)], next: tart))), "failure: its check is open with another button")
expect(!startFailed.stands(on: shown(answerOf([check("daemon", "running")]))), "failure: its check goes on")
let onFailed = SetupFailure(decodeDo(on), on: shown(answerOf([check("updates", "todo", on)], rows: [row(on)])))
expect(onFailed.stands(on: shown(answerOf([check("updates", "todo", back)], rows: [row(back)]))), "failure: a line's button under another label")
expect(onFailed.stands(on: shown(answerOf([check("updates", "unknown")]))) && onFailed.stands(on: shown(answerOf([]))), "failure: unread is no verdict")
expect(!onFailed.stands(on: shown(answerOf([check("updates", "done")]))), "failure: a line's, done")
let runFailed = SetupFailure(decodeDo(start), on: shown(answerOf([check("setup", "todo", start)], next: start)))
expect(!runFailed.stands(on: shown(answerOf([check("setup", "blocked", tart)], next: tart))), "failure: its check has changed and says why itself")
let offFailed = SetupFailure(decodeDo(tart), on: shown(answerOf([check("login", "done")], rows: [row(tart)])))
expect(offFailed.id == nil && offFailed.stands(on: shown(answerOf([], rows: [row(tart)]))) && !offFailed.stands(on: shown(answerOf([]))), "failure: an action no check has follows itself")

// When buttons may be pressed.
var asks = SetupAsks()
asks.changed()
let before = asks.turn
expect(asks.settling && asks.clock.probes(at: t0), "asks: off until drawn")
asks.changed()
expect(!asks.answered("a", probed: true, asked: before) && asks.settling && asks.clock.needed, "asks: an answer from before is not current")
expect(asks.answered("a", probed: false, asked: asks.turn) && asks.settling, "asks: a plain answer turns nothing on")
expect(asks.clock.probes(at: t0) && asks.answered("a", probed: true, asked: asks.turn) && !asks.settling, "asks: a full one does")
expect(asks.answered("b", probed: false, asked: asks.turn) && asks.settling, "asks: another key turns them off")
asks.nothingToAsk()
expect(!asks.settling, "asks: the app's own button needs no answer")
expect(asks.clock.probes(at: t0) && asks.answered("a", probed: true, asked: asks.turn) && !asks.clock.needed, "asks: all drawn")
asks.missed(probed: false)
asks.nothingToAsk()
expect(asks.clock.probes(at: t0), "asks: gone and back at once, the next ask draws all of it, whatever was drawn before")

// A run's invite stays in the field until setup went through with it.
func setupState(_ state: String, _ view: String, started: String? = "now") -> Status {
    try! JSONDecoder().decode(Status.self, from: Data(#"{"menu":{"title":"t","detail":"","cores":"","items":[]},"setup":{"state":"\#(state)",\#(started.map { #""started":"\#($0)","# } ?? "")"checks":[],"view":{"key":"k","heading":"h","progress":{"title":""},"sections":[]\#(view)}}}"#.utf8))
}
let wants = #","do":{"label":"Continue setup","args":["host"],"invite_args":["host","--invite-stdin"]}"#
func spent(_ state: String, _ view: String = "", given: String? = "i\n", field: String = " i", over: String? = nil, started: String? = "now") -> Bool { inviteSpent(given: given, over: over, field: field, setupState(state, view, started: started)) }
expect(!spent("running") && !spent("interrupted", wants) && !spent("blocked"), "invite: kept while setup may need it again")
expect(spent("succeeded") && !spent("succeeded", given: nil) && !spent("succeeded", wants), "invite: dropped once setup went through, and only one a run used")
expect(!spent("succeeded", field: "another"), "invite: one pasted since is kept")
let refused = wants + #","invite_refused":true"#
expect(spent("blocked", refused) && !spent("blocked", refused, field: "another") && !spent("blocked", refused, given: nil), "invite: one refused is dropped")
expect(!spent("blocked", refused, over: "now") && !spent("succeeded", over: "now"), "invite: what an earlier run came to is no verdict on it")
expect(spent("blocked", refused, over: "before") && spent("succeeded", over: "before") && spent("succeeded", started: nil), "invite: the run it was given to is, or one an older Potluck tells of")
expect(sameInvite(" a\n", "a") && !sameInvite("a", "b") && !sameInvite(nil, ""), "invite: the one given")
let freshMac = all["status-probe-fresh-mac.json"]!
expect(shown(freshMac).inviteLater && shown(freshMac).act?.label == "Install Potluck", "invite: a link's is kept on a Mac whose invite step is still to come")
expect(!shown(all["status-lending.json"]!).inviteLater, "invite: a Mac that lends takes none")
expect(shown(all["status-daemon-down.json"]!).act != nil && !shown(all["status-daemon-down.json"]!).inviteLater, "invite: Start Potluck takes none, though it is a button")
expect(!shown(setupState("none", "")).inviteLater && shown(setupState("none", wants)).inviteLater, "invite: a Potluck from before invite_later: kept while there is a step")

// A Potluck older than this app: its setup is never run, its other lines and
// details stay, and it can be updated.
let older = answerOf([], next: #"{"label":"Set up","args":["host","--"],"invite":true}"#, rows: [row(on), row(#"{"label":"Lend","args":["host","--"],"invite":true}"#)])
let oldScreen = shown(older)
expect(needsUpdating(older) && oldScreen.act == updatePotluck && oldScreen.offers.count == 2 && !oldScreen.offers.contains { $0.argvInvite }, "older: update, and daily updates: \(oldScreen.offers.map(\.label))")
expect(oldScreen.details.hasSuffix("\nmore") && oldScreen.sections.first?.rows?.count == 2, "older: its lines and details stay")
expect(setupScreen(older, canUpdate: false, updated: false).act == nil, "older: another program is not potluck's to update")
expect(setupScreen(older, canUpdate: true, updated: true).lead != oldScreen.lead, "older: an update that brought nothing newer says so")
let oldest = try! JSONDecoder().decode(Status.self, from: Data(#"{"menu":{"title":"t","detail":"","cores":"","items":[]},"setup":{"state":"none","checks":[\#(check("updates", "todo", on))]}}"#.utf8))
expect(shown(oldest).offers.map(\.label) == ["Update Potluck", "Turn on"], "older, with no view: \(shown(oldest).offers.map(\.label))")
let noSetup = try! JSONDecoder().decode(Status.self, from: Data(#"{"menu":{"title":"t","detail":"","cores":"","items":[]}}"#.utf8))
expect(shown(noSetup).act == updatePotluck, "older, with no setup at all")
expect(!needsUpdating(all["status-probe-ready.json"]!) && shown(all["status-probe-ready.json"]!).act?.label == "Set up this Mac", "this one is not older")
expect(withoutProbe(["host", "--probe"], after: 2) == ["host"] && withoutProbe(["host", "--probe"], after: 1) == nil && withoutProbe(["host"], after: 2) == nil, "older than --probe: asked without it")

func stages(_ n: String) -> [String] { (all[n]?.setup?.stages ?? []).map { $0.name + ":" + $0.result } }

if let s = all["status-never-set-up.json"]?.setup {
    expect(s.state == "none" && (s.stages ?? []).isEmpty, "never set up: state none, no stages")
    expect(s.next != nil, "never set up: a next step")
    let d = s.checks.first { $0.id == "setup" }?.do
    expect(d?.args == ["--json", "host", "--yes", "--no-sudo", "--detach", "--instead-of-invite=", "--"] && d?.invite_args == ["--json", "host", "--yes", "--no-sudo", "--detach", "--invite-stdin"], "never set up: the setup button runs host in the background with the invite")
}
if let s = all["status-setup-running.json"] {
    expect(s.setup?.state == "running", "running: state")
    expect(stages("status-setup-running.json") == ["preflight:succeeded", "images:running"], "running: stages \(stages("status-setup-running.json"))")
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
    expect(s.next_id == "potluck" && p?.args?.prefix(3) == ["--json", "upgrade", "--release"], "fresh Mac: installing Potluck is the next button")
    expect(s.checks.first { $0.id == "tart" }?.do?.args == ["--json", "tart", "install"], "fresh Mac: Tart's button installs it")
    expect(s.checks.first { $0.id == "softnet" }?.state == "blocked", "fresh Mac: macOS 15 has no Softnet to install")
}
for (n, st) in all {
    expect(needsAttention(st, problem: nil) == (["status-blocked-validation.json", "status-daemon-down.json"].contains(n)), "\(n): the menu bar is marked only where the menu says so")
    expect(needsAttention(st, problem: "no answer"), "\(n): and when an ask fails")
}
if let s = all["status-lending.json"] {
    expect(s.setup?.state == "succeeded" && (s.setup?.stages ?? []).count == s.setup?.stage_order?.count, "lending: every stage")
    expect(s.setup?.checks.first { $0.id == "lending" }?.state == "done", "lending: the lending check is done")
}

if let d = all["status-lending.json"]?.setup?.checks.first(where: { $0.id == "updates" })?.do {
    expect(d.args == ["--json", "upgrade", "enable"] && d.invite_args == nil && d.url == nil, "lending: the updates button runs upgrade enable \(d.args ?? [])")
}

// Action answers: the admin step rule reads admin_step and nothing else.
let answers = CommandLine.arguments[2]
func answer(_ n: String) -> Data { FileManager.default.contents(atPath: answers + "/" + n) ?? Data() }
let install = adminStep(in: answer("softnet-install-admin-step.json"))
expect(install?.argv.first == "/Users/bailey/.local/bin/potluck" && install?.argv.count == 5, "softnet install: admin_step argv \(install?.argv ?? [])")
expect(install?.why == "macOS will ask for this Mac's password to install the network helper.", "softnet install: admin_step why")
expect(adminStep(in: answer("softnet-install-done.json")) == nil, "softnet install done: no admin step")
expect(adminStep(in: answer("uninstall-admin-step.json"))?.argv == ["/Users/bailey/.local/bin/potluck", "softnet", "uninstall", "--admin-step"], "uninstall: admin_step argv")
expect(adminStep(in: answer("uninstall-done.json")) == nil, "uninstall done: no admin step")
expect(adminStep(in: Data("not json".utf8)) == nil && adminStep(in: Data()) == nil && adminStep(in: Data("[1]".utf8)) == nil, "an answer that is not an object holds no admin step")

// uninstall's answers: the plan, the admin step, a refusal and the end.
if let plan = actionAnswer(in: answer("uninstall-plan.json")) {
    expect(plan.done == [] && plan.left?.count == 6 && plan.pools_run == ["friends"], "plan: left and pools_run")
    expect(plan.warning?.contains("friends") == true && plan.remove?.detail == plan.view?.warning, "plan: the warning names the pools")
    expect(plan.remove?.args == ["--json", "uninstall", "--yes", "--no-sudo"] && plan.force == nil, "plan: remove's args")
    expect(plan.view?.stays.first?.hasPrefix("Tart") == true && plan.view?.left.count == 6, "plan: what stays, and the window's steps")
    expect(plan.view?.choice?.on == false && plan.view?.choice?.args.last == "--with-tart", "plan: the box")
} else { expect(false, "decode uninstall-plan.json") }
if let r = actionAnswer(in: answer("uninstall-refused.json")) {
    expect(r.error?.contains("1 sandbox is still running") == true && r.force?.args.last == "--force" && r.force?.confirm.isEmpty == false, "refused: the error and what force offers")
} else { expect(false, "decode uninstall-refused.json") }
expect(actionAnswer(in: answer("uninstall-done.json"))?.left == [], "done: nothing left")

// What a cancelled or failed action says. "Nothing was changed" only when the
// action says it did nothing.
var told = Progress()
expect(!cancelText(told).contains("Nothing was changed"), "cancel: an action that does not say what it did")
told.take(actionAnswer(in: Data("{\"done\":[],\"left\":[\"a\"]}".utf8)))
expect(cancelText(told) == "Nothing was changed.\nChoose Try again when you're ready to enter your password.", "cancel: nothing done")
told.take(actionAnswer(in: answer("uninstall-admin-step.json")))
let cancelledWords = cancelText(told)
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
if let w = actionAnswer(in: answer("uninstall-plan-with-tart.json")) {
    expect(w.view?.choice?.on == true && w.remove?.args.last == "--with-tart=all", "plan with the box ticked")
} else { expect(false, "decode uninstall-plan-with-tart.json") }
let once = "--json uninstall --yes --no-sudo"
fake([("uninstall-admin-step.json", 0), ("uninstall-done.json", 0)], { r, ran in
    guard case .done(let p) = r else { return expect(false, "admin step then done: \(r)") }
    expect(ran == [once, once], "run, admin step, the same once more: \(ran)")
    expect(p.left.isEmpty && p.done?.contains { $0.hasPrefix("remove the managed Softnet") } == true && p.done?.first?.contains("mac-c4m8") == true, "done is both runs' and the admin step: \(p.done ?? [])")
    expect(p.view?.done.map(\.did) == ["Stopped lending this Mac", "Left the groups this Mac joined: studio", "Removed the network helper", "Stopped Potluck", "Deleted Potluck's data on this Mac", "Removed the Potluck program"], "the window's done, each once: \(p.view?.done ?? [])")
})
for (name, admin, go) in [("the app's question", AdminResult.ran(""), false), ("the password dialog", .cancelled, true)] {
    fake([("uninstall-admin-step.json", 0)], { r, ran in
        guard case .cancelled(let p) = r else { return expect(false, "cancelled at \(name): \(r)") }
        expect(ran == [once] && p.done?.count == 2 && p.left.first?.hasPrefix("remove the managed Softnet") == true, "cancelled at \(name): \(p)")
    }, admin: admin, go: go)
}
fake([("uninstall-admin-step.json", 0)], { r, ran in
    guard case .failed(let why, let p) = r else { return expect(false, "admin step failed: \(r)") }
    expect(why.detail == "the admin step: no" && !why.words.contains("no,") && ran == [once] && p.done?.count == 2, "admin step failed keeps what was done: \(p)")
}, admin: .failed("no"))
fake([("uninstall-admin-step.json", 0), ("uninstall-admin-step.json", 0)], { r, ran in
    guard case .failed(_, let p) = r else { return expect(false, "asked again: \(r)") }
    expect(ran == [once, once] && p.left.first?.hasPrefix("remove the managed Softnet") == true && p.done?.contains { $0.contains("Softnet") } == false, "an admin step asked for twice is a failure, not done: \(p)")
})
fake([("uninstall-refused.json", 1)], { r, ran in
    guard case .failed(_, let p) = r else { return expect(false, "refused: \(r)") }
    expect(ran == [once] && p.done == [] && p.force?.args.last == "--force", "refused: never forced by itself")
})

// The invite is only ever on stdin: in the run after an admin step too, and
// with the very run Ask again keeps. The fake potluck logs its arguments, its
// environment, what ps shows of it and its stdin.
let secretPart = UUID().uuidString // no other process on this machine shows it
let secret = "potluck-mini:abcdefabcdef:" + secretPart
if let d = all["status-probe-ready.json"]?.setup?.view?.do {
    let bin = tmp + "/ps-potluck"
    let script = "#!/bin/sh\nd=\(shellQuote(tmp))\nps -axww -o command= >> $d/seen; env >> $d/seen; printf '%s\\n' \"$*\" >> $d/args\ncat >> $d/stdin\nif [ -e $d/asked ]; then echo '{}'; else touch $d/asked; cat \(shellQuote(answers + "/softnet-install-admin-step.json")); fi\n"
    try! script.write(toFile: bin, atomically: true, encoding: .utf8)
    try! FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: bin)
    setenv("POTLUCK_BIN", bin, 1)
    func press(_ pasted: String, go: Bool) -> (end: String, args: [String], stdin: String, seen: String) {
        for f in ["seen", "args", "stdin", "asked"] { try? FileManager.default.removeItem(atPath: tmp + "/" + f) }
        let run = d.run(pasted: pasted)!
        var end = ""
        func once(_ go: Bool) {
            end = ""
            runAction(run.args, stdin: run.stdin, admin: { _ in .ran("") }, ask: { _, answer in answer(go) }) { r in
                switch r { case .done: end = "done"; case .failed(let w, _): end = "failed: " + w.raw; case .cancelled: end = "cancelled" }
            }
            let until = Date().addingTimeInterval(20)
            while end.isEmpty && Date() < until { RunLoop.main.run(until: Date().addingTimeInterval(0.02)) }
        }
        once(go)
        if !go { once(true) } // Ask again
        func read(_ f: String) -> String { (try? String(contentsOfFile: tmp + "/" + f, encoding: .utf8)) ?? "" }
        return (end, read("args").split(separator: "\n").map(String.init), read("stdin"), read("seen"))
    }
    let with = d.invite_args!.joined(separator: " "), without = d.args!.joined(separator: " ")
    for go in [true, false] {
        let r = press("  \(secret)\n", go: go)
        expect(r.end == "done" && r.args == [with, with], "invite, go \(go): \(r.end) \(r.args)")
        expect(r.stdin == secret + "\n" + secret + "\n", "invite, go \(go): stdin \(r.stdin.debugDescription)")
        expect(r.seen.contains("ps-potluck") && !r.seen.contains(secretPart), "invite, go \(go): ps and the environment never show it")
    }
    let none = press("  ", go: true)
    expect(none.end == "done" && none.args == [without, without] && none.stdin.isEmpty, "nothing pasted: \(none.end) \(none.args) \(none.stdin.debugDescription)")
} else { expect(false, "ready: no button") }
// A run that ends without reading its stdin does not end this app, however
// much was pasted.
setenv("POTLUCK_BIN", "/usr/bin/false", 1)
expect(runPotluck(["x"], stdin: String(repeating: "x", count: 1 << 20)).0 == 1, "stdin nobody reads")

// The Trash takes only this app's own bundle, and nothing is deleted.
func bundle(_ name: String, _ id: String) -> URL {
    let u = URL(fileURLWithPath: tmp + "/" + name)
    try! FileManager.default.createDirectory(at: u.appendingPathComponent("Contents"), withIntermediateDirectories: true)
    try! PropertyListSerialization.data(fromPropertyList: ["CFBundleIdentifier": id], format: .xml, options: 0).write(to: u.appendingPathComponent("Contents/Info.plist"))
    return u
}
var trashed: [String] = []
let mine = bundle("Potluck.app", "sh.potluck.menu")
expect(trashApp(mine, trash: { trashed.append($0.lastPathComponent) }) == nil && trashed == ["Potluck.app"], "trash: this app")
for u in [bundle("Other.app", "com.example.other"), bundle("plain folder", "sh.potluck.menu"), bundle("AppTranslocation/x/Potluck.app", "sh.potluck.menu"), URL(fileURLWithPath: tmp)] {
    expect(trashApp(u, trash: { trashed.append($0.path) }) != nil && trashed.count == 1, "trash: refuses \(u.path)")
}
struct NoTrash: Error {}
expect(trashApp(mine, trash: { _ in throw NoTrash() }) != nil, "trash: a failed move says so")

// Only the join page's own invite link, alone, comes in by potluck://.
let good = "https://potluck.sh/join#v1." + String(repeating: "aZ_-", count: 8)
func enc(_ s: String) -> String { s.addingPercentEncoding(withAllowedCharacters: .alphanumerics)! }
expect(inviteIn(URL(string: "potluck://open?invite=" + enc(good))!) == good, "link: the join page's")
expect(inviteURL(good).flatMap(inviteIn) == good, "link: handed to another copy")
for bad in ["potluck://open", "potluck://open?invite=", "potluck://run?invite=" + enc(good), "https://open?invite=" + enc(good), "potluck://open?invite=" + enc(good) + "&invite=" + enc(good),
            "potluck://open?args=x&invite=" + enc(good), "potluck://open/x?invite=" + enc(good), "potluck://open?invite=" + enc(good + "\n"), "potluck://open?invite=" + enc(good + " --yes"),
            "potluck://open?invite=" + enc("https://evil.example/join#v1.aaaaaaaaaaaaaaaaaaaaa"), "potluck://open?invite=" + enc("https://potluck.sh.evil.example/join#v1.aaaaaaaaaaaaaaaaaaaaa"),
            "potluck://open?invite=--yes", "potluck://u:p@open?invite=" + enc(good), "potluck://open?invite=" + enc(good) + "#x",
            "potluck://open?invite=" + enc("https://potluck.sh/join#v1." + String(repeating: "a", count: 601))] {
    expect(inviteIn(URL(string: bad)!) == nil, "link: refuses \(bad)")
}
expect(inviteFill(field: " ", invite: good) == .fill && inviteFill(field: good + "\n", invite: good) == .fill && inviteFill(field: "mine", invite: good) == .ask, "link: never over other text without a question")

// Moving to Applications: only over a recorded copy of this app.
let from = tmp + "/Downloads", apps = tmp + "/Apps"
let me = bundle("Downloads/Potluck.app", "sh.potluck.menu")
let to = apps + "/Potluck.app"
func move(recorded: [String], quits: Bool = true) -> String {
    do { return try install(me, into: apps, recorded: recorded, quit: { _ in quits }, trash: { try FileManager.default.moveItem(at: $0, to: URL(fileURLWithPath: tmp + "/trash-" + UUID().uuidString)) }).path } catch { return "refused" }
}
func there() -> [String] { ((try? FileManager.default.contentsOfDirectory(atPath: apps)) ?? []).sorted() }
setxattr(me.path, "com.apple.quarantine", "x", 1, 0, 0)
expect(move(recorded: []) == to && there() == ["Potluck.app"], "move: an empty folder \(there())")
expect(getxattr(to, "com.apple.quarantine", nil, 0, 0, 0) < 0, "move: the copy is not quarantined again")
expect(FileManager.default.fileExists(atPath: from + "/Potluck.app"), "move: copies")
expect(move(recorded: []) == "refused" && there() == ["Potluck.app"], "move: over a copy never recorded there")
expect(move(recorded: [to], quits: false) == "refused" && there() == ["Potluck.app"], "move: over one that still runs")
expect(move(recorded: [to]) == to && there() == ["Potluck.app"], "move: over a recorded copy")
try! FileManager.default.removeItem(atPath: to)
_ = bundle("Apps/Potluck.app", "com.example.recipes")
expect(move(recorded: [to]) == "refused" && NSDictionary(contentsOfFile: to + "/Contents/Info.plist")?["CFBundleIdentifier"] as? String == "com.example.recipes", "move: never over another app")
try! FileManager.default.removeItem(atPath: to)
try! FileManager.default.createSymbolicLink(atPath: to, withDestinationPath: me.path)
expect(move(recorded: [to]) == "refused", "move: never over a link")
try! FileManager.default.removeItem(atPath: to)

// The app under its old name: only potluck's listed places, only this app,
// only for the renamed one where apps are installed.
let mineOld = bundle("Apps/Potluck Menu.app", "sh.potluck.menu").path
let notMine = bundle("Other/Potluck Menu.app", "com.example.recipes").path
let renamed = URL(fileURLWithPath: to)
expect(oldApps([mineOld, notMine, me.path, "/nowhere/Potluck Menu.app"], me: renamed, folders: [apps]).map(\.path) == [mineOld], "old app: only this app's own, under its old name")
expect(oldApps([], me: renamed, folders: [apps]).isEmpty && oldApps([mineOld], me: me, folders: [apps]).isEmpty && oldApps([mineOld], me: URL(fileURLWithPath: mineOld), folders: [apps]).isEmpty, "old app: none listed, not installed, or the old one itself")
let two = bundle("Two/Potluck.app", appBundleID)
expect(installedCopy(["/nowhere", two.deletingLastPathComponent().path])?.path == two.path && installedCopy(["/nowhere", tmp + "/Other"]) == nil, "an installed copy: ours only")
// The copy the app was moved from goes to the Trash only when it is that
// very build, outside the Applications folders, and named by the app's own
// record or by macOS for its translocated copy.
let fmm = FileManager.default
func build(_ name: String, program: String, id: String = appBundleID) -> URL {
    let u = URL(fileURLWithPath: tmp + "/" + name)
    try! fmm.createDirectory(at: u.appendingPathComponent("Contents/MacOS"), withIntermediateDirectories: true)
    try! PropertyListSerialization.data(fromPropertyList: ["CFBundleIdentifier": id, "CFBundleExecutable": "PotluckMenu"], format: .xml, options: 0).write(to: u.appendingPathComponent("Contents/Info.plist"))
    try! program.write(to: u.appendingPathComponent("Contents/MacOS/PotluckMenu"), atomically: true, encoding: .utf8)
    return u
}
let installed = build("Moved/Applications/Potluck.app", program: "v1")
let download = build("Moved/Downloads/Potluck.app", program: "v1")
let movedFolders = [tmp + "/Moved/Applications"]
let translocated = URL(fileURLWithPath: tmp + "/AppTranslocation/ABC/d/Potluck.app")
expect(sameBuild(download, installed) && !sameBuild(build("Moved/Other/Potluck.app", program: "v2"), installed), "moved: the same build, byte for byte")
expect(movedFrom(translocated, original: download, installed: installed, recorded: [], folders: movedFolders) == download, "moved: the download macOS ran from a temporary place")
expect(movedFrom(download, original: nil, installed: installed, recorded: [download.path], folders: movedFolders) == download, "moved: a recorded place")
expect(movedFrom(download, original: nil, installed: installed, recorded: [], folders: movedFolders) == nil, "moved: never a place not recorded")
expect(movedFrom(translocated, original: nil, installed: installed, recorded: [], folders: movedFolders) == nil, "moved: macOS does not say where it was")
let newer = build("Moved/Newer/Potluck.app", program: "v2")
expect(movedFrom(translocated, original: newer, installed: installed, recorded: [], folders: movedFolders) == nil, "moved: another build is the person's own")
let notOurs = build("Moved/Recipes/Potluck.app", program: "v1", id: "com.example.recipes")
expect(movedFrom(translocated, original: notOurs, installed: installed, recorded: [], folders: movedFolders) == nil, "moved: another app")
expect(movedFrom(translocated, original: installed, installed: installed, recorded: [], folders: movedFolders) == nil, "moved: never the installed copy")
let otherInstalled = build("Moved/Applications/Old/Potluck.app", program: "v1")
expect(movedFrom(otherInstalled, original: nil, installed: installed, recorded: [otherInstalled.path], folders: movedFolders) == nil, "moved: never one in an Applications folder")
try! fmm.createSymbolicLink(atPath: tmp + "/Moved/Link.app", withDestinationPath: download.path)
expect(movedFrom(translocated, original: URL(fileURLWithPath: tmp + "/Moved/Link.app"), installed: installed, recorded: [], folders: movedFolders) == nil, "moved: never a link")
try! fmm.createSymbolicLink(atPath: tmp + "/Moved/Apps", withDestinationPath: tmp + "/Moved/Applications")
let viaLink = URL(fileURLWithPath: tmp + "/Moved/Apps/Potluck.app"), viaOld = URL(fileURLWithPath: tmp + "/Moved/Apps/Old/Potluck.app")
expect(movedFrom(viaLink, original: nil, installed: installed, recorded: [viaLink.path], folders: movedFolders) == nil, "moved: never the installed copy by a linked folder")
expect(movedFrom(viaOld, original: nil, installed: installed, recorded: [viaOld.path], folders: movedFolders) == nil, "moved: never one in an Applications folder by a linked folder")
expect(movedFrom(download, original: nil, installed: installed, recorded: [download.path], folders: movedFolders) == download, "moved: still the download, once the links are followed")
expect(translocatedFrom(download) == nil, "moved: a place that is not translocated has no original")

// Moving itself to the Trash, the app takes its caches and preferences with
// it, but not what another running copy unpacked. A domain of the test's own:
// the real app's preferences on this Mac are never touched.
let domain = "sh.potluck.menu-test.\(getpid()).\(UUID().uuidString)"
let caches = URL(fileURLWithPath: tmp + "/Home/Library/Caches/" + domain), prefs = URL(fileURLWithPath: tmp + "/Home/Library/Preferences/" + domain + ".plist")
expect(appFiles(home: URL(fileURLWithPath: "/Users/x")).preferences.path == "/Users/x/Library/Preferences/sh.potluck.menu.plist" && appFiles(home: URL(fileURLWithPath: "/Users/x")).caches.path == "/Users/x/Library/Caches/sh.potluck.menu", "forget: the app's own files")
try! fmm.createDirectory(at: caches.appendingPathComponent("potluck.1"), withIntermediateDirectories: true)
try! fmm.createDirectory(at: caches.appendingPathComponent("com.apple.nsurlsessiond"), withIntermediateDirectories: true)
try! fmm.createDirectory(at: prefs.deletingLastPathComponent(), withIntermediateDirectories: true)
try! "x".write(to: prefs, atomically: true, encoding: .utf8)
var forgotten: [String] = []
forgetApp(caches: caches, preferences: prefs, domain: domain + "-other", forget: { forgotten.append($0) })
expect(fmm.fileExists(atPath: prefs.path) && forgotten == [domain + "-other"], "forget: only its own domain's files")
forgetApp(caches: caches, preferences: prefs, domain: domain, forget: { forgotten.append($0) })
expect(!fmm.fileExists(atPath: prefs.path) && forgotten.last == domain && (try? fmm.contentsOfDirectory(atPath: caches.path)) == ["potluck.1"], "forget: preferences gone, another copy's Potluck stays")
try! fmm.removeItem(at: caches.appendingPathComponent("potluck.1"))
forgetApp(caches: caches, preferences: prefs, domain: domain, forget: { _ in })
expect(!fmm.fileExists(atPath: caches.path), "forget: the caches folder, once nothing stays in it")

// The release an app carries: the installer beside it is run on it, with
// nothing of this Mac's home or potluck in it; what it leaves is run with the
// release named, and a refusal leaves nothing.
let res = tmp + "/Resources", fm = FileManager.default
try! fm.createDirectory(atPath: res + "/release", withIntermediateDirectories: true)
let bundled = Bundled(resources: res, caches: tmp + "/Caches/app")
expect(!bundled.carried && bundled.potluck == nil && bundled.refused == nil, "carried: nothing yet")
try! "{}".write(toFile: res + "/release/manifest.json", atomically: true, encoding: .utf8)
setenv("POTLUCK_UPDATE_KEY", "ed25519:mine", 1)
try! "#!/bin/sh\n[ \"$*\" = \"--from \(res)/release --no-modify-path\" ] && [ ! -e \"$POTLUCK_HOME\" ] && [ -z \"$POTLUCK_UPDATE_KEY\" ] || exit 3\nmkdir -p \"$POTLUCK_INSTALL_DIR\" && printf '#!/bin/sh\\n' >\"$POTLUCK_INSTALL_DIR/potluck\" && chmod 755 \"$POTLUCK_INSTALL_DIR/potluck\"\n".write(toFile: res + "/install.sh", atomically: true, encoding: .utf8)
expect(bundled.carried, "carried: a release and its installer")
bundled.prepare()
expect(bundled.potluck == tmp + "/Caches/app/potluck.\(getpid())/potluck" && bundled.refused == nil, "carried: checked and ready, \(bundled.refused ?? "")")
expect(bundled.environment(for: bundled.potluck!) == ["POTLUCK_BUNDLED_RELEASE": res + "/release", "POTLUCK_NO_AUTOSTART": "1"] && bundled.environment(for: "/usr/local/bin/potluck").isEmpty, "carried: only its own potluck is told of it")
let step = ["--json", "upgrade", "--release", bundled.release]
expect(bundled.refusal(step, with: bundled.potluck!) == nil && bundled.refusal(step, with: "/usr/local/bin/potluck") != nil && bundled.refusal(["host", "status"], with: "/usr/local/bin/potluck") == nil, "carried: another program never installs it")
expect(!bundled.wanted(again: true), "carried: ready, so never made again")
bundled.vanished()
expect(bundled.refused == nil, "carried: there, so nothing vanished")
try! fm.removeItem(atPath: bundled.cache)
expect(bundled.wanted(again: false), "carried: what was made is gone, so made again")
bundled.prepare()
try! fm.removeItem(atPath: bundled.cache)
bundled.vanished()
expect(!bundled.wanted(again: false) && bundled.wanted(again: true) && bundled.refused != nil, "carried: gone as soon as it was made, so made again only when asked")
try! "mkdir -p \"$POTLUCK_INSTALL_DIR\"; touch \"$POTLUCK_INSTALL_DIR/potluck\"; echo 'does not verify' >&2; exit 1\n".write(toFile: res + "/install.sh", atomically: true, encoding: .utf8)
bundled.prepare()
expect(bundled.potluck == nil && bundled.refused == "does not verify" && !fm.fileExists(atPath: bundled.cache), "carried: refused, \(bundled.refused ?? "ready")")
expect(!bundled.wanted(again: false) && bundled.wanted(again: true), "carried: refused, so checked again only when asked")
bundled.clear()
expect(bundled.refused == nil && bundled.wanted(again: false), "carried: cleared, so made again when it is needed")
// A second copy of the app, and one that ended without clearing.
let ended = Process()
ended.executableURL = URL(fileURLWithPath: "/usr/bin/true")
try! ended.run()
ended.waitUntilExit()
for d in ["potluck", "potluck.\(getppid())", "potluck.\(ended.processIdentifier)", "potluck.mine", "other"] { try! fm.createDirectory(atPath: tmp + "/Caches/app/" + d, withIntermediateDirectories: true) }
bundled.clear()
expect(try! fm.contentsOfDirectory(atPath: tmp + "/Caches/app").sorted() == ["other", "potluck.\(getppid())", "potluck.mine"].sorted(), "carried: cleared takes only what no running app uses: \(try! fm.contentsOfDirectory(atPath: tmp + "/Caches/app"))")
// The app's folder goes while it runs.
let moving = Bundled(resources: res, caches: tmp + "/Caches/app")
expect(!moving.moved, "carried: in its place")
try! fm.moveItem(atPath: res, toPath: res + ".moved")
expect(moving.moved && !moving.wanted(again: true), "carried: moved while open")
expect(moving.refusal(["--release", moving.release], with: "x") == movedWords && moving.refusal(["host"], with: "x") == nil, "carried: moved, so what names it is not run")
expect(noRunWords([], moving) == movedWords && noRunWords([DaemonRecord(program: "supper", exe: "/bin/ls", pid: 1)], moving) != movedWords, "carried: moved, and nothing else to run")
try! fm.moveItem(atPath: res + ".moved", toPath: res)
expect(!Bundled(resources: tmp + "/slim", caches: tmp + "/Caches/app").moved, "carried: an app with none has not moved")
expect(noPotluckScreen(refused: nil, moved: false, records: []).act == getPotluck, "none: a way to get it")
expect(noPotluckScreen(refused: "x", moved: false, records: []).words == bundledRefusedWords, "none: refused")
expect(noPotluckScreen(refused: "x", moved: true, records: []).words == movedWords, "none: moved")
let supper = noPotluckScreen(refused: "x", moved: true, records: [DaemonRecord(program: "supper", exe: "/bin/ls", pid: 1)])
expect(supper.act == nil && supper.heading == "Potluck isn't running", "none: another program's is there, whatever became of the app's own")
unsetenv("POTLUCK_UPDATE_KEY")
// The machine's records: another program's is run only while its daemon
// lives, else potluck's own, daemon or not; either way there is a Potluck here.
let recs = tmp + "/daemons"
setenv("POTLUCK_DAEMONS_DIR", recs, 1)
func record(_ program: String, _ exe: String, _ pid: Int32) {
    try! "{\"program\":\"\(program)\",\"exe\":\"\(exe)\",\"pid\":\(pid)}".write(toFile: recs + "/\(program).json", atomically: true, encoding: .utf8)
    chmod(recs + "/\(program).json", 0o600)
}
try! fm.createDirectory(atPath: recs, withIntermediateDirectories: true)
chmod(recs, 0o700)
expect(daemonRecords().isEmpty && recordedProgram() == nil, "records: none")
record("gone", tmp + "/nowhere", getpid())
expect(daemonRecords().isEmpty, "records: a program that is not there")
record("supper", "/bin/ls", Int32.max)
expect(daemonRecords().count == 1 && recordedProgram() == nil, "records: a stopped daemon's program is there, and not run")
record("supper", "/bin/ls", getpid())
expect(recordedProgram() == "/bin/ls", "records: a live daemon's program")
record("potluck", "/bin/cat", getpid())
expect(recordedProgram() == "/bin/ls", "records: the other program's live daemon first")
record("supper", "/bin/ls", Int32.max)
record("potluck", "/bin/cat", Int32.max)
expect(recordedProgram() == "/bin/cat", "records: else potluck's own, daemon or not")
chmod(recs + "/potluck.json", 0o644)
expect(recordedProgram() == nil, "records: one open to others does not count")
expect(noProgramWords().contains("Start supper"), "records: how to go on, \(noProgramWords())")
record("zm", "/bin/ls", getpid())
record("supper", "/bin/ls", getpid())
expect(recordedProgram() == nil && noProgramWords().contains("supper and zm"), "records: two live ones")
unsetenv("POTLUCK_DAEMONS_DIR")
let suite = "potluck-test-" + UUID().uuidString
let places = Places(defaults: UserDefaults(suiteName: suite)!)
places.record("/a/Potluck.app"); places.record("/a/Potluck.app"); places.record("/x/AppTranslocation/y/Potluck.app")
expect(places.all == ["/a/Potluck.app"], "places: \(places.all)")
UserDefaults().removePersistentDomain(forName: suite)

// The app's own acts: the ones it knows have buttons.
let acts = all["status-lending.json"]?.setup?.view?.sections.flatMap { $0.rows ?? [] }.compactMap { $0.do }.filter { $0.app != nil } ?? []
expect(acts.map(\.app) == ["login_item"] && acts.allSatisfy(\.known), "login item: a button")
expect((try? JSONDecoder().decode(SetupDo.self, from: Data("{\"label\":\"x\",\"app\":\"later\"}".utf8)))?.known == false, "an act this app does not know has none")
appEnvironment = { ["POTLUCK_LOGIN_ITEM": "off"] }
setenv("POTLUCK_BIN", "/usr/bin/env", 1)
let heard = String(decoding: runPotluck([]).1, as: UTF8.self)
expect(heard.contains("POTLUCK_LOGIN_ITEM=off") && heard.contains("HOME="), "potluck learns the login item")
appEnvironment = { [:] }

// A failed action shows potluck's plain words; the rest is for Copy details.
let failure = String(decoding: answer("error.txt"), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
expect(commandTrouble(failure, 1) == Trouble(words: "That isn't a whole invite.", detail: "checking the invite\nthis is not a whole invite\ncopy the whole invite again: potluck invite <you> --host"), "error: \(commandTrouble(failure, 1))")
expect(commandTrouble("launchctl bootstrap gui/501: exit status 5", 1) == Trouble(words: unexplained, detail: "launchctl bootstrap gui/501: exit status 5") && commandTrouble("", 2).detail == "potluck exited 2", "an error with no plain form is copied, not shown")
expect(commandTrouble(#"{"error":{"code":"bad_request","message":"not whole","hint":"again"}}"#, 1) == Trouble(words: "not whole\nagain", detail: "not whole\nagain"), "an older potluck's refusal is its words")
expect(commandTrouble(#"{"error":{"code":"internal","message":"sql"}}"#, 1) == Trouble(words: unexplained, detail: "sql"), "its own failure is not")
expect(commandTrouble("Start supper.", notRun) == Trouble(words: "Start supper."), "the app's own words are shown")

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
