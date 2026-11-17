// `npm run og-assets`: draw, once and by hand, what the build needs to make share images and icons without a
// dependency: the blank share-image template (build/og/template.png: the quilt, the logo, no words), the glyphs of
// the site's font at the sizes the build sets titles in (build/og/glyphs.json: each glyph's coverage, advance and
// the kerning between pairs), and the icons (public/favicon.ico with 16, 32 and 48 px pictures, apple-touch-icon.png
// at 180 px, icon-192.png and icon-512.png for site.webmanifest). Run it after the font, the logo or the quilt
// changes, and commit what it writes. It needs Chrome (CHROME names the binary). The build itself
// (build/share.mjs) runs on Node's standard library alone.
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { deflateSync } from "node:zlib";
import { fileURLToPath } from "node:url";
import { chrome, HTML, PUBLIC_DIR, run, serve, SITE_DIR } from "./chrome.mjs";
import { CHARS, FACES } from "./share.mjs";

const OG_DIR = join(SITE_DIR, "build", "og");

// The logo, as the page's header draws it.
const LOGO_SVG = `<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'><path d='M4 14h24a12 10 0 0 1-24 0z' fill='#C8402F'/><rect x='13' y='9' width='6' height='3' rx='1.5' fill='#2F6B34'/></svg>`;
// The page's quilt (site.css): the patchwork strip along the top and the diamond stitching on cream.
const STRIP_SVG = `<svg xmlns='http://www.w3.org/2000/svg' width='168' height='10'><rect x='0' width='24' height='10' fill='#E9A99B'/><rect x='24' width='24' height='10' fill='#F0CB74'/><rect x='48' width='24' height='10' fill='#A9C98A'/><rect x='72' width='24' height='10' fill='#9FC2DA'/><rect x='96' width='24' height='10' fill='#F6E7C8'/><rect x='120' width='24' height='10' fill='#E9A99B'/><rect x='144' width='24' height='10' fill='#A9C98A'/><path d='M0 5h168' stroke='#FFFBF0' stroke-width='1' stroke-dasharray='3 2.5'/></svg>`;
const DIAMOND_SVG = `<svg xmlns='http://www.w3.org/2000/svg' width='56' height='56'><path d='M0 28L28 0L56 28L28 56Z' fill='none' stroke='#EEE4D0' stroke-width='1.1' stroke-dasharray='3.5 3.5'/></svg>`;

// The page Chrome runs: it draws everything on canvases and writes the result into the DOM as JSON, which
// --dump-dom prints once the time budget has let the fonts load.
function page() {
  return `<!doctype html><meta charset="utf-8">
<style>
@font-face{font-family:Newsreader;font-style:normal;font-weight:200 800;src:url(/fonts/newsreader-roman.woff2) format("woff2")}
</style>
<pre id="out">pending</pre>
<script>
const FACES = ${JSON.stringify(FACES)};
const CHARS = ${JSON.stringify(CHARS)};
const svg = (s) => new Promise((ok, bad) => { const i = new Image(); i.onload = () => ok(i); i.onerror = bad; i.src = "data:image/svg+xml," + encodeURIComponent(s); });
const canvas = (w, h) => { const c = document.createElement("canvas"); c.width = w; c.height = h; return [c, c.getContext("2d")]; };
async function main() {
  await Promise.all(FACES.map((f) => document.fonts.load(f.weight + " " + f.size + "px Newsreader", CHARS)));
  const logo = await svg(${JSON.stringify(LOGO_SVG)});
  const out = { faces: {}, icons: {} };

  // The template: the quilt, and the logo with the name, where the home page's share image had them.
  const [t, g] = canvas(1200, 630);
  g.fillStyle = "#FBF7EE"; g.fillRect(0, 0, 1200, 630);
  g.save(); g.translate(0, 10); g.fillStyle = g.createPattern(await svg(${JSON.stringify(DIAMOND_SVG)}), "repeat"); g.fillRect(0, -10, 1200, 630); g.restore();
  g.fillStyle = g.createPattern(await svg(${JSON.stringify(STRIP_SVG)}), "repeat-x"); g.fillRect(0, 0, 1200, 10);
  g.drawImage(logo, 80, 74, 64, 64);
  g.fillStyle = "#1D261D"; g.font = "700 52px Newsreader"; g.textBaseline = "alphabetic"; g.fillText("Potluck", 160, 124);
  out.template = t.toDataURL("image/png");

  // Each face: every glyph's coverage (alpha) cropped to its ink, its advance, and the kerning of every pair.
  for (const f of FACES) {
    const font = f.weight + " " + f.size + "px Newsreader";
    const cell = Math.ceil(f.size * 1.6), base = Math.ceil(f.size * 1.15);
    const [c, x] = canvas(cell, cell);
    x.font = font; x.fontKerning = "normal";
    const glyphs = {};
    for (const ch of CHARS) {
      x.clearRect(0, 0, cell, cell); x.fillStyle = "#000"; x.fillText(ch, Math.ceil(f.size * 0.2), base);
      const d = x.getImageData(0, 0, cell, cell).data;
      let x0 = cell, y0 = cell, x1 = -1, y1 = -1;
      for (let y = 0; y < cell; y++) for (let i = 0; i < cell; i++) if (d[(y * cell + i) * 4 + 3]) { x0 = Math.min(x0, i); x1 = Math.max(x1, i); y0 = Math.min(y0, y); y1 = Math.max(y1, y); }
      const w = x1 < 0 ? 0 : x1 - x0 + 1, h = x1 < 0 ? 0 : y1 - y0 + 1;
      const a = [];
      for (let y = 0; y < h; y++) for (let i = 0; i < w; i++) a.push(d[((y0 + y) * cell + x0 + i) * 4 + 3]);
      glyphs[ch] = { adv: x.measureText(ch).width, dx: w ? x0 - Math.ceil(f.size * 0.2) : 0, dy: h ? y0 - base : 0, w, h, a: btoa(String.fromCharCode(...a)) };
    }
    const kern = {};
    for (const a of CHARS) for (const b of CHARS) {
      const k = x.measureText(a + b).width - glyphs[a].adv - glyphs[b].adv;
      if (Math.abs(k) >= 0.25) kern[a + b] = Math.round(k * 100) / 100;
    }
    out.faces[f.name] = { glyphs, kern };
  }

  // The icons: the logo alone, clear around it at small sizes, on cream with a margin where a platform crops.
  for (const [name, size, pad, bg] of [["16", 16, 0, ""], ["32", 32, 0, ""], ["48", 48, 0, ""], ["apple-touch-icon", 180, 22, "#FBF7EE"], ["icon-192", 192, 24, "#FBF7EE"], ["icon-512", 512, 64, "#FBF7EE"]]) {
    const [c, x] = canvas(size, size);
    if (bg) { x.fillStyle = bg; x.fillRect(0, 0, size, size); }
    x.drawImage(logo, pad, pad, size - 2 * pad, size - 2 * pad);
    out.icons[name] = c.toDataURL("image/png");
  }
  document.getElementById("out").textContent = JSON.stringify(out);
}
main().catch((e) => { document.getElementById("out").textContent = "error: " + e; });
</script>`;
}

const png = (dataUrl) => Buffer.from(dataUrl.slice(dataUrl.indexOf(",") + 1), "base64");

/** A .ico holding PNG pictures (Windows Vista and later, and every browser, read PNG entries). */
export function ico(pictures) {
  const head = Buffer.alloc(6 + 16 * pictures.length);
  head.writeUInt16LE(0, 0);
  head.writeUInt16LE(1, 2);
  head.writeUInt16LE(pictures.length, 4);
  let offset = head.length;
  pictures.forEach(({ size, data }, i) => {
    const e = 6 + 16 * i;
    head.writeUInt8(size >= 256 ? 0 : size, e);
    head.writeUInt8(size >= 256 ? 0 : size, e + 1);
    head.writeUInt16LE(1, e + 4); // colour planes
    head.writeUInt16LE(32, e + 6); // bits per pixel
    head.writeUInt32LE(data.length, e + 8);
    head.writeUInt32LE(offset, e + 12);
    offset += data.length;
  });
  return Buffer.concat([head, ...pictures.map((p) => p.data)]);
}

async function main() {
  const server = await serve({ "/": [HTML, page()] });
  const work = mkdtempSync(join(tmpdir(), "og-assets-"));
  try {
    const dom = await run("og-assets", chrome("og-assets"), ["--headless=new", "--disable-gpu", `--user-data-dir=${join(work, "profile")}`,
      "--virtual-time-budget=10000", "--dump-dom", `http://127.0.0.1:${server.address().port}/`]);
    const missed = server.missed.filter((p) => p !== "/favicon.ico"); // Chrome asks for one on its own, before og-assets has drawn it
    const text = dom.match(/<pre id="out">([\s\S]*?)<\/pre>/)?.[1].replace(/&quot;/g, '"').replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&");
    if (!text || !text.startsWith("{")) throw new Error(`og-assets: the page did not finish: ${text?.slice(0, 200)}`);
    if (missed.length) throw new Error(`og-assets: the page asked for files this script does not serve: ${missed.join(", ")}`);
    const out = JSON.parse(text);
    writeFileSync(join(OG_DIR, "template.png"), png(out.template));
    // Each glyph's coverage is kept deflated, so the file stays small enough to read in a diff's summary.
    for (const face of Object.values(out.faces)) for (const g of Object.values(face.glyphs)) g.a = deflateSync(Buffer.from(g.a, "base64")).toString("base64");
    writeFileSync(join(OG_DIR, "glyphs.json"), `${JSON.stringify(out.faces)}\n`);
    writeFileSync(join(PUBLIC_DIR, "favicon.ico"), ico(["16", "32", "48"].map((s) => ({ size: Number(s), data: png(out.icons[s]) }))));
    for (const name of ["apple-touch-icon", "icon-192", "icon-512"]) writeFileSync(join(PUBLIC_DIR, `${name}.png`), png(out.icons[name]));
    console.log("og-assets: wrote build/og/template.png, build/og/glyphs.json, public/favicon.ico, apple-touch-icon.png, icon-192.png, icon-512.png");
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
