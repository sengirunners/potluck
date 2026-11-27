// What the app does by itself when it starts: offers to move to an
// Applications folder, starts Potluck again on a Mac set up to lend (a
// check's `auto`), moves the app under its old name to the Trash, and takes
// an invite from a potluck:// link. And its own acts on its login item.

import AppKit

/// What the setup window asks (potluck's menu names the same).
let setupArgs = ["--json", "host", "status", "--probe"]

/// What a start has done, so each is done once.
struct Launch {
    var places = Places(defaults: .standard)
    var settled = false // the app runs here: it moves nowhere now
    var invite: String? // a link's, until the setup window takes it
    var autoStarted = false
    var oldTried = Set<String>()
}

/// Asks the app running from bundle to quit, and whether it did in 10 s.
func quitApp(at bundle: URL) -> Bool {
    func running() -> [NSRunningApplication] {
        NSRunningApplication.runningApplications(withBundleIdentifier: appBundleID).filter {
            $0 != .current && !$0.isTerminated && $0.bundleURL?.standardizedFileURL == bundle.standardizedFileURL
        }
    }
    running().forEach { $0.terminate() }
    let until = Date().addingTimeInterval(10)
    while !running().isEmpty {
        if Date() > until { return false }
        RunLoop.current.run(until: Date().addingTimeInterval(0.1))
    }
    return true
}

func trashItem(_ u: URL) throws { try FileManager.default.trashItem(at: u, resultingItemURL: nil) }

extension App {
    var home: String { FileManager.default.homeDirectoryForCurrentUser.path }

    func application(_ a: NSApplication, open urls: [URL]) {
        guard let invite = urls.lazy.compactMap(inviteIn).first else { return } // not an invite: nothing to do
        launch.invite = invite
        if launch.settled { takeInvite() }
    }

    /// Puts a link's invite in the setup window (nothing while a removal
    /// runs, or Potluck is removed).
    func takeInvite() {
        guard let invite = launch.invite else { return }
        launch.invite = nil
        if held { return showUninstall() }
        setupWindow(setupArgs).offer(invite)
    }

    /// Offers to move the app to an Applications folder, when it is in none.
    /// A no is kept for that place, unless the person asked (asked). True:
    /// the moved copy goes on and this one ends.
    func offerMove(asked: Bool) -> Bool {
        let me = Bundle.main.bundleURL, folders = applicationFolders(home: home)
        if isInstalled(me.path, folders: folders) || !isThisApp(me) { return false }
        let key = "notMoved", no = UserDefaults.standard.stringArray(forKey: key) ?? []
        if !asked && no.contains(me.path) { return false }
        let there = installedCopy(folders)
        let a = moveQuestion(installed: there != nil)
        var answer = Dock.ask(a)
        if let there = there {
            if answer == .alertFirstButtonReturn {
                reopen(at: there, from: isTranslocated(me.path) ? me : nil) // only a download macOS ran from a temporary place: a copy kept elsewhere is the person's own
                return true
            }
            answer = answer == .alertSecondButtonReturn ? .alertFirstButtonReturn : .alertThirdButtonReturn
        }
        if answer != .alertFirstButtonReturn {
            if !asked && !isTranslocated(me.path) { UserDefaults.standard.set(Array((no + [me.path]).suffix(16)), forKey: key) }
            return false
        }
        let folder = there?.deletingLastPathComponent().path ?? (FileManager.default.isWritableFile(atPath: folders[0]) ? folders[0] : folders[1])
        do {
            let to = try install(me, into: folder, recorded: launch.places.all, quit: quitApp, trash: trashItem)
            launch.places.record(to.path)
            reopen(at: to, from: me)
            return true
        } catch {
            tell("Potluck wasn't moved", (error as? Trouble)?.words ?? "Move it there yourself in Finder.")
            return false
        }
    }

    /// The question before a move: with a copy installed already, which may
    /// be newer, Return opens that one.
    func moveQuestion(installed: Bool) -> NSAlert {
        let a = NSAlert()
        if installed {
            a.messageText = "Potluck is already in your Applications folder"
            a.informativeText = "Open the installed copy, or replace it if you meant to install this download."
            if isTranslocated(Bundle.main.bundlePath) { a.informativeText += " Opening either one moves this download to the Trash." }
            a.addButton(withTitle: "Open installed copy")
            a.addButton(withTitle: "Replace with this copy")
        } else {
            a.messageText = "Move Potluck to your Applications folder?"
            a.informativeText = "This keeps Potluck easy to find and lets it open when you log in."
            a.addButton(withTitle: "Move to Applications")
        }
        a.addButton(withTitle: "Not now").keyEquivalent = "\u{1b}"
        return a
    }

    /// Opens the copy at to, with the invite this one holds, then ends this
    /// one; from, where it was moved from, goes to the Trash when movedFrom
    /// is sure it is that very copy (for a download macOS runs from a
    /// temporary place, the place it was opened from).
    func reopen(at to: URL, from: URL?) {
        let c = NSWorkspace.OpenConfiguration()
        // This one has the same bundle id: only a new instance is surely that
        // copy's, unless that copy runs already.
        c.createsNewApplicationInstance = !NSRunningApplication.runningApplications(withBundleIdentifier: appBundleID).contains {
            $0 != .current && $0.bundleURL?.standardizedFileURL == to.standardizedFileURL
        }
        let done = { (app: NSRunningApplication?, _: Error?) in
            DispatchQueue.main.async {
                guard app != nil else {
                    self.tell("Potluck is in your Applications folder", "Open it from there.")
                    return NSApp.terminate(nil)
                }
                if let me = from, let copy = movedFrom(me, original: translocatedFrom(me), installed: to, recorded: self.launch.places.all,
                                                       folders: applicationFolders(home: self.home)) { try? trashItem(copy) }
                NSApp.terminate(nil)
            }
        }
        if let i = launch.invite, let u = inviteURL(i) {
            NSWorkspace.shared.open([u], withApplicationAt: to, configuration: c, completionHandler: done)
        } else {
            NSWorkspace.shared.openApplication(at: to, configuration: c, completionHandler: done)
        }
    }

    /// What a good status lets the app do by itself, once a start each.
    func settle(_ st: Status) {
        guard !held, let s = st.setup else { return }
        // On a Mac never set up, the setup window, the first time the app runs.
        if !UserDefaults.standard.bool(forKey: "setupShown") {
            UserDefaults.standard.set(true, forKey: "setupShown")
            if s.state == "none", s.view?.do != nil { setupWindow(setupArgs).show() }
        }
        if !launch.autoStarted {
            launch.autoStarted = true
            for d in s.checks.compactMap(\.do) where d.auto == true && d.args != nil && d.invite_args == nil { autoRun(d.args!) }
        }
        let old = oldApps(s.old_apps ?? [], me: Bundle.main.bundleURL, folders: applicationFolders(home: home))
        for o in old where launch.oldTried.insert(o.path).inserted { removeOld(o) }
    }

    /// Runs a check's auto action; what it leaves undone the checklist still
    /// says. It never asks for a password.
    func autoRun(_ args: [String]) {
        clicks += 1
        runAction(args, ask: { _, answer in answer(false) }) { _ in
            self.clicks -= 1
            self.refresh()
        }
    }

    /// Quits the app under its old name and moves it to the Trash, so one
    /// glyph shows; then offers the login item, which the old one may have been.
    func removeOld(_ o: URL) {
        guard quitApp(at: o), isThisApp(o) else { return }
        do { try trashItem(o) } catch { return }
        guard login.state() == "off" else { return }
        let a = NSAlert()
        a.messageText = "Potluck Menu is now called Potluck"
        a.informativeText = "The old app is in the Trash. Should Potluck start when you log in? You can change this later under Set up and check."
        a.addButton(withTitle: "Start at login")
        a.addButton(withTitle: "Not now")
        guard Dock.ask(a) == .alertFirstButtonReturn else { return }
        loginAct("login_item") { e in if let e = e { self.tell("Potluck won't start at login", e.words) } }
    }

    /// The app's own acts on its login item.
    func loginAct(_ what: String, _ done: @escaping (Trouble?) -> Void) {
        switch what {
        case "login_item":
            if isTranslocated(Bundle.main.bundlePath) { // macOS's copy is gone after a restart
                if offerMove(asked: true) { return done(nil) }
                return done(Trouble(words: "Potluck has to be in your Applications folder first. Move it there, then try again."))
            }
            do { try login.turnOn() } catch {
                if login.state() == "refused" { login.openSettings(); return done(nil) }
                return done(Trouble(words: "This Mac wouldn't add Potluck to the apps that open when you log in. You can add it yourself in System Settings > General > Login Items.", detail: "login item: \(error)"))
            }
            if login.state() == "refused" { login.openSettings() }
        case "login_item_off":
            do { try login.turnOff() } catch {
                return done(Trouble(words: "This Mac wouldn't take Potluck off the apps that open when you log in. You can do it yourself in System Settings > General > Login Items.", detail: "login item: \(error)"))
            }
        case "login_item_settings": login.openSettings()
        default: break // an act this build does not know
        }
        done(nil)
    }

    func tell(_ title: String, _ text: String) {
        let a = NSAlert()
        a.messageText = title
        a.informativeText = text
        _ = Dock.ask(a)
    }
}
