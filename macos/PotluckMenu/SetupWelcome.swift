import AppKit

/// The welcome stays above setup while its checks and next step change.
func setupWelcome() -> NSView {
    let icon = NSImageView(image: NSApp.applicationIconImage)
    icon.imageScaling = .scaleProportionallyUpOrDown
    icon.setAccessibilityElement(false)
    NSLayoutConstraint.activate([
        icon.widthAnchor.constraint(equalToConstant: 88),
        icon.heightAnchor.constraint(equalTo: icon.widthAnchor),
    ])

    let title = NSTextField(labelWithString: "Welcome to Potluck")
    title.font = .systemFont(ofSize: 26, weight: .semibold)
    let subtitle = NSTextField(wrappingLabelWithString: "Share this Mac with your team.")
    subtitle.font = .systemFont(ofSize: 14)
    subtitle.textColor = .secondaryLabelColor
    let text = NSStackView(views: [title, subtitle])
    text.orientation = .vertical
    text.alignment = .leading
    text.spacing = 5
    subtitle.widthAnchor.constraint(equalTo: text.widthAnchor).isActive = true

    let welcome = NSStackView(views: [icon, text])
    welcome.orientation = .horizontal
    welcome.alignment = .centerY
    welcome.spacing = 16
    return welcome
}
