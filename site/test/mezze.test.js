// mezze.sh: the second site (build/mezze.mjs, mezze/, src/mezze.js) and potluck.sh's side of the move.
import assert from "node:assert/strict";
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";
import { drift, HOME, MEZZE_DIR, ORIGIN, PUBLIC_DIR, SHARE_SIZE } from "../build/mezze.mjs";
import { SITE_DIR } from "../build/build.mjs";
import { BOTS } from "../build/output.mjs";
import { TOPICS } from "../src/contact.js";
import { MEZZE_URL } from "../src/mezze-redirect.js";
import mezze from "../src/mezze.js";
import potluck from "../src/worker.js";
import { fakeKV } from "./fakekv.js";

const read = (rel) => readFileSync(join(PUBLIC_DIR, rel), "utf8");
const HTML = read("index.html");
const graphOf = (html) => JSON.parse(html.match(/<script type="application\/ld\+json">(.*?)<\/script>/)[1])["@graph"];
const attr = (html, name) => [...html.matchAll(new RegExp(`\\s${name}="([^"]*)"`, "g"))].map((m) => m[1].replace(/&amp;/g, "&"));
const pngSize = (buf) => [buf.readUInt32BE(16), buf.readUInt32BE(20)];
const context = () => {
  const pending = [];
  return { waitUntil: (p) => pending.push(p), settled: () => Promise.all(pending) };
};

test("mezze.sh's committed output matches a fresh build (run `npm run build`)", () => {
  assert.deepEqual(drift(), []);
});

test("the home page names mezze.sh as its canonical address, with Open Graph, the picked share image and JSON-LD", () => {
  assert.ok(HTML.includes(`<link rel="canonical" href="${ORIGIN}/">`));
  assert.ok(HTML.includes(`<meta property="og:url" content="${ORIGIN}/">`));
  assert.ok(HTML.includes(`<meta property="og:site_name" content="Mezze">`));
  assert.ok(HTML.includes(`<meta property="og:image" content="${ORIGIN}/og/home.png">`));
  assert.ok(HTML.includes(`<meta name="twitter:image" content="${ORIGIN}/og/home.png">`));
  assert.ok(HTML.includes(`<meta property="og:image:width" content="${SHARE_SIZE[0]}">`));
  assert.ok(HTML.includes(`<meta property="og:image:height" content="${SHARE_SIZE[1]}">`));
  assert.deepEqual(pngSize(readFileSync(join(PUBLIC_DIR, "og/home.png"))), SHARE_SIZE);
  assert.ok(!HTML.includes("potluck.sh/og/"), "no share image from the other site");
  const graph = graphOf(HTML);
  const app = graph.find((n) => [].concat(n["@type"]).includes("SoftwareApplication"));
  assert.equal(app.name, "Mezze");
  assert.equal(app.url, `${ORIGIN}/`);
  assert.equal(app.license, "https://www.apache.org/licenses/LICENSE-2.0");
  assert.equal(app.codeRepository, undefined, "the source is not public yet");
  assert.equal(app.softwareVersion, undefined);
  assert.ok(graph.some((n) => n["@type"] === "Organization"));
  assert.ok(graph.some((n) => n["@type"] === "WebSite" && n.url === `${ORIGIN}/`));
  assert.ok(HTML.includes(`<h1 id="hero-h">${HOME.headline.replace("coding agents ", "coding agents <em>").replace(/\.$/, ".</em>")}</h1>`), "the h1 is HOME.headline");
});

test("robots.txt, the sitemap and the llms files are mezze.sh's own", () => {
  const robots = read("robots.txt");
  for (const bot of BOTS) assert.match(robots, new RegExp(`User-agent: ${bot}\\nAllow: /\\nDisallow: /api/`));
  assert.match(robots, new RegExp(`Sitemap: ${ORIGIN}/sitemap.xml\\n$`));
  const locs = [...read("sitemap.xml").matchAll(/<loc>([^<]+)<\/loc>/g)].map((m) => m[1]);
  assert.deepEqual(locs, [`${ORIGIN}/`]);
  for (const f of ["llms.txt", "llms-full.txt"]) {
    const t = read(f);
    assert.match(t, /^# Mezze\n/);
    assert.match(t, /source is not public yet/);
    assert.match(t, /Apache-2\.0/);
    assert.match(t, /https:\/\/potluck\.sh/);
  }
  assert.match(read("llms-full.txt"), /## The quick start/);
});

test("the page says what is true today: source not public, no date, names Mezze and Potluck only, and links to Potluck", () => {
  const text = [HTML, readFileSync(join(MEZZE_DIR, "content/home.md"), "utf8")].join("\n");
  assert.match(HTML, /Its source is not public yet/);
  assert.match(HTML, /Mezze's licence is Apache-2\.0/);
  assert.match(HTML, /Mezze, the agent and team layer on <a href="https:\/\/potluck\.sh\/">Potluck<\/a>/);
  for (const bad of [/Picnic/i, /Potluck Mezze/, /Potluck Dish/, /\bv0\b/, /prototype/i, /coming (soon|in)/i, /\b20\d\d-\d\d-\d\d\b.*public/]) assert.doesNotMatch(text, bad);
  // Commands the page shows must be the ones that act: `zm install` without --yes is a dry run.
  assert.doesNotMatch(HTML, /zm install (claude|codex)<\/code>/);
});

test("every internal link and asset on the page resolves to a file or an anchor", () => {
  const ids = new Set(attr(HTML, "id"));
  for (const ref of [...attr(HTML, "href"), ...attr(HTML, "src")]) {
    if (/^(https?:|data:|mailto:)/.test(ref)) continue;
    if (ref.startsWith("#")) {
      assert.ok(ids.has(ref.slice(1)), `${ref}: no such anchor`);
      continue;
    }
    const path = ref.split("#")[0];
    const file = path === "/" ? "index.html" : path.slice(1);
    assert.ok(existsSync(join(PUBLIC_DIR, file)), `${ref}: no file mezze/public/${file}`);
  }
});

test("the icons exist, are drawn at their sizes, and the page names them", () => {
  const ico = readFileSync(join(PUBLIC_DIR, "favicon.ico"));
  assert.deepEqual([ico.readUInt16LE(2), ico.readUInt16LE(4)], [1, 3]);
  assert.deepEqual(pngSize(readFileSync(join(PUBLIC_DIR, "apple-touch-icon.png"))), [180, 180]);
  const manifest = JSON.parse(read("site.webmanifest"));
  assert.equal(manifest.name, "Mezze");
  for (const icon of manifest.icons) assert.equal(pngSize(readFileSync(join(PUBLIC_DIR, icon.src.slice(1)))).join("x"), icon.sizes);
  for (const link of ['rel="icon" href="/favicon.ico"', 'rel="apple-touch-icon" href="/apple-touch-icon.png"', 'rel="manifest" href="/site.webmanifest"']) assert.ok(HTML.includes(link), link);
  assert.notDeepEqual(readFileSync(join(PUBLIC_DIR, "favicon.ico")), readFileSync(join(SITE_DIR, "public/favicon.ico")), "the olive, not Potluck's bowl");
  assert.equal(read("7b1a88c94e3d01e8f2150746ddb8a872.txt").trim(), "7b1a88c94e3d01e8f2150746ddb8a872");
});

test("the form is the founders form: the server's topics and fields, posted to this site, Mezze picked", () => {
  const form = HTML.slice(HTML.indexOf('<form class="talk"'), HTML.indexOf("</form>"));
  assert.match(form, /action="\/api\/contact"/);
  const offered = Object.fromEntries([...form.matchAll(/<option value="([^"]+)"[^>]*>([^<]+)</g)].map((m) => [m[1], m[2]]));
  assert.deepEqual(offered, TOPICS);
  assert.match(form, /<option value="dish" selected>/);
  assert.deepEqual([...form.matchAll(/name="([^"]+)"/g)].map((m) => m[1]).sort(), ["email", "leave_empty", "message", "name", "team", "topic"]);
  assert.ok(existsSync(join(PUBLIC_DIR, "thanks.html")));
});

test("mezze.sh's Worker keeps a form post as a mezze.sh message, refuses other sites, and serves the rest as assets", async () => {
  const seen = [];
  const e = { CONTACT: fakeKV(), ASSETS: { fetch: async (req) => (seen.push(new URL(req.url).pathname), new Response("asset")) } };
  const body = new URLSearchParams({ name: "Ada", email: "ada@example.com", topic: "dish", team: "", message: "Twelve agents." }).toString();
  const post = (origin) => new Request("https://mezze.sh/api/contact", { method: "POST", body,
    headers: { "content-type": "application/x-www-form-urlencoded", "content-length": String(body.length), accept: "application/json", origin } });
  assert.equal((await mezze.fetch(post("https://potluck.sh"), e, context())).status, 403);
  const res = await mezze.fetch(post("https://mezze.sh"), e, context());
  assert.equal(res.status, 200);
  const kept = [...e.CONTACT.store.entries()].filter(([k]) => k.startsWith("msg:")).map(([, v]) => JSON.parse(v.value));
  assert.equal(kept.length, 1);
  assert.equal(kept[0].site, "mezze.sh");
  assert.equal((await mezze.fetch(new Request("https://mezze.sh/api/waitlist"), e, context())).status, 404);
  assert.equal(await (await mezze.fetch(new Request("https://mezze.sh/"), e, context())).text(), "asset");
  assert.deepEqual(seen, ["/"]);
  assert.deepEqual(Object.keys(await import("../src/mezze.js")), ["default"]);
});

test("a mezze.sh message's email names mezze.sh", async () => {
  const { notice } = await import("../src/notify.js");
  const m = notice({ name: "Ada", email: "ada@example.com", topic: "dish", team: "", message: "hi", at: "2026-11-17T00:00:00Z", site: "mezze.sh" }, TOPICS);
  assert.equal(m.subject, "mezze.sh: Ada (Try Mezze)");
  assert.match(m.text, /contact form on mezze\.sh/);
});

test("potluck.sh/mezze moves to mezze.sh for good, the Worker sees that path first, and the home card links there", async () => {
  assert.equal(MEZZE_URL, `${ORIGIN}/`);
  const e = { ASSETS: { fetch: async () => new Response("asset") } };
  for (const path of ["/mezze", "/mezze/", "/mezze.html", "/mezze.html/"]) {
    const res = await potluck.fetch(new Request(`https://potluck.sh${path}`), e, context());
    assert.equal(res.status, 301, path);
    assert.equal(res.headers.get("location"), MEZZE_URL);
  }
  assert.equal(await (await potluck.fetch(new Request("https://potluck.sh/mezzanine"), e, context())).text(), "asset");
  const wrangler = readFileSync(join(SITE_DIR, "wrangler.jsonc"), "utf8");
  assert.match(wrangler, /"run_worker_first": \[[^\]]*"\/mezze", "\/mezze\/", "\/mezze.html", "\/mezze.html\/"/);
  assert.ok(!existsSync(join(SITE_DIR, "public/mezze.html")), "no page left at /mezze to shadow the redirect");
  assert.ok(!readFileSync(join(SITE_DIR, "public/sitemap.xml"), "utf8").includes("potluck.sh/mezze<"));
  assert.match(readFileSync(join(SITE_DIR, "public/index.html"), "utf8"), /<a href="https:\/\/mezze\.sh\/">What Mezze does<\/a>/);
});

test("mezze.sh's wrangler config serves mezze/public on mezze.sh and www, with the shared form's bindings", () => {
  const w = readFileSync(join(MEZZE_DIR, "wrangler.jsonc"), "utf8");
  assert.match(w, /"name": "mezze-site"/);
  assert.match(w, /"main": "..\/src\/mezze.js"/);
  assert.match(w, /"directory": ".\/public"/);
  assert.match(w, /"pattern": "mezze.sh", "custom_domain": true/);
  assert.match(w, /"pattern": "www.mezze.sh", "custom_domain": true/);
  const potluckW = readFileSync(join(SITE_DIR, "wrangler.jsonc"), "utf8");
  const contactId = (s) => s.match(/"binding": "CONTACT", "id": "([0-9a-f]+)"/)[1];
  assert.equal(contactId(w), contactId(potluckW), "one store for the founders form");
});

test("a form post that cannot be kept sends the reader back to the form on the site it came from", async () => {
  const body = "name=Ada&email=not-an-address&topic=dish&message=hi";
  const post = (host) => new Request(`https://${host}/api/contact`, { method: "POST", body,
    headers: { "content-type": "application/x-www-form-urlencoded", "content-length": String(body.length) } });
  const e = { CONTACT: fakeKV() };
  const back = async (worker, host) => (await (await worker.fetch(post(host), e, context())).text()).match(/<a href="([^"]+)">Back to the form/)[1];
  assert.equal(await back(mezze, "mezze.sh"), "/#talk");
  assert.equal(await back(mezze, "www.mezze.sh"), "/#talk");
  assert.equal(await back(potluck, "potluck.sh"), "/#founders");
  assert.match(HTML, /id="talk"/);
});

test("no page in content/pages/ can take /mezze, the redirect's path", async () => {
  const { RESERVED_SLUGS } = await import("../build/pages.mjs");
  assert.ok(RESERVED_SLUGS.includes("mezze"));
});

// The live robot (mezze/public/robot.js, drawing mezze/public/3d/robot.bin from build/mezze-robot.py).
test("the robot's model file parses, lines up with the picture, and has every plate the labels name", async () => {
  const { parse } = await import("../mezze/public/robot.js");
  const buf = readFileSync(join(PUBLIC_DIR, "3d/robot.bin"));
  const model = parse(buf.buffer.slice(buf.byteOffset, buf.byteOffset + buf.length));
  // The canvas keeps the picture's aspect, so the fallback image and the first frame line up.
  const [w, h] = model.size;
  assert.equal(w / h, 880 / 960);
  assert.match(HTML, /<img src="\/img\/robot.webp" width="880" height="960"/);
  const names = model.parts.map((p) => p.name);
  for (const want of ["root", "kick", "body", "arm_l", "arm_r", "head", "ground"]) assert.ok(names.includes(want), want);
  const plates = model.parts.filter((p) => p.plate).map((p) => p.plate.label).sort();
  assert.deepEqual(plates, ["Ana's agents", "Leo's agents", "events", "mail", "sessions", "work items"]);
  for (const p of model.parts) {
    if (p.parent) assert.ok(names.indexOf(p.parent) < names.indexOf(p.name), `${p.name}: its parent comes first`);
    for (const i of p.idx) assert.ok(i < p.verts, `${p.name}: index in range`);
  }
  // Every label is tied to a part of the model, and the picture's seven labels are there.
  const tagged = [...HTML.matchAll(/<li data-part="([^"]+)"[^>]*><i class="d\d"><\/i>([^<]+)<\/li>/g)];
  assert.deepEqual(tagged.map((m) => m[2]).sort(), ["Ana's agents", "Leo's agents", "events", "mail", "sessions", "work items", "your agent"]);
  for (const [, part] of tagged) assert.ok(names.includes(part), part);
  assert.ok(buf.length < 256 * 1024, `the model is ${buf.length} bytes`);
});

test("no push, however hard, takes a joint past its limit or a plate off its stick", async () => {
  const { JOINTS, SLIDE, joint, slide, step } = await import("../mezze/public/robot.js");
  for (const kind of Object.keys(JOINTS)) {
    const J = joint(kind);
    let worst = 0;
    for (let n = 0; n < 4000; n++) {
      if (n % 7 === 0) J.v = J.v.map(() => (Math.random() - 0.5) * 400);
      step(J, 1 / 180);
      worst = Math.max(worst, ...J.a.map(Math.abs));
    }
    assert.ok(worst <= JOINTS[kind].max + 1e-9, `${kind}: ${worst} > ${JOINTS[kind].max}`);
  }
  // A plate's slide, shoved hard while tilted to its limit both ways, never goes past SLIDE.
  const P = joint("plate");
  let far = 0;
  for (let n = 0; n < 20000; n++) {
    if (n % 5 === 0) { P.sv = P.sv.map(() => (Math.random() - 0.5) * 1e4); P.v = P.v.map(() => (Math.random() - 0.5) * 400); }
    step(P, 1 / 180);
    slide(P, 1 / 180);
    far = Math.max(far, Math.hypot(...P.s));
  }
  assert.ok(far <= SLIDE + 1e-12, `slide ${far} > ${SLIDE}`);
  assert.ok(SLIDE < 0.2, "a plate's centre stays well inside its rim (radius about 0.6) over the stick");
});

test("a joint or slide that goes non-finite starts again from rest instead of escaping its limits", async () => {
  const { joint, slide, step } = await import("../mezze/public/robot.js");
  const J = joint("plate");
  J.a = [NaN, 0, 0]; J.v = [Infinity, 0, 0]; J.s = [NaN, 0]; J.sv = [0, -Infinity];
  step(J, 1 / 60);
  slide(J, 1 / 60);
  for (const x of [...J.a, ...J.v, ...J.s, ...J.sv]) assert.ok(Number.isFinite(x));
});

test("the robot loads after the page only when motion is welcome, and the picture keeps its place and its words", () => {
  const loader = HTML.slice(HTML.lastIndexOf("<script"), HTML.indexOf("</body>"));
  assert.match(loader, /prefers-reduced-motion: reduce/);
  assert.match(loader, /import\("\/robot.js"\)/);
  assert.ok(HTML.lastIndexOf("<script") > HTML.indexOf("</footer>"), "after the content");
  const alt = HTML.match(/<img src="\/img\/robot.webp"[^>]* alt="([^"]+)"/)[1];
  for (const label of ["your agent", "sessions", "events", "Leo's agents", "work items", "mail", "Ana's agents"]) assert.ok(alt.includes(label), label);
  assert.doesNotMatch(HTML, /<figure class="stage"[^>]*title=/, "no promise to touch before the robot is live");
  assert.doesNotMatch(HTML, /<canvas/, "the canvas is made by the script, never in the page");
  const css = readFileSync(join(PUBLIC_DIR, "site.css"), "utf8");
  assert.match(css, /\.stage canvas\{position:absolute;/, "the canvas never takes space of its own");
});

// The scenes below the robot (index.html .scene, scenes.js, img/props/ from build/mezze-props.mjs, plates.js).
// A WebP's canvas size, from its VP8X header (every prop has transparency, so every prop has one).
const webpSize = (buf) => {
  assert.equal(buf.toString("ascii", 12, 16), "VP8X");
  return [1 + buf.readUIntLE(24, 3), 1 + buf.readUIntLE(27, 3)];
};

test("img/props/ is exactly what the props tool draws, each at its size, and every plate's sheet holds TURNS turns", async () => {
  const { JOBS } = await import("../build/mezze-props.mjs");
  const { COLS, TURNS } = await import("../mezze/public/plates.js");
  const names = JOBS.map((j) => `${j.out || j.prop}.webp`).sort();
  assert.deepEqual(readdirSync(join(PUBLIC_DIR, "img/props")).sort(), names);
  for (const j of JOBS) {
    const scale = j.scale || 2, w = Math.round(j.w * scale), h = Math.round(j.h * scale);
    const want = j.turns ? [w * COLS, h * Math.ceil(TURNS / COLS)] : [w, h];
    assert.deepEqual(webpSize(readFileSync(join(PUBLIC_DIR, `img/props/${j.out || j.prop}.webp`))), want, j.out || j.prop);
    if (j.turns) assert.equal(j.turns, TURNS, `${j.out}: drawn at plates.js's TURNS`);
  }
});

test("every plate on the page has its still picture and its sheet of turns, and every picture site.css names exists", () => {
  const css = read("site.css");
  for (const [, url] of css.matchAll(/url\((\/[^)]+)\)/g)) assert.ok(existsSync(join(PUBLIC_DIR, url.slice(1))), `site.css: ${url}`);
  const foods = new Set(attr(HTML, "data-food"));
  assert.ok(foods.size > 0);
  for (const f of foods) {
    assert.ok(css.includes(`.dish[data-food=${f}]{background-image:url(/img/props/plate-${f}.webp)}`), `${f}: its still`);
    assert.ok(existsSync(join(PUBLIC_DIR, `img/props/plate-${f}-turns.webp`)), `${f}: its sheet`);
  }
});

test("the scenes load with the robot, only when motion is welcome, and leave the hero's .stage to robot.js", () => {
  const loader = HTML.slice(HTML.lastIndexOf("<script"), HTML.indexOf("</body>"));
  assert.match(loader, /import\("\/scenes.js"\)/);
  assert.match(loader, /prefers-reduced-motion: reduce/);
  assert.equal(HTML.match(/class="stage"/g).length, 1, "robot.js finds the first .stage: the hero's must be the only one");
});
