#!/usr/bin/env python3
"""Draws Potluck's GitHub social preview image: docs/img/social-preview.svg and social-preview.png (1280x640).

    python3 -I docs/img/social-preview.py

The picture is built with the SVG helpers in diagram.py (svg, box, text, group) from the explainer-video skill
(~/.claude/skills/explainer-video; set DIAGRAM_DIR to use another copy). Those helpers are written for a dark slide,
so this script sets their colour constants to the site's palette before it calls them. What the helpers do not do, and
this script adds: Newsreader (the site's font, embedded in the SVG so the file draws the same anywhere), the quilt
(the patchwork strip and diamond stitching of site/public/site.css, as SVG patterns), and the logo. The PNG is the
SVG screenshotted by headless Chrome (CHROME names the binary), as site/build/chrome.mjs does for the other pictures.

GitHub shows this image under Settings > Social preview; this script never uploads it.
"""
import base64
import os
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
FONTS = os.path.join(HERE, "..", "..", "site", "public", "fonts")
sys.path.insert(0, os.environ.get("DIAGRAM_DIR", os.path.expanduser("~/.claude/skills/explainer-video")))
import diagram as d  # noqa: E402

W, H = 1280, 640
CREAM, INK, SOFT, GREEN, RED = "#FBF7EE", "#1D261D", "#4B5648", "#2F6B34", "#C8402F"

# diagram.py's colours are for a dark slide: its helpers read these names when they draw.
d.INK, d.MUTED, d.PANEL, d.LINE, d.BG = INK, SOFT, "#FFFBF0", "#E5DCCA", CREAM


def font_face(name, weight, style, file):
    with open(os.path.join(FONTS, file), "rb") as f:
        data = base64.b64encode(f.read()).decode()
    return (f"@font-face{{font-family:{name};font-weight:{weight};font-style:{style};"
            f"src:url(data:font/woff2;base64,{data}) format('woff2')}}")


# The page's patchwork (site.css --patch, 24 px squares) and strip (--strip), as patterns.
PATCH = [["#E9A99B", "#F6E7C8", "#A9C98A"], ["#F0CB74", "#FFF3E2", "#9FC2DA"], ["#A9C98A", "#F6E7C8", "#E9A99B"]]
STRIP = ["#E9A99B", "#F0CB74", "#A9C98A", "#9FC2DA", "#F6E7C8", "#E9A99B", "#A9C98A"]
SCALE = 1.4  # the strip is 10 px on the site; 14 px reads at card size


def defs():
    strip = "".join(f'<rect x="{24 * i}" width="24" height="10" fill="{c}"/>' for i, c in enumerate(STRIP))
    return (
        "<style>" + font_face("Newsreader", "200 800", "normal", "newsreader-roman.woff2")
        + font_face("Newsreader", "200 800", "italic", "newsreader-italic.woff2")
        + "text{font-family:Newsreader,'Iowan Old Style','Palatino Linotype',Georgia,serif}</style>"
        '<pattern id="diamond" width="56" height="56" patternUnits="userSpaceOnUse" x="0" y="14">'
        '<path d="M0 28L28 0L56 28L28 56Z" fill="none" stroke="#EEE4D0" stroke-width="1.1" stroke-dasharray="3.5 3.5"/></pattern>'
        f'<pattern id="strip" width="168" height="10" patternUnits="userSpaceOnUse" patternTransform="scale({SCALE})">'
        f'{strip}<path d="M0 5h168" stroke="#FFFBF0" stroke-width="1" stroke-dasharray="3 2.5"/></pattern>'
    )


def logo(x, y, size):
    """The site's logo (a red bowl with a green lid knob, drawn on a 32 px grid)."""
    return (f'<g transform="translate({x} {y}) scale({size / 32})"><path d="M4 14h24a12 10 0 0 1-24 0z" fill="{RED}"/>'
            f'<rect x="13" y="9" width="6" height="3" rx="1.5" fill="{GREEN}"/></g>')


def quilt(x, y, cell):
    """One quilt block: nine stitched squares in a cream card with a stitched seam, the bowl on the middle one."""
    pad = cell * 0.3
    side = cell * 3 + pad * 2
    parts = [d.box(x, y, side, side, fill="#FFFBF0", color=INK)]
    parts.append(f'<rect x="{x + 11}" y="{y + 11}" width="{side - 22}" height="{side - 22}" rx="6" fill="none" '
                 f'stroke="#CDB98F" stroke-width="2" stroke-dasharray="8 6"/>')
    k = cell / 24
    for r in range(3):
        for c in range(3):
            px, py = x + pad + c * cell, y + pad + r * cell
            parts.append(f'<rect x="{px}" y="{py}" width="{cell}" height="{cell}" fill="{PATCH[r][c]}"/>'
                         f'<rect x="{px + 2.5 * k}" y="{py + 2.5 * k}" width="{19 * k}" height="{19 * k}" fill="none" '
                         f'stroke="#FFFBF0" stroke-width="{k}" stroke-dasharray="{2.5 * k} {2 * k}"/>')
    mid = x + pad + cell
    parts.append(logo(mid + cell * 0.1, y + pad + cell + cell * 0.1, cell * 0.8))
    return d.group(*parts)


def picture():
    headline = d.group(
        d.text(80, 218, "Put your team's", 100, INK, 700, "start"),
        d.text(80, 322, "idle machines", 100, INK, 700, "start"),
        f'<text x="80" y="426" font-size="100" font-weight="700" text-anchor="start" dominant-baseline="middle" fill="{INK}">'
        f'<tspan fill="{GREEN}" font-style="italic" font-weight="600">to work</tspan>.</text>',
    ).replace("<g>", '<g style="letter-spacing:-2px">', 1)
    body = "".join([
        f'<rect width="{W}" height="{H}" fill="{CREAM}"/>',
        f'<rect width="{W}" height="{H}" fill="url(#diamond)"/>',
        f'<rect width="{W}" height="{14}" fill="url(#strip)"/>',
        logo(80, 62, 64),
        d.text(160, 96, "Potluck", 56, INK, 700, "start"),
        headline,
        d.text(80, 524, "Self-hosted GitHub Actions runners and sandboxes", 33, SOFT, 400, "start"),
        d.text(80, 566, "for coding agents · free, Apache-2.0", 33, SOFT, 400, "start"),
        quilt(885, 160, 90),
    ])
    out = d.svg(W, H, body)
    return out.replace("<svg ", f'<svg width="{W}" height="{H}" ', 1).replace("</defs>", defs() + "</defs>", 1)


def chrome():
    named = os.environ.get("CHROME")
    mac = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
    return named or (mac if os.path.exists(mac) else "google-chrome")


def render(svg_path, png_path):
    with tempfile.TemporaryDirectory() as profile:
        subprocess.run([chrome(), "--headless=new", "--disable-gpu", f"--user-data-dir={profile}", "--hide-scrollbars",
                        f"--window-size={W},{H}", "--virtual-time-budget=5000", f"--screenshot={png_path}",
                        "file://" + os.path.abspath(svg_path)], check=True, timeout=60, stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL)


if __name__ == "__main__":
    svg_path, png_path = os.path.join(HERE, "social-preview.svg"), os.path.join(HERE, "social-preview.png")
    with open(svg_path, "w") as f:
        f.write(picture() + "\n")
    render(svg_path, png_path)
    print(f"wrote {svg_path} and {png_path} ({os.path.getsize(png_path)} bytes)")
