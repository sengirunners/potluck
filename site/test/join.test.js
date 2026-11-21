import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import worker from "../src/worker.js";
import { joinView, platformOf } from "../public/join.js";

const PUBLIC = new URL("../public/", import.meta.url);
const read = (name) => readFileSync(new URL(name, PUBLIC), "utf8");
const HTML = read("join.html");
const ORIGIN = "https://potluck.sh";
const FRAG = "v1.AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-";

test("a valid fragment on a computer gives the exact install command and the manual line", () => {
  for (const hash of [`#${FRAG}`, FRAG]) {
    const v = joinView(hash, ORIGIN, "mac");
    assert.equal(v.kind, "command");
    assert.equal(v.command, `curl -fsSL https://potluck.sh/install.sh | sh -s -- --join '${ORIGIN}/join#${FRAG}'`);
    assert.equal(v.github, `curl -fsSL https://potluck.sh/install.sh | sh -s -- --join '${ORIGIN}/join#${FRAG}' --github`);
    assert.equal(v.link, `${ORIGIN}/join#${FRAG}`);
    assert.equal(v.mac, true);
  }
  assert.equal(joinView(`#${FRAG}`, ORIGIN, "linux").mac, false);
});

test("anything that is not a plain invite fragment gives no command", () => {
  const bad = [
    "", "#", "#v1.", "#v1.short", `#${FRAG}'`, `#${FRAG}' ; rm -rf ~ ; '`, `#${FRAG} x`, "#v1.abcdefghijklmnop$(id)",
    `#${FRAG}#more`, `##${FRAG}`, `#v2.${FRAG.slice(3)}`, `#${FRAG.slice(3)}`, `#${FRAG}\n`, `#v1.${"a".repeat(601)}`,
    '#v1.abcdefghijklmnop"', "#v1.abcdefghijklmnop`id`", "#v1.abcdefghijklmnop;id", "#v1.abcdefghijklmnop%27",
  ];
  for (const hash of bad) {
    for (const platform of ["mac", "linux", "other"]) {
      assert.deepEqual(joinView(hash, ORIGIN, platform), { kind: "incomplete" }, JSON.stringify(hash));
    }
  }
  assert.equal(joinView(`#v1.${"a".repeat(600)}`, ORIGIN, "mac").kind, "command");
  assert.equal(joinView(`#v1.${"a".repeat(16)}`, ORIGIN, "mac").kind, "command");
  for (const origin of ["", "https://potluck.sh/x", "https://a.b'c", "javascript:alert(1)", "https://a b"]) {
    assert.equal(joinView(`#${FRAG}`, origin, "mac").kind, "incomplete", origin);
  }
});

test("a phone or Windows machine is told to open the link on the computer to join with", () => {
  assert.deepEqual(joinView(`#${FRAG}`, ORIGIN, "other"), { kind: "link", link: `${ORIGIN}/join#${FRAG}` });
  for (const p of ["iPhone", "Android", "Windows", "Win32", "Mozilla/5.0 (Linux; Android 14)"]) assert.equal(platformOf(p), "other", p);
  assert.equal(platformOf("macOS"), "mac");
  assert.equal(platformOf("MacIntel"), "mac");
  assert.equal(platformOf("Linux x86_64"), "linux");
});

test("the page has no form, no inline script, no third party, and loads only its own files", () => {
  assert.doesNotMatch(HTML, /<form/i);
  assert.doesNotMatch(HTML, /<style|<script>|\son[a-z]+=|javascript:/i);
  assert.doesNotMatch(HTML, /https?:\/\/(?!potluck\.sh\/install\.sh|sengi\.run|mezze\.sh)/);
  assert.match(HTML, /<script type="module" src="\/join\.js"><\/script>/);
  assert.match(HTML, /<link rel="stylesheet" href="\/site\.css">/);
  assert.match(HTML, /<meta name="robots" content="noindex">/);
  assert.doesNotMatch(HTML, /canonical/);
  assert.doesNotMatch(read("join.js"), /fetch\(|XMLHttpRequest|sendBeacon|innerHTML|outerHTML|insertAdjacentHTML|document\.write|eval\(|new Function/);
});

test("with scripts off the page holds the manual line, and script-only blocks start hidden", () => {
  const noscript = HTML.match(/<noscript>([\s\S]*?)<\/noscript>/)?.[1] ?? "";
  assert.match(noscript, /On the Mac or Linux machine you want to use, paste this in a terminal\./);
  assert.ok(noscript.includes("curl -fsSL https://potluck.sh/install.sh | sh -s -- --join '&lt;paste this page's full address here&gt;'"));
  for (const id of ["ok", "phone", "bad", "mac-hint"]) assert.match(HTML, new RegExp(`id="${id}"[^>]*\\shidden`), id);
  assert.doesNotMatch(HTML.replace(/<noscript>[\s\S]*?<\/noscript>/, ""), /<code>curl/, "the command is built by the script");
});

test("the page says what it should", () => {
  for (const s of [
    "You have an invite to a Potluck pool.", "On the Mac or Linux machine you want to use, paste this in a terminal.",
    "Terminal is in Applications, then Utilities.", "Already have Potluck?", "Optional: to also sign in with GitHub", "Open this link on the Mac or Linux machine you want to join with.",
    "This link is incomplete.", "Ask for it again.",
    "The invite works once and expires. If it is refused, ask whoever sent it for a new one. Only use a link from someone you know.",
  ]) assert.ok(HTML.includes(s), s);
  assert.doesNotMatch(HTML, /—/);
});

test("/join gets the headers that keep the invite private", () => {
  // Workers static assets apply public/_headers by path; the Worker never sees /join.
  const rules = new Map();
  let path;
  for (const line of read("_headers").split("\n")) {
    if (!line.trim() || line.startsWith("#")) continue;
    if (!/^\s/.test(line)) {
      path = line.trim();
      rules.set(path, new Map());
      continue;
    }
    const i = line.indexOf(":");
    rules.get(path).set(line.slice(0, i).trim().toLowerCase(), line.slice(i + 1).trim());
  }
  const h = rules.get("/join");
  assert.ok(h, "a rule for /join");
  assert.equal(h.get("referrer-policy"), "no-referrer");
  assert.equal(h.get("cache-control"), "no-store");
  assert.equal(h.get("content-security-policy"), "default-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'");
  assert.equal(h.get("x-robots-tag"), "noindex");
  assert.deepEqual([...rules.keys()], ["/join"], "no other path is touched");
});

test("GET /join goes to the static assets", async () => {
  let seen;
  const env = { ASSETS: { fetch: async (req) => { seen = req; return new Response("page"); } } };
  const res = await worker.fetch(new Request("https://potluck.sh/join"), env);
  assert.equal(await res.text(), "page");
  assert.equal(new URL(seen.url).pathname, "/join");
});

test("/join is in no sitemap, llms or robots file", () => {
  for (const f of ["sitemap.xml", "llms.txt", "llms-full.txt", "robots.txt"]) assert.doesNotMatch(read(f), /\/join/, f);
});
