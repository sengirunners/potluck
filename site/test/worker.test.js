import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import { test } from "node:test";
import { handleCount } from "../src/waitlist.js";
import worker from "../src/worker.js";

const PUBLIC = new URL("../public/", import.meta.url);
const INDEX = readFileSync(new URL("index.html", PUBLIC), "utf8");

// A KV namespace in memory: get, put with metadata, and list with prefix and cursor pages. `fail` names the
// calls that throw.
function fakeKV(pageSize = 1000, fail = []) {
  const store = new Map();
  const check = (op) => {
    if (fail.includes(op)) throw new Error(`KV ${op}() failed.`);
  };
  return {
    store,
    async get(k) {
      check("get");
      return store.get(k)?.value ?? null;
    },
    async put(k, value, opts = {}) {
      check("put");
      store.set(k, { value, metadata: opts.metadata });
    },
    async list({ prefix = "", cursor } = {}) {
      check("list");
      const names = [...store.keys()].filter((k) => k.startsWith(prefix)).sort();
      const start = cursor ? Number(cursor) : 0;
      const slice = names.slice(start, start + pageSize);
      const done = start + pageSize >= names.length;
      return {
        keys: slice.map((name) => ({ name, metadata: store.get(name).metadata })),
        list_complete: done,
        cursor: done ? undefined : String(start + pageSize),
      };
    },
  };
}

const env = (extra = {}) => ({ WAITLIST: fakeKV(), ...extra });

// A sign-up as the old form stored it: the record, with hub and capacity also in the key's metadata.
async function signedUp(kv, email, hub, capacity) {
  const record = { hub, capacity, joined: "2026-11-09T12:00:00.000Z", updated: "2026-11-09T12:00:00.000Z" };
  await kv.put(`email:${email}`, JSON.stringify(record), { metadata: { hub, capacity } });
}

function countReq(auth) {
  const headers = auth === undefined ? {} : { authorization: auth };
  return new Request("https://potluck.sh/api/waitlist/count", { headers });
}

test("count needs the bearer token and fails closed without one configured", async () => {
  const e = env({ WAITLIST_ADMIN_TOKEN: "s3cret-token" });
  await signedUp(e.WAITLIST, "a@b.co", true, false);
  for (const auth of [undefined, "", "Bearer", "Bearer wrong", "Basic s3cret-token", "Bearer s3cret-token-x"]) {
    const res = await handleCount(countReq(auth), e);
    assert.equal(res.status, 401, String(auth));
    assert.equal(res.headers.get("www-authenticate"), "Bearer");
  }
  for (const token of [undefined, ""]) {
    const res = await handleCount(countReq("Bearer "), env({ WAITLIST_ADMIN_TOKEN: token }));
    assert.equal(res.status, 401);
    assert.equal((await handleCount(countReq("Bearer undefined"), env({ WAITLIST_ADMIN_TOKEN: token }))).status, 401);
  }
  const post_ = new Request("https://potluck.sh/api/waitlist/count", { method: "POST", headers: { authorization: "Bearer s3cret-token" } });
  assert.equal((await handleCount(post_, e)).status, 405);
});

test("count returns only total, hub, capacity and both, across pages, and no emails", async () => {
  const e = { WAITLIST: fakeKV(2), WAITLIST_ADMIN_TOKEN: "s3cret-token" };
  await signedUp(e.WAITLIST, "one@example.com", false, false);
  await signedUp(e.WAITLIST, "two@example.com", true, true);
  await signedUp(e.WAITLIST, "three@example.com", true, true);
  await signedUp(e.WAITLIST, "four@example.com", false, true);
  await signedUp(e.WAITLIST, "five@example.com", false, false);
  // A record written without metadata is still counted from its value.
  await e.WAITLIST.put("email:six@example.com", JSON.stringify({ hub: true, capacity: false, joined: "x", updated: "x" }));

  const res = await handleCount(countReq("Bearer s3cret-token"), e);
  assert.equal(res.status, 200);
  const text = await res.text();
  assert.deepEqual(JSON.parse(text), { total: 6, hub: 3, capacity: 3, both: 2 });
  assert.ok(!text.includes("@"));
});

test("count answers 503 in our own words when KV fails or is missing", async () => {
  for (const e of [{ WAITLIST: fakeKV(1000, ["list"]) }, {}]) {
    const res = await handleCount(countReq("Bearer t"), { ...e, WAITLIST_ADMIN_TOKEN: "t" });
    assert.equal(res.status, 503);
    assert.deepEqual(await res.json(), { ok: false, error: "unavailable" });
  }
});

test("sign-ups are closed: a post gets 410 and stores nothing", async () => {
  const e = env();
  const res = await worker.fetch(
    new Request("https://potluck.sh/api/waitlist", {
      method: "POST",
      headers: { accept: "application/json", "content-type": "application/x-www-form-urlencoded" },
      body: "email=a%40b.co&hub=yes",
    }),
    e,
  );
  assert.equal(res.status, 410);
  assert.deepEqual(await res.json(), { ok: false, error: "closed" });
  assert.equal(e.WAITLIST.store.size, 0);
});

// Each pattern matches markup that would fetch from another site. A same-site path starts with one "/";
// "//host" is protocol-relative and goes elsewhere. url(#id) points into the page, and data: is inline.
const THIRD_PARTY = [
  /<link[^>]+href=(?!["']?(?:\/(?!\/)|data:|#))/i,
  /<script[^>]+src=/i,
  /<img[^>]+src=(?!["']?(?:\/(?!\/)|data:))/i,
  /url\((?!\s*['"]?(?:\/(?!\/)|data:|#))/i,
  /\s(?:src|href)=["']?\/\//i,
  /@import/i,
];
const fetchesElsewhere = (html) => THIRD_PARTY.some((p) => p.test(html));

test("the third-party check refuses other hosts, protocol-relative ones included", () => {
  for (const bad of [
    'a{background:url("//evil.com/x")}', "a{background:url(//evil.com/x)}", "a{background:url(https://evil.com/x)}",
    "a{background:url('https://evil.com/x')}", '<link rel="stylesheet" href="//evil.com/x.css">',
    '<link rel="stylesheet" href="https://evil.com/x.css">', '<img src="//evil.com/x.png">', '<video src="//evil.com/x">',
    '<script src="/x.js"></script>', "@import 'x.css';",
  ]) assert.ok(fetchesElsewhere(bad), bad);
  for (const ok of [
    "a{background:url(/fonts/x.woff2)}", 'a{background:url("/fonts/x.woff2")}', "a{fill:url(#hatch)}",
    'a{background:url("data:image/svg+xml,x")}', '<link rel="preload" href="/fonts/x.woff2">', '<img src="/x.png">',
  ]) assert.ok(!fetchesElsewhere(ok), ok);
});

test("the page makes no third-party requests and loads its fonts from /fonts", () => {
  for (const pattern of THIRD_PARTY) assert.doesNotMatch(INDEX, pattern);
  assert.doesNotMatch(INDEX, /googleapis|gstatic/);
  const fonts = [...INDEX.matchAll(/url\(\s*['"]?(\/fonts\/[^)'"\s]+)['"]?\s*\)/g)].map((m) => m[1]);
  assert.ok(fonts.length > 0, "the page loads its fonts from /fonts/");
  for (const f of fonts) assert.ok(existsSync(new URL(`.${f}`, PUBLIC)), f);
  assert.ok(existsSync(new URL("fonts/OFL-Newsreader.txt", PUBLIC)));
  assert.doesNotMatch(INDEX, /—/, "no em dashes");
});

test("the page offers the install line, the founders' address and Potluck Dish, and no waitlist", () => {
  assert.match(INDEX, /<code id="install-cmd">curl -fsSL https:\/\/raw\.githubusercontent\.com\/sengirunners\/potluck\/main\/install\.sh \| sh<\/code>/);
  assert.match(INDEX, /href="https:\/\/github\.com\/sengirunners\/potluck"/);
  assert.match(INDEX, /href="mailto:ops@sengi\.run/);
  assert.match(INDEX, /Potluck Dish/);
  assert.doesNotMatch(INDEX, /<form|\/api\/waitlist|waitlist/i);
  assert.doesNotMatch(INDEX, /\(soon\)|stet|not ready/i);
});

// The scene's script finds each machine's label by data-for and the point it labels by data-pin. A machine with no
// label (or a label with no machine) would break the layout or leave a button that does nothing.
test("every machine in the picnic has one label and one pin, and every label has a machine", () => {
  const ids = (re) => [...INDEX.matchAll(re)].map((m) => m[1]).sort();
  const machines = ids(/<div class="m\b[^"]*"\s+data-id="([^"]+)"/g);
  assert.deepEqual(machines, ["cloud", "dish", "laptop", "mini-mac", "mini-pc", "server", "tower"]);
  assert.deepEqual(ids(/data-for="([^"]+)"/g), machines);
  assert.deepEqual(ids(/data-pin="([^"]+)"/g), machines);
  // The card's data: every machine but the dish, which links to its own box. The object literal is evaluated, so
  // its layout (indentation, quoting, line breaks) does not matter.
  const start = INDEX.indexOf("{", INDEX.indexOf("const MACHINES"));
  let depth = 0, stop = start;
  for (; stop < INDEX.length; stop++) {
    if (INDEX[stop] === "{") depth++;
    if (INDEX[stop] === "}" && --depth === 0) break;
  }
  const data = new Function(`return (${INDEX.slice(start, stop + 1)});`)();
  assert.deepEqual(Object.keys(data).sort(), machines.filter((id) => id !== "dish"));
  assert.match(INDEX, /<a class="tag soon" href="#dish"/);
  assert.match(INDEX, /id="dish"/);
});

// Claims a review found false or misleading (README.md "What works today" and install.sh are the sources).
test("the page hedges what does not work yet and names who to email", () => {
  assert.doesNotMatch(INDEX, /checked before anything runs/i, "a first install on macOS's LibreSSL is not verified");
  assert.match(INDEX, /checks the signature when it can/);
  assert.doesNotMatch(INDEX, /One command opens a pull request/i, "potluck ci pr prepares a script; it does not open the PR");
  assert.match(INDEX, /Not run live yet/, "no Actions job has run live on a pool machine");
  assert.match(INDEX, /Once Potluck runs your GitHub Actions jobs \(not yet/, "the savings band says Actions jobs don't run yet");
  assert.match(INDEX, /potluck agent-runner claude/, "agent-runner takes the adapter's name");
  assert.match(INDEX, /href="https:\/\/github\.com\/sengirunners\/potluck#what-works-today"/);
  assert.doesNotMatch(INDEX, /Apple's licence lets you/i, "the lending rule is our reading, not Apple's");
  assert.match(INDEX, /Jackson and Clay/);
  assert.ok((INDEX.match(/>Email the founders</g) ?? []).length <= 2, "at most two founders buttons");
  assert.doesNotMatch(INDEX, /A person reads every email/);
});

test("the entry module exports only the handler (workerd refuses other named exports)", async () => {
  assert.deepEqual(Object.keys(await import("../src/worker.js")), ["default"]);
});

test("the worker routes the two waitlist paths, and sends everything else to the static assets", async () => {
  const seen = [];
  const e = env({ ASSETS: { fetch: async (req) => (seen.push(new URL(req.url).pathname), new Response("asset")) } });
  assert.equal(await (await worker.fetch(new Request("https://potluck.sh/"), e)).text(), "asset");
  assert.equal(await (await worker.fetch(new Request("https://potluck.sh/?joined=1"), e)).text(), "asset");
  assert.equal(await (await worker.fetch(new Request("https://potluck.sh/api/other"), e)).text(), "asset");
  assert.equal((await worker.fetch(new Request("https://potluck.sh/api/waitlist", { method: "POST" }), e)).status, 410);
  assert.equal((await worker.fetch(countReq(), e)).status, 401);
  assert.deepEqual(seen, ["/", "/", "/api/other"]);
});
