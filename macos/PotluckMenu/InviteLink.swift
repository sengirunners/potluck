// potluck://open?invite=<the invite link, URL-encoded>, which the join page's
// "Open in Potluck" opens. Any web page can open one, so the app only puts
// the invite in the setup window's field: the person presses the button.
// The invite is never logged and never an argument. Plain Foundation, so
// macos/PotluckMenu/test.sh compiles it.

import Foundation

/// The invite link in url, or nil when url is not exactly that: syntax only
/// (the join page's own, site/public/join.js); potluck judges the invite.
func inviteIn(_ url: URL) -> String? {
    guard url.absoluteString.utf8.count <= 2048,
          let c = URLComponents(url: url, resolvingAgainstBaseURL: false),
          c.scheme?.lowercased() == "potluck", c.host?.lowercased() == "open",
          c.path.isEmpty || c.path == "/", c.user == nil, c.password == nil, c.port == nil, c.fragment == nil,
          let items = c.queryItems, items.count == 1, items[0].name == "invite", let invite = items[0].value
    else { return nil }
    let shape = "^https://potluck\\.sh/join#v1\\.[A-Za-z0-9_-]{16,600}\\z"
    return invite.range(of: shape, options: .regularExpression) == nil ? nil : invite
}

/// The link that gives another copy of the app the same invite.
func inviteURL(_ invite: String) -> URL? {
    var c = URLComponents()
    c.scheme = "potluck"
    c.host = "open"
    c.queryItems = [URLQueryItem(name: "invite", value: invite)]
    return c.url
}

/// What a link's invite does to the field, which holds text.
enum InviteFill { case fill, ask }

func inviteFill(field: String, invite: String) -> InviteFill {
    let has = field.trimmingCharacters(in: .whitespacesAndNewlines)
    return has.isEmpty || has == invite ? .fill : .ask
}
