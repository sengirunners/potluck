// Share images, one per page, drawn by the build on Node's standard library alone: the page's title set in the
// site's font on the blank template (build/og/template.png), with the page's address under it. The template and the
// font's glyphs are drawn once by hand with `npm run og-assets` (build/og-assets.mjs, which needs Chrome) and committed.
import { readFileSync } from "node:fs";
import { inflateSync, deflateSync } from "node:zlib";

/** The faces the build sets text in; og-assets draws a glyph set for each. */
export const FACES = [
  { name: "title-84", weight: 700, size: 84 },
  { name: "title-70", weight: 700, size: 70 },
  { name: "title-58", weight: 700, size: 58 },
  { name: "foot", weight: 400, size: 30 },
];
/** The characters a title or address may use. Another one fails the build: add it here and run `npm run og-assets`. */
export const CHARS = [...Array.from({ length: 95 }, (_, i) => String.fromCharCode(32 + i)), ..."’‘“”—–·é…"].join("");

const WIDTH = 1200;
const HEIGHT = 630;
const LEFT = 80;
const MEASURE = 1040; // the widest a line of the title may be
const INK = [0x1d, 0x26, 0x1d];
const SOFT = [0x4b, 0x56, 0x48];
// The title's sizes, largest first, with the most lines each may take; the first that fits is used.
const FITS = [["title-84", 3], ["title-70", 4], ["title-58", 5]];

// --- PNG, the subset needed: 8-bit RGB or RGBA, not interlaced, in; 8-bit RGB out.

const CRC_TABLE = Array.from({ length: 256 }, (_, n) => {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  return c >>> 0;
});
const crc32 = (buf) => {
  let c = 0xffffffff;
  for (const b of buf) c = CRC_TABLE[(c ^ b) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
};
const SIGNATURE = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);

/** Decodes a PNG to {width, height, rgb} (alpha, if any, dropped: the template is opaque). */
export function decodePng(buf) {
  if (!buf.subarray(0, 8).equals(SIGNATURE)) throw new Error("not a PNG");
  let width, height, channels;
  const idat = [];
  for (let at = 8; at < buf.length; ) {
    const len = buf.readUInt32BE(at);
    const type = buf.toString("latin1", at + 4, at + 8);
    const data = buf.subarray(at + 8, at + 8 + len);
    if (type === "IHDR") {
      width = data.readUInt32BE(0);
      height = data.readUInt32BE(4);
      const [depth, colour, , , interlace] = data.subarray(8, 13);
      if (depth !== 8 || interlace !== 0 || (colour !== 2 && colour !== 6)) throw new Error(`PNG: only 8-bit RGB or RGBA, not interlaced (got depth ${depth}, colour ${colour}, interlace ${interlace})`);
      channels = colour === 6 ? 4 : 3;
    } else if (type === "IDAT") idat.push(data);
    else if (type === "IEND") break;
    at += 12 + len;
  }
  const raw = inflateSync(Buffer.concat(idat));
  const stride = width * channels;
  const px = Buffer.alloc(stride * height);
  for (let y = 0; y < height; y++) {
    const filter = raw[y * (stride + 1)];
    const src = raw.subarray(y * (stride + 1) + 1, (y + 1) * (stride + 1));
    for (let i = 0; i < stride; i++) {
      const a = i >= channels ? px[y * stride + i - channels] : 0;
      const b = y ? px[(y - 1) * stride + i] : 0;
      const c = y && i >= channels ? px[(y - 1) * stride + i - channels] : 0;
      let v;
      if (filter === 0) v = 0;
      else if (filter === 1) v = a;
      else if (filter === 2) v = b;
      else if (filter === 3) v = (a + b) >> 1;
      else if (filter === 4) {
        const p = a + b - c, pa = Math.abs(p - a), pb = Math.abs(p - b), pc = Math.abs(p - c);
        v = pa <= pb && pa <= pc ? a : pb <= pc ? b : c;
      } else throw new Error(`PNG: unknown filter ${filter}`);
      px[y * stride + i] = (src[i] + v) & 0xff;
    }
  }
  if (channels === 3) return { width, height, rgb: px };
  const rgb = Buffer.alloc(width * height * 3);
  for (let i = 0, j = 0; i < px.length; i += 4, j += 3) px.copy(rgb, j, i, i + 3);
  return { width, height, rgb };
}

/** Encodes {width, height, rgb} as an 8-bit RGB PNG, each row filtered with Sub. */
export function encodePng({ width, height, rgb }) {
  const stride = width * 3;
  const raw = Buffer.alloc((stride + 1) * height);
  for (let y = 0; y < height; y++) {
    const o = y * (stride + 1);
    raw[o] = 1;
    for (let i = 0; i < stride; i++) raw[o + 1 + i] = (rgb[y * stride + i] - (i >= 3 ? rgb[y * stride + i - 3] : 0)) & 0xff;
  }
  const chunk = (type, data) => {
    const head = Buffer.alloc(8);
    head.writeUInt32BE(data.length, 0);
    head.write(type, 4, "latin1");
    const crc = Buffer.alloc(4);
    crc.writeUInt32BE(crc32(Buffer.concat([head.subarray(4), data])), 0);
    return Buffer.concat([head, data, crc]);
  };
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr.set([8, 2, 0, 0, 0], 8);
  return Buffer.concat([SIGNATURE, chunk("IHDR", ihdr), chunk("IDAT", deflateSync(raw, { level: 9 })), chunk("IEND", Buffer.alloc(0))]);
}

/** True when two PNGs hold the same pixels (compressors differ between zlib versions; the picture is what counts). */
export function samePixels(a, b) {
  const x = decodePng(a);
  const y = decodePng(b);
  return x.width === y.width && x.height === y.height && x.rgb.equals(y.rgb);
}

// --- Text

let assets;
function load() {
  if (!assets) {
    const faces = JSON.parse(readFileSync(new URL("og/glyphs.json", import.meta.url), "utf8"));
    for (const face of Object.values(faces)) for (const g of Object.values(face.glyphs)) g.a = inflateSync(Buffer.from(g.a, "base64"));
    assets = { faces, template: decodePng(readFileSync(new URL("og/template.png", import.meta.url))) };
  }
  return assets;
}

function face(name) {
  const f = load().faces[name];
  if (!f) throw new Error(`share image: no glyphs for ${name} in build/og/glyphs.json; run \`npm run og-assets\``);
  return f;
}

function checkChars(text, where) {
  for (const ch of text) if (!CHARS.includes(ch)) throw new Error(`${where}: the share image has no glyph for ${JSON.stringify(ch)}; add it to CHARS in build/share.mjs and run \`npm run og-assets\``);
}

/** How wide `text` sets in face `f`, kerning included. */
function measure(f, text) {
  let w = 0;
  for (let i = 0; i < text.length; i++) w += f.glyphs[text[i]].adv + (i ? f.kern[text[i - 1] + text[i]] ?? 0 : 0);
  return w;
}

/** Breaks `text` into lines no wider than `max`, at spaces; null when a single word is wider. */
function wrap(f, text, max) {
  const lines = [];
  let line = "";
  for (const word of text.split(/\s+/)) {
    const next = line ? `${line} ${word}` : word;
    if (measure(f, next) <= max) line = next;
    else {
      if (!line || measure(f, word) > max) return null;
      lines.push(line);
      line = word;
    }
  }
  if (line) lines.push(line);
  return lines;
}

function draw(img, f, text, x, baseline, colour) {
  let pen = x;
  for (let i = 0; i < text.length; i++) {
    const g = f.glyphs[text[i]];
    if (i) pen += f.kern[text[i - 1] + text[i]] ?? 0;
    const gx = Math.round(pen + g.dx);
    const gy = Math.round(baseline + g.dy);
    for (let y = 0; y < g.h; y++) {
      const py = gy + y;
      if (py < 0 || py >= img.height) continue;
      for (let c = 0; c < g.w; c++) {
        const px = gx + c;
        const a = g.a[y * g.w + c];
        if (!a || px < 0 || px >= img.width) continue;
        const o = (py * img.width + px) * 3;
        for (let k = 0; k < 3; k++) img.rgb[o + k] = Math.round(img.rgb[o + k] + ((colour[k] - img.rgb[o + k]) * a) / 255);
      }
    }
    pen += g.adv;
  }
}

/**
 * The share image for one page, as PNG bytes: `title` on the template at the largest size it fits, and `address`
 * (the page's address without https://) under it. `where` names the source in errors.
 */
export function shareImage({ title, address, where }) {
  checkChars(title, where);
  checkChars(address, where);
  const t = load().template;
  if (t.width !== WIDTH || t.height !== HEIGHT) throw new Error(`build/og/template.png must be ${WIDTH}x${HEIGHT}`);
  const img = { width: WIDTH, height: HEIGHT, rgb: Buffer.from(t.rgb) };
  const fit = FITS.map(([name, most]) => [face(name), wrap(face(name), title.trim(), MEASURE), most, Number(name.split("-")[1])]).find(([, lines, most]) => lines && lines.length <= most);
  if (!fit) throw new Error(`${where}: the title is too long for a share image (five lines at the smallest size); shorten it`);
  const [f, lines, , size] = fit;
  const lead = Math.round(size * 1.08);
  lines.forEach((line, i) => draw(img, f, line, LEFT, 170 + Math.round(size * 0.9) + i * lead, INK));
  const foot = face("foot");
  // Named as Potluck's, so on a page about other software (Tart) it is never read as that software's licence.
  const line = `${address} · Potluck is free and open source (Apache-2.0)`;
  draw(img, foot, measure(foot, line) <= MEASURE ? line : address, LEFT, 560, SOFT);
  return encodePng(img);
}
