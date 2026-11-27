// Moving this app's own bundle to the Trash, after an uninstall. Plain
// Foundation, so macos/PotluckMenu/test.sh compiles it.

import Foundation

/// Moves the app at bundle to the Trash (trash does the move) and returns
/// nil, or returns why it did not. Only an app bundle whose Info.plist names
/// this app, at the place the person put it: macOS runs a downloaded app it
/// has not yet let out of quarantine from a read-only copy elsewhere, and
/// trashing that copy would leave the real one.
func trashApp(_ bundle: URL, trash: (URL) throws -> Void) -> String? {
    let byHand = "Move the app to the Trash yourself in Finder."
    guard isThisApp(bundle) else { return "Potluck can't tell where this app is. " + byHand }
    if isTranslocated(bundle.path) {
        return "macOS runs this app from a temporary copy. " + byHand
    }
    do { try trash(bundle) } catch { return "This Mac wouldn't move the app to the Trash. " + byHand }
    return nil
}

/// What the app keeps of its own once it is in the Trash: its caches folder
/// and its preferences, which nothing else removes (potluck uninstall names
/// them but cannot remove them while the app runs). The domain goes from
/// the preferences system first, so it is not written back. In the caches
/// folder a Potluck another running copy of the app unpacked (potluck.<pid>,
/// which Bundled.clear leaves) stays, and the folder with it.
func forgetApp(caches: URL, preferences: URL, domain: String, forget: (String) -> Void = { UserDefaults.standard.removePersistentDomain(forName: $0) }) {
    forget(domain)
    let fm = FileManager.default
    if preferences.lastPathComponent == domain + ".plist" { try? fm.removeItem(at: preferences) }
    guard caches.lastPathComponent == domain else { return }
    for name in (try? fm.contentsOfDirectory(atPath: caches.path)) ?? [] where !name.hasPrefix("potluck") {
        try? fm.removeItem(at: caches.appendingPathComponent(name))
    }
    rmdir(caches.path) // only when nothing stays in it
}

/// The app's caches folder and preferences file in home's Library.
func appFiles(home: URL) -> (caches: URL, preferences: URL) {
    let lib = home.appendingPathComponent("Library")
    return (lib.appendingPathComponent("Caches/" + appBundleID), lib.appendingPathComponent("Preferences/" + appBundleID + ".plist"))
}
