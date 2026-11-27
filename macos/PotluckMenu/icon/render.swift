// Renders an SVG to square PNGs with macOS's own WebKit, at exact pixel
// sizes whatever the display: render <svg> <out dir> <size>...
// Each size is drawn as an image that wide, so the SVG's
// `@media (max-width: ...)` rules give the small sizes their own drawing.

import AppKit
import WebKit

func fail(_ why: String) -> Never {
    FileHandle.standardError.write(Data("render: \(why)\n".utf8))
    exit(1)
}

let args = CommandLine.arguments
let sizes = args.dropFirst(3).compactMap { Int($0) }
guard args.count > 3, sizes.count == args.count - 3 else { fail("usage: render <svg> <out dir> <size>...") }
let marker = #"width="1024" height="1024""#
let svg = (try? String(contentsOfFile: args[1], encoding: .utf8)) ?? ""
guard svg.contains(marker) else { fail("\(args[1]) is not an SVG with \(marker)") }

@MainActor func draw(_ web: WKWebView) async throws {
    for n in sizes {
        let sized = svg.replacingOccurrences(of: marker, with: "width=\"\(n)\" height=\"\(n)\"")
        let js = """
        const img = new Image();
        img.src = "data:image/svg+xml;base64," + src;
        await img.decode();
        const c = document.createElement("canvas");
        c.width = c.height = n;
        c.getContext("2d").drawImage(img, 0, 0, n, n);
        return c.toDataURL("image/png").split(",")[1];
        """
        let out = try await web.callAsyncJavaScript(js, arguments: ["src": Data(sized.utf8).base64EncodedString(), "n": n], contentWorld: .page)
        guard let b64 = out as? String, let png = Data(base64Encoded: b64) else { fail("WebKit gave no \(n)x\(n) picture") }
        try png.write(to: URL(fileURLWithPath: args[2]).appendingPathComponent("\(n).png"))
    }
}

final class Page: NSObject, WKNavigationDelegate {
    func webView(_ web: WKWebView, didFinish n: WKNavigation!) {
        Task { @MainActor in
            do { try await draw(web) } catch { fail("\(error)") }
            exit(0)
        }
    }
    func webView(_ web: WKWebView, didFail n: WKNavigation!, withError e: Error) { fail("\(e)") }
    func webViewWebContentProcessDidTerminate(_ web: WKWebView) { fail("WebKit's process ended") }
}

NSApplication.shared.setActivationPolicy(.prohibited)
let page = Page()
let web = WKWebView(frame: NSRect(x: 0, y: 0, width: 100, height: 100))
web.navigationDelegate = page
web.loadHTMLString("<!doctype html>", baseURL: nil)
DispatchQueue.main.asyncAfter(deadline: .now() + 120) { fail("no answer from WebKit in 2 minutes") }
NSApp.run()
