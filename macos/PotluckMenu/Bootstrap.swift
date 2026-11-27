// The release this app may carry (Resources/release, beside the install.sh
// that checks it, Resources/install.sh): on a Mac with no potluck the app
// runs potluck from it, so the setup window can say what this Mac needs and
// install it. install.sh, the terminal's own installer, checks the signature
// and the tarball before anything of the release runs, and puts the programs
// in the app's cache directory; installing them is potluck's own upgrade.
// Plain Foundation, so macos/PotluckMenu/test.sh compiles it.

import Foundation

final class Bundled {
    /// The app's own.
    static let app = Bundled(resources: Bundle.main.resourcePath ?? "", caches: appFiles(home: FileManager.default.homeDirectoryForCurrentUser).caches.path)

    let resources: String
    private let caches: String
    private let hadRelease: Bool
    private let lock = NSLock()
    private var program: String?
    private var refusal: String?
    private var busy = false

    init(resources: String, caches: String) {
        self.resources = resources
        self.caches = caches
        hadRelease = FileManager.default.fileExists(atPath: resources + "/release/manifest.json")
    }

    /// Where this app's programs go: one directory a running app, so a second
    /// copy of it, opening or quitting, takes none of this one's away.
    var cache: String { caches + "/potluck.\(getpid())" }

    var release: String { resources + "/release" }
    private var installer: String { resources + "/install.sh" }

    /// Whether this app carries a release.
    var carried: Bool {
        let fm = FileManager.default
        return fm.fileExists(atPath: release + "/manifest.json") && fm.fileExists(atPath: installer)
    }

    /// Whether it carried one when it started and no longer does: the app was
    /// moved or deleted while it was open.
    var moved: Bool { hadRelease && !carried }

    /// Whether prepare is to be run now, on a Mac with no Potluck: it made
    /// none, or what it made is gone. After a refusal only when asked again.
    func wanted(again: Bool) -> Bool { carried && !preparing && potluck == nil && (again || refused == nil) }

    /// The checked potluck, once prepare has made it, while it is there.
    var potluck: String? {
        lock.lock()
        defer { lock.unlock() }
        return program.flatMap { FileManager.default.isExecutableFile(atPath: $0) ? $0 : nil }
    }

    /// Why prepare made none: the installer's own words.
    var refused: String? {
        lock.lock()
        defer { lock.unlock() }
        return refusal
    }

    /// Whether prepare is to run or runs: nothing is asked of potluck yet.
    var preparing: Bool {
        get { lock.lock(); defer { lock.unlock() }; return busy }
        set { lock.lock(); busy = newValue; lock.unlock() }
    }

    /// Checks the release and puts its programs in the cache directory, in
    /// place of any there. Off the main thread: it takes a few seconds.
    func prepare() {
        clear()
        let why = install()
        lock.lock()
        program = why == nil ? cache + "/potluck" : nil
        refusal = why
        busy = false
        lock.unlock()
        if why != nil { try? FileManager.default.removeItem(atPath: cache) }
    }

    /// What prepare made just now is gone already (something on this Mac
    /// removes it): a refusal, so it is made again when the person asks, not
    /// over and over.
    func vanished() {
        if potluck != nil { return }
        lock.lock()
        defer { lock.unlock() }
        if refusal == nil { refusal = "the programs unpacked to \(cache) were gone right after" }
    }

    /// Removes the cached programs, and those of apps that ended without it
    /// (and of versions with one directory for all, "potluck").
    func clear() {
        lock.lock()
        program = nil
        refusal = nil
        lock.unlock()
        let fm = FileManager.default
        for name in (try? fm.contentsOfDirectory(atPath: caches)) ?? [] {
            let parts = name.split(separator: ".", omittingEmptySubsequences: false)
            guard parts[0] == "potluck", parts.count <= 2 else { continue }
            if parts.count == 2 {
                guard let pid = pid_t(parts[1]), pid > 0 else { continue }
                if pid != getpid(), kill(pid, 0) == 0 || errno != ESRCH { continue } // its app may run
            }
            try? fm.removeItem(atPath: caches + "/" + name)
        }
    }

    private func install() -> String? {
        let none = cache + "/none" // never made: no home, daemon or settings of this Mac's takes part
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/bin/sh")
        p.arguments = [installer, "--from", release, "--no-modify-path"]
        let mine = ProcessInfo.processInfo.environment
        var env = ["PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "POTLUCK_INSTALL_DIR": cache, "POTLUCK_HOME": none, "POTLUCK_DAEMONS_DIR": none, "POTLUCK_NO_AUTOSTART": "1"]
        for k in ["HOME", "TMPDIR"] { env[k] = mine[k] }
        env["HOME"] = env["HOME"] ?? NSHomeDirectory()
        p.environment = env
        let out = Pipe()
        p.standardOutput = out
        p.standardError = out
        p.standardInput = FileHandle.nullDevice
        do { try p.run() } catch { return "\(error)" }
        let said = String(decoding: out.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        p.waitUntilExit()
        if p.terminationStatus != 0 { return said.isEmpty ? "the installer exited \(p.terminationStatus)" : said }
        return FileManager.default.isExecutableFile(atPath: cache + "/potluck") ? nil : "the installer left no potluck\n" + said
    }

    /// Why program is not to run args, or nil: what names the carried release
    /// is only for its own potluck, which never puts it over an installed one,
    /// and only while it is there. A step drawn before another was installed
    /// is not run with that one.
    func refusal(_ args: [String], with program: String) -> String? {
        if !args.contains(release) { return nil }
        if moved { return movedWords }
        return program == potluck ? nil : "A Potluck was installed on this Mac just now, so the copy in this app was not used. This window checks again by itself."
    }

    /// What a run of program is told: the release it can install, when it is
    /// this one's potluck, which starts no daemon from the cache directory.
    func environment(for program: String) -> [String: String] {
        program == potluck ? ["POTLUCK_BUNDLED_RELEASE": release, "POTLUCK_NO_AUTOSTART": "1"] : [:]
    }
}

/// The setup window's words when the carried release could not be used: its
/// check failed, or something else did (a full disk).
let bundledRefusedWords = "The Potluck that came with this app couldn't be checked and unpacked, so it isn't used. Press Check again in the setup window, or quit Potluck from its menu at the top of the screen and open it again. If this stays, quit it, move it to the Trash and download it again from potluck.sh."

/// Its words when the app and its release are no longer where they were.
let movedWords = "Potluck was moved or deleted while it was open, so it can't go on. Quit it from its menu at the top of the screen, then open it again from where it is now. If it's gone, download it again from potluck.sh."
