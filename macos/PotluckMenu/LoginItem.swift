// The app's own login item (macOS's SMAppService): whether it starts when
// the person logs in. Only the app can read or change it; the words and the
// buttons are potluck's `login` check, which learns the state from the app
// (POTLUCK_LOGIN_ITEM, cli/host_setup.go).

import ServiceManagement

struct LoginItem {
    /// on, off, or refused (turned off in System Settings).
    var state: () -> String
    var turnOn: () throws -> Void
    var turnOff: () throws -> Void
    var openSettings: () -> Void

    static let system = LoginItem(
        state: {
            switch SMAppService.mainApp.status {
            case .enabled: return "on"
            case .requiresApproval: return "refused"
            default: return "off" // never registered, or not found
            }
        },
        turnOn: { try SMAppService.mainApp.register() },
        turnOff: { try SMAppService.mainApp.unregister() },
        openSettings: { SMAppService.openSystemSettingsLoginItems() })
}
