// `npm run readme-hero`: draw the picture at the top of the repository's README (docs/img/readme-hero.webp) from
// the home page, so the two cannot drift apart: the home page's headline and picnic, restyled by readme-hero.css.
// Run it after the home page's hero changes, and commit the picture. It needs Chrome (or Chromium; CHROME names
// the binary) and cwebp.
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { chrome, CSS, HTML, PUBLIC_DIR, run, serve, SITE_DIR } from "./chrome.mjs";

export const HERO_PATH = fileURLToPath(new URL("../../docs/img/readme-hero.webp", import.meta.url));
// The picture's size in CSS pixels; it is drawn at twice that. The card fills the window (readme-hero.css).
const WIDTH = 1100;
const HEIGHT = 460;

// The page's logo mark, as the home page's header draws it.
const BRAND = `<div class="rh-brand"><svg viewBox="0 0 32 32" aria-hidden="true"><path d="M4 14h24a12 10 0 0 1-24 0z" fill="#C8402F"/><rect x="13" y="9" width="6" height="3" rx="1.5" fill="#2F6B34"/></svg>Potluck</div>`;
const SUB = `<p class="rh-sub">Free and open source · macOS and Linux</p>`;

// heroPage turns the home page into the hero's page: the extra stylesheet, the logo above the headline and one line
// under it. Each edit names the home page text it hangs on, and a home page without that text is an error, not a
// picture with a piece missing.
export function heroPage(index) {
  const edits = [
    ['<link rel="stylesheet" href="/site.css">', (at) => `${at}<link rel="stylesheet" href="/readme-hero.css">`],
    ['<h1 id="headline">', (at) => `${BRAND}${at}`],
    ['<a class="jump" href="#install">Install</a>', () => SUB],
  ];
  let page = index;
  for (const [at, edit] of edits) {
    if (page.split(at).length !== 2) throw new Error(`readme-hero: the home page must hold ${at} exactly once`);
    page = page.replace(at, edit(at));
  }
  return page;
}

async function main() {
  const server = await serve({
    "/": [HTML, heroPage(readFileSync(join(PUBLIC_DIR, "index.html"), "utf8"))],
    "/readme-hero.css": [CSS, readFileSync(join(SITE_DIR, "build", "readme-hero.css"))],
  });
  const work = mkdtempSync(join(tmpdir(), "readme-hero-"));
  try {
    const png = join(work, "hero.png");
    // A transparent default background keeps the card's rounded corners clear, so it sits on a light or dark page.
    // The time budget lets the fonts load and the scene lay itself out before the picture is taken.
    await run("readme-hero", chrome("readme-hero"), ["--headless=new", "--disable-gpu", "--hide-scrollbars", `--user-data-dir=${join(work, "profile")}`,
      `--window-size=${WIDTH},${HEIGHT}`, "--force-device-scale-factor=2", "--default-background-color=00000000",
      "--virtual-time-budget=8000", `--screenshot=${png}`, `http://127.0.0.1:${server.address().port}/`]);
    if (!existsSync(png)) throw new Error("readme-hero: Chrome wrote no picture");
    if (server.missed.length) throw new Error(`readme-hero: the page asked for files this script does not serve: ${server.missed.join(", ")}`);
    mkdirSync(dirname(HERO_PATH), { recursive: true });
    await run("readme-hero", "cwebp", ["-quiet", "-q", "90", "-alpha_q", "100", "-m", "6", png, "-o", HERO_PATH]);
    console.log(`readme-hero: wrote ${HERO_PATH}`);
  } finally {
    server.close();
    rmSync(work, { recursive: true, force: true });
  }
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  main().catch((e) => {
    console.error(e.message);
    process.exit(1);
  });
}
