// Moving this app's own bundle to the Trash, after an uninstall. Plain
// Foundation, so macos/PotluckMenu/test.sh compiles it.

import Foundation

/// Moves the app at bundle to the Trash (trash does the move) and returns
/// nil, or returns why it did not. Only an app bundle whose Info.plist names
/// this app, at the place the person put it: macOS runs a downloaded app it
/// has not yet let out of quarantine from a read-only copy elsewhere, and
/// trashing that copy would leave the real one.
func trashApp(_ bundle: URL, trash: (URL) throws -> Void) -> String? {
    let plist = bundle.appendingPathComponent("Contents/Info.plist")
    let byHand = "Move the app to the Trash yourself in Finder."
    guard bundle.pathExtension == "app", let info = NSDictionary(contentsOf: plist),
          info["CFBundleIdentifier"] as? String == "sh.potluck.menu"
    else { return "\(bundle.path) is not this app's bundle. " + byHand }
    if bundle.path.contains("/AppTranslocation/") {
        return "macOS runs this app from a temporary copy. " + byHand
    }
    do { try trash(bundle) } catch { return "\(error.localizedDescription) " + byHand }
    return nil
}
