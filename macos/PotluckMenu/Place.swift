// Where the app is, and moving it: to an Applications folder, and an app
// under its old name to the Trash. A bundle is replaced or removed only when
// its bundle id is this app's and it was recorded at that very place (the
// app's own launches, or potluck's note of where the app asked from); it is
// moved to the Trash, never deleted. Plain Foundation, so
// macos/PotluckMenu/test.sh compiles it.

import Foundation

let appBundleID = "sh.potluck.menu"
let appName = "Potluck.app"
let oldAppName = "Potluck Menu.app"

/// Whether bundle is this app (any copy of it): an app bundle, not a link,
/// whose Info.plist names it.
func isThisApp(_ bundle: URL) -> Bool {
    guard bundle.pathExtension == "app", (try? FileManager.default.destinationOfSymbolicLink(atPath: bundle.path)) == nil,
          let info = NSDictionary(contentsOf: bundle.appendingPathComponent("Contents/Info.plist"))
    else { return false }
    return info["CFBundleIdentifier"] as? String == appBundleID
}

/// macOS runs a downloaded app it has not yet let out of quarantine from a
/// read-only copy at a path of its own.
func isTranslocated(_ path: String) -> Bool { path.contains("/AppTranslocation/") }

/// Where macOS translocated the app at bundle from: the copy the person
/// opened, such as one in Downloads (nil: bundle is not translocated, or
/// macOS does not say). It asks Security's SecTranslocateCreateOriginalPathForURL,
/// which has no public header, so it is looked up by name.
func translocatedFrom(_ bundle: URL) -> URL? {
    guard isTranslocated(bundle.path) else { return nil }
    typealias Original = @convention(c) (CFURL, UnsafeMutablePointer<Unmanaged<CFError>?>?) -> Unmanaged<CFURL>?
    guard let sec = dlopen("/System/Library/Frameworks/Security.framework/Security", RTLD_LAZY),
          let sym = dlsym(sec, "SecTranslocateCreateOriginalPathForURL"),
          let u = unsafeBitCast(sym, to: Original.self)(bundle as CFURL, nil)?.takeRetainedValue() as URL?,
          !isTranslocated(u.path)
    else {
        // Then the copy the person opened stays where it is: say so, for a bug report.
        NSLog("Potluck: macOS did not say where this translocated copy came from, so it stays there")
        return nil
    }
    return u.standardizedFileURL
}

/// Whether the bundles a and b are the same build of this app: both this
/// app, with the same Info.plist, program and code signature, byte for byte.
func sameBuild(_ a: URL, _ b: URL) -> Bool {
    guard isThisApp(a), isThisApp(b),
          let info = NSDictionary(contentsOf: a.appendingPathComponent("Contents/Info.plist")),
          let exe = info["CFBundleExecutable"] as? String, !exe.contains("/")
    else { return false }
    let files = ["Contents/Info.plist", "Contents/MacOS/" + exe, "Contents/_CodeSignature/CodeResources"]
    return files.allSatisfy { f in
        let x = FileManager.default.contents(atPath: a.appendingPathComponent(f).path)
        let y = FileManager.default.contents(atPath: b.appendingPathComponent(f).path)
        return x == y && (x != nil || f.hasSuffix("CodeResources")) // an unsigned build has none
    }
}

/// The copy the app at me was moved from, for the Trash once the copy at
/// installed runs: me itself, or the place macOS translocated it from
/// (original), only when it is this app outside the Applications folders,
/// recorded as one of the app's places or the one macOS names for it, and
/// the very build now installed. Else nil: a file that may be the person's
/// own is left where it is.
func movedFrom(_ me: URL, original: URL?, installed: URL, recorded: [String], folders: [String]) -> URL? {
    let from: URL
    if isTranslocated(me.path) {
        guard let o = original else { return nil }
        from = o
    } else {
        guard recorded.contains(me.path) else { return nil }
        from = me
    }
    // A folder on the way may be a link (to an Applications folder, even
    // to the installed copy): the paths and the files themselves are
    // compared with every link followed.
    let real = canonical(from), realFolders = folders.map { canonical(URL(fileURLWithPath: $0)) }
    guard real != canonical(installed), !isInstalled(from.path, folders: folders), !isInstalled(real, folders: realFolders),
          let a = fileID(from), let b = fileID(installed), !a.isEqual(b),
          sameBuild(from, installed) // which takes no link
    else { return nil }
    return from
}

/// The path to u with every link and firmlink followed, in the case the
/// disk keeps it (the path itself when there is no file).
private func canonical(_ u: URL) -> String {
    (try? u.resolvingSymlinksInPath().resourceValues(forKeys: [.canonicalPathKey]))?.canonicalPath ?? u.resolvingSymlinksInPath().path
}

/// What macOS knows the file at u as, the same by any path to it.
private func fileID(_ u: URL) -> NSObject? {
    (try? URL(fileURLWithPath: u.path).resourceValues(forKeys: [.fileResourceIdentifierKey]))?.fileResourceIdentifier as? NSObject
}

/// The folders an app is installed in.
func applicationFolders(home: String) -> [String] { ["/Applications", home + "/Applications"] }

/// Whether the app at path is in an Applications folder (or in one of its
/// own folders there).
func isInstalled(_ path: String, folders: [String]) -> Bool {
    folders.contains { path.hasPrefix($0 + "/") }
}

/// Where a copy of this app is installed already, in folders' order.
func installedCopy(_ folders: [String]) -> URL? {
    folders.map { URL(fileURLWithPath: $0).appendingPathComponent(appName) }.first(where: isThisApp)
}

/// Copies the app at from into folder as Potluck.app and returns its place.
/// A copy of this app there goes to the Trash first, only when recorded (an
/// app of another name or program is never touched); quit asks a running one
/// to quit and says whether it did. The copy is beside the place until it is
/// whole, so a failed one leaves what was there.
func install(_ from: URL, into folder: String, recorded: [String], quit: (URL) -> Bool, trash: (URL) throws -> Void) throws -> URL {
    let fm = FileManager.default
    let to = URL(fileURLWithPath: folder).appendingPathComponent(appName)
    if to.standardizedFileURL == from.standardizedFileURL { return to }
    guard isThisApp(from) else { throw Trouble(words: "Potluck can't tell where this app is. Move it to your Applications folder yourself in Finder.") }
    let there = (try? fm.attributesOfItem(atPath: to.path)) != nil // a dangling link counts
    if there && !(isThisApp(to) && recorded.contains(to.path)) {
        throw Trouble(words: "There is already an app named Potluck in your Applications folder, and Potluck can't tell that it's an older copy of this one. Move it to the Trash yourself in Finder, then open this one again.")
    }
    let part = URL(fileURLWithPath: folder).appendingPathComponent(".\(appName).\(UUID().uuidString)")
    do {
        try fm.createDirectory(atPath: folder, withIntermediateDirectories: true)
        try fm.copyItem(at: from, to: part)
    } catch {
        try? fm.removeItem(at: part) // this copy of ours, never whole
        throw Trouble(words: "Potluck couldn't copy itself to your Applications folder. Move it there yourself in Finder.", detail: "\(error)")
    }
    // The person let this app open: its copy opens as it is, not from a
    // quarantine path again.
    unquarantine(part)
    do {
        if there {
            guard quit(to) else { throw Trouble(words: "The Potluck in your Applications folder is still running. Quit it from its menu, then open this one again.") }
            try trash(to)
        }
        try fm.moveItem(at: part, to: to)
    } catch {
        try? fm.removeItem(at: part)
        throw (error as? Trouble) ?? Trouble(words: "Potluck couldn't move itself to your Applications folder. Move it there yourself in Finder.", detail: "\(error)")
    }
    return to
}

/// Takes the download quarantine off the app at bundle and everything in it.
func unquarantine(_ bundle: URL) {
    removexattr(bundle.path, "com.apple.quarantine", XATTR_NOFOLLOW)
    guard let all = FileManager.default.enumerator(atPath: bundle.path) else { return }
    for case let p as String in all { removexattr(bundle.path + "/" + p, "com.apple.quarantine", XATTR_NOFOLLOW) }
}

/// The apps of potluck's old_apps the app at `me` may move to the Trash: this
/// app under its old name, when `me` is the renamed one, installed.
func oldApps(_ listed: [String], me: URL, folders: [String]) -> [URL] {
    guard me.lastPathComponent == appName, isInstalled(me.path, folders: folders) else { return [] }
    return listed.map { URL(fileURLWithPath: $0) }.filter { $0.lastPathComponent == oldAppName && isThisApp($0) && !isTranslocated($0.path) }
}

/// The places the app was started from, which it keeps in its own
/// preferences.
struct Places {
    let defaults: UserDefaults
    static let key = "places"
    var all: [String] { defaults.stringArray(forKey: Places.key) ?? [] }

    func record(_ path: String) {
        guard !isTranslocated(path), !all.contains(path) else { return }
        defaults.set(Array((all + [path]).suffix(16)), forKey: Places.key)
    }
}
