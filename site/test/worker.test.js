import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import { test } from "node:test";
import { handleCount, handlePage, handleWaitlist, MESSAGES, normaliseEmail, RESULT_SLOT } from "../src/waitlist.js";
import worker from "../src/worker.js";

const PUBLIC = new URL("../public/", import.meta.url);
const INDEX = readFileSync(new URL("index.html", PUBLIC), "utf8");

// A KV namespace in memory: get, put with metadata, and list with prefix and cursor pages. `fail` names the
// calls that throw, as KV does when the daily write quota is gone.
function fakeKV(pageSize = 1000, fail = []) {
  const store = new Map();
  let puts = 0;
  const check = (op) => {
    if (fail.includes(op)) throw new Error(`KV ${op}() limit exceeded for the day.`);
  };
  return {
    store,
    get puts() {
      return puts;
    },
    async get(k) {
      check("get");
      return store.get(k)?.value ?? null;
    },
    async put(k, value, opts = {}) {
      check("put");
      puts++;
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

const URL_ = "https://potluck.sh/api/waitlist";
const env = (extra = {}) => ({ WAITLIST: fakeKV(), ...extra });
const JSON_ACCEPT = { accept: "application/json" };

// A form post as the page's script sends it (asks for JSON).
function post(fields, headers = {}) {
  return new Request(URL_, {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded", ...JSON_ACCEPT, ...headers },
    body: new URLSearchParams(fields),
  });
}

// A plain browser form post, no JavaScript.
function formPost(fields, headers = {}) {
  return post(fields, { accept: "text/html,application/xhtml+xml,*/*;q=0.8", ...headers });
}

function countReq(auth) {
  const headers = auth === undefined ? {} : { authorization: auth };
  return new Request("https://potluck.sh/api/waitlist/count", { headers });
}

const record = (e, email) => JSON.parse(e.WAITLIST.store.get(`email:${email}`).value);

const T0 = new Date("2026-11-09T12:00:00Z");
const T1 = new Date("2026-11-10T08:30:00Z");
const T2 = new Date("2026-11-11T09:00:00Z");

test("stores hub and capacity independently with the time, and echoes nothing", async () => {
  const e = env();
  const res = await handleWaitlist(post({ email: " Dev@Example.com ", hub: "yes" }), e, T0);
  assert.equal(res.status, 200);
  assert.equal(res.headers.get("content-type"), "application/json; charset=utf-8");
  const text = await res.text();
  assert.deepEqual(JSON.parse(text), { ok: true });
  assert.ok(!text.includes("example.com") && !text.includes("hub"));
  assert.deepEqual([...e.WAITLIST.store.keys()], ["email:dev@example.com"]);
  const { value, metadata } = e.WAITLIST.store.get("email:dev@example.com");
  assert.deepEqual(JSON.parse(value), { hub: true, capacity: false, joined: T0.toISOString(), updated: T0.toISOString() });
  assert.deepEqual(metadata, { hub: true, capacity: false });
});

test("a sign-up with neither box ticked is valid; both boxes are stored together", async () => {
  const e = env();
  assert.equal((await handleWaitlist(post({ email: "none@example.com" }), e, T0)).status, 200);
  assert.deepEqual(record(e, "none@example.com"), { hub: false, capacity: false, joined: T0.toISOString(), updated: T0.toISOString() });
  assert.equal((await handleWaitlist(post({ email: "both@example.com", hub: "on", capacity: "on" }), e, T0)).status, 200);
  assert.deepEqual(record(e, "both@example.com"), { hub: true, capacity: true, joined: T0.toISOString(), updated: T0.toISOString() });
  // Anything but a tick value counts as unticked.
  await handleWaitlist(post({ email: "odd@example.com", hub: "no", capacity: "" }), e, T0);
  assert.deepEqual(record(e, "odd@example.com"), { hub: false, capacity: false, joined: T0.toISOString(), updated: T0.toISOString() });
});

test("accepts a JSON body with booleans and a same-site Origin", async () => {
  const e = env();
  const req = new Request(URL_, {
    method: "POST",
    headers: { "content-type": "application/json", origin: "https://potluck.sh", ...JSON_ACCEPT },
    body: JSON.stringify({ email: "a@b.co", hub: false, capacity: true }),
  });
  const res = await handleWaitlist(req, e, T0);
  assert.equal(res.status, 200);
  const r = record(e, "a@b.co");
  assert.equal(r.hub, false);
  assert.equal(r.capacity, true);
});

test("a repeat sign-up may add interests but never clears one", async () => {
  const e = env();
  await handleWaitlist(post({ email: "a@b.co", hub: "yes" }), e, T0);
  const res = await handleWaitlist(post({ email: "A@B.CO", capacity: "yes" }), e, T1);
  assert.deepEqual(await res.json(), { ok: true }, "same answer for a known address");
  assert.equal(e.WAITLIST.store.size, 1);
  assert.deepEqual(record(e, "a@b.co"), { hub: true, capacity: true, joined: T0.toISOString(), updated: T1.toISOString() });
  assert.deepEqual(e.WAITLIST.store.get("email:a@b.co").metadata, { hub: true, capacity: true });

  // Someone else posting the address with nothing ticked cannot zero the answers, and costs no write.
  const puts = e.WAITLIST.puts;
  const again = await handleWaitlist(post({ email: "  a@b.co", hub: "", capacity: "" }), e, T2);
  assert.deepEqual(await again.json(), { ok: true });
  assert.deepEqual(record(e, "a@b.co"), { hub: true, capacity: true, joined: T0.toISOString(), updated: T1.toISOString() });
  assert.equal(e.WAITLIST.puts, puts);
});

test("email: normalised for the key, and only plain ASCII addresses accepted", async () => {
  const good = [
    ["  Mixed@Example.COM\n", "mixed@example.com"],
    ["o'brien+tag@mail.example.co.uk", "o'brien+tag@mail.example.co.uk"],
    ["a.b_c%d!#$&*/=?^`{|}~-@x-y.example.com", "a.b_c%d!#$&*/=?^`{|}~-@x-y.example.com"],
    [`${"a".repeat(64)}@example.com`, `${"a".repeat(64)}@example.com`],
  ];
  for (const [raw, want] of good) assert.equal(normaliseEmail(raw), want, raw);

  const bad = [
    "", "not-an-email", "a@b", "@example.com", "a@@example.com", "a b@example.com", "<x>@example.com",
    `${"a".repeat(65)}@example.com`, "=cmd@example.com", "+a@example.com", "-a@example.com", "a@exa_mple.com",
    "a@-example.com", "a@example.com.", "a@example..com", 'a"b@example.com', "a,b@example.com",
    "a\u0000b@example.com", "a\tb@example.com", "a\u007fb@example.com", "ü@example.com", "a@exämple.com",
    "Kelvin@example.com", // the Kelvin sign lower-cases to an ASCII k
    `${"a".repeat(64)}@${`${"b".repeat(60)}.`.repeat(4)}com`, // over 254
  ];
  assert.equal(normaliseEmail(undefined), null);
  assert.equal(normaliseEmail(["a@b.co"]), null);
  for (const email of bad) {
    assert.equal(normaliseEmail(email), null, JSON.stringify(email));
    const e = env();
    const res = await handleWaitlist(post({ email, hub: "yes" }), e, T0);
    assert.equal(res.status, 400, JSON.stringify(email));
    assert.deepEqual(await res.json(), { ok: false, error: "email" });
    assert.equal(e.WAITLIST.store.size, 0);
  }
});

test("honeypot: answers like a success and stores nothing", async () => {
  const e = env();
  const res = await handleWaitlist(post({ email: "a@b.co", hub: "yes", website: "spam.example" }), e, T0);
  assert.equal(res.status, 200);
  assert.deepEqual(await res.json(), { ok: true });
  const plain = await handleWaitlist(formPost({ email: "a@b.co", website: "spam.example" }), e, T0);
  assert.equal(plain.status, 303);
  assert.equal(plain.headers.get("location"), "/?joined=1#waitlist");
  assert.equal(e.WAITLIST.store.size, 0);
});

test("a plain form post (no application/json in Accept) gets a 303 back to the page", async () => {
  const e = env({ WAITLIST_LIMITER: { limit: async () => ({ success: true }) } });
  const ok = await handleWaitlist(formPost({ email: "a@b.co", hub: "on" }), e, T0);
  assert.equal(ok.status, 303);
  assert.equal(ok.headers.get("location"), "/?joined=1#waitlist");
  assert.equal(ok.headers.get("cache-control"), "no-store");
  assert.equal(record(e, "a@b.co").hub, true);

  const noAccept = new Request(URL_, {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded" },
    body: "email=bad",
  });
  const bad = await handleWaitlist(noAccept, e, T0);
  assert.equal(bad.status, 303);
  assert.equal(bad.headers.get("location"), "/?error=email#waitlist");

  const limited = env({ WAITLIST_LIMITER: { limit: async () => ({ success: false }) } });
  assert.equal((await handleWaitlist(formPost({ email: "a@b.co" }), limited)).headers.get("location"), "/?error=rate#waitlist");
  const cross = await handleWaitlist(formPost({ email: "a@b.co" }, { origin: "https://evil.example" }), e);
  assert.equal(cross.headers.get("location"), "/?error=origin#waitlist");
  assert.equal(limited.WAITLIST.store.size, 0);
});

test("KV and limiter failures answer 503 unavailable in our own words, never throw", async () => {
  for (const fail of [["get"], ["put"]]) {
    const e = { WAITLIST: fakeKV(1000, fail) };
    const res = await handleWaitlist(post({ email: "a@b.co", hub: "yes" }), e, T0);
    assert.equal(res.status, 503, fail[0]);
    assert.deepEqual(await res.json(), { ok: false, error: "unavailable" });
    const plain = await handleWaitlist(formPost({ email: "a@b.co" }), e, T0);
    assert.equal(plain.headers.get("location"), "/?error=unavailable#waitlist");
  }
  const broken = env({ WAITLIST_LIMITER: { limit: async () => { throw new Error("limiter down"); } } });
  const res = await handleWaitlist(post({ email: "a@b.co" }), broken, T0);
  assert.equal(res.status, 503);
  assert.equal(broken.WAITLIST.store.size, 0);
  assert.equal((await handleWaitlist(post({ email: "a@b.co" }), {}, T0)).status, 503, "no KV binding");

  for (const fail of [["list"], ["get"]]) {
    const kv = fakeKV(1000, fail);
    kv.store.set("email:x@example.com", { value: "{}", metadata: undefined });
    const res = await handleCount(countReq("Bearer t"), { WAITLIST: kv, WAITLIST_ADMIN_TOKEN: "t" });
    assert.equal(res.status, 503, fail[0]);
    assert.deepEqual(await res.json(), { ok: false, error: "unavailable" });
  }
});

test("refuses oversized bodies, with or without content-length", async () => {
  const big = new URLSearchParams({ email: "a@b.co", hub: "yes", pad: "x".repeat(5000) }).toString();
  const e = env();
  assert.equal((await handleWaitlist(post({ email: "a@b.co", hub: "yes", pad: "x".repeat(5000) }), e)).status, 413);
  const stream = new ReadableStream({
    start(c) {
      for (let i = 0; i < big.length; i += 1000) c.enqueue(new TextEncoder().encode(big.slice(i, i + 1000)));
      c.close();
    },
  });
  const chunked = new Request(URL_, {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded", ...JSON_ACCEPT },
    body: stream,
    duplex: "half",
  });
  assert.equal(chunked.headers.get("content-length"), null);
  assert.equal((await handleWaitlist(chunked, e)).status, 413);
  assert.equal(e.WAITLIST.store.size, 0);
});

test("refuses other methods, other sites, unknown bodies, and rate-limited clients", async () => {
  const e = env();
  const get = await handleWaitlist(new Request(URL_), e);
  assert.equal(get.status, 405);
  assert.equal(get.headers.get("allow"), "POST");

  const cross = await handleWaitlist(post({ email: "a@b.co", hub: "yes" }, { origin: "https://evil.example" }), e);
  assert.equal(cross.status, 403);

  const text = new Request(URL_, { method: "POST", headers: { "content-type": "text/plain", ...JSON_ACCEPT }, body: "email=a@b.co" });
  assert.equal((await handleWaitlist(text, e)).status, 400);
  const badJson = new Request(URL_, { method: "POST", headers: { "content-type": "application/json", ...JSON_ACCEPT }, body: "[1]" });
  assert.equal((await handleWaitlist(badJson, e)).status, 400);

  const keys = [];
  const limited = env({ WAITLIST_LIMITER: { limit: async ({ key }) => (keys.push(key), { success: false }) } });
  const res = await handleWaitlist(post({ email: "a@b.co", hub: "yes" }, { "cf-connecting-ip": "203.0.113.7" }), limited);
  assert.equal(res.status, 429);
  assert.deepEqual(await res.json(), { ok: false, error: "rate" });
  assert.deepEqual(keys, ["203.0.113.7"]);
  assert.equal(limited.WAITLIST.store.size, 0);
  assert.equal(e.WAITLIST.store.size, 0);
});

test("count needs the bearer token and fails closed without one configured", async () => {
  const e = env({ WAITLIST_ADMIN_TOKEN: "s3cret-token" });
  await handleWaitlist(post({ email: "a@b.co", hub: "yes" }), e, T0);
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
  const signups = [
    ["one@example.com", {}],
    ["two@example.com", { hub: "yes" }],
    ["three@example.com", { hub: "yes", capacity: "yes" }],
    ["four@example.com", { capacity: "yes" }],
    ["five@example.com", {}],
    ["two@example.com", { capacity: "yes" }], // adds capacity, keeps hub
  ];
  for (const [email, ticks] of signups) await handleWaitlist(post({ email, ...ticks }), e, T0);
  // A record written without metadata is still counted from its value.
  await e.WAITLIST.put("email:six@example.com", JSON.stringify({ hub: true, capacity: false, joined: "x", updated: "x" }));

  const res = await handleCount(countReq("Bearer s3cret-token"), e);
  assert.equal(res.status, 200);
  const text = await res.text();
  assert.deepEqual(JSON.parse(text), { total: 6, hub: 3, capacity: 3, both: 2 });
  assert.ok(!text.includes("@"));
});

test("the page fills in the result line for ?joined=1 and ?error=<code>, from fixed words only", async () => {
  assert.equal(INDEX.split(RESULT_SLOT).length, 2, "index.html has exactly one empty result line");
  const assets = { fetch: async () => new Response(INDEX, { headers: { "content-type": "text/html; charset=utf-8", etag: '"x"' } }) };
  const page = async (q) => handlePage(new Request(`https://potluck.sh/${q}`), { ASSETS: assets });

  const joined = await page("?joined=1");
  assert.equal(joined.status, 200);
  assert.equal(joined.headers.get("cache-control"), "no-store");
  assert.equal(joined.headers.get("etag"), null);
  assert.ok((await joined.text()).includes(`<p class="result" role="status" aria-live="polite">${MESSAGES.joined}</p>`));

  const rate = await (await page("?error=rate")).text();
  assert.ok(rate.includes(`<p class="result bad" role="status" aria-live="polite">${MESSAGES.rate}</p>`));
  for (const q of ["?error=unavailable", "?error=joined", "?error=%3Cscript%3Ealert(1)%3C/script%3E", "?error=__proto__"]) {
    const html = await (await page(q)).text();
    assert.ok(html.includes("That didn't work. Try again, or email ops@sengi.run."), q);
    assert.ok(!html.includes("<script>alert"), q);
  }
  assert.equal(await (await page("")).text(), INDEX);
  assert.equal(await (await page("?joined=0")).text(), INDEX);
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

test("the page makes no third-party requests and has the waitlist fields", () => {
  for (const pattern of THIRD_PARTY) assert.doesNotMatch(INDEX, pattern);
  assert.doesNotMatch(INDEX, /googleapis|gstatic/);
  const fonts = [...INDEX.matchAll(/url\(\s*['"]?(\/fonts\/[^)'"\s]+)['"]?\s*\)/g)].map((m) => m[1]);
  assert.ok(fonts.length > 0, "the page loads its fonts from /fonts/");
  for (const f of fonts) assert.ok(existsSync(new URL(`.${f}`, PUBLIC)), f);
  assert.ok(existsSync(new URL("fonts/OFL-Newsreader.txt", PUBLIC)));
  assert.match(INDEX, /<input type="checkbox" name="hub"/);
  assert.match(INDEX, /<input type="checkbox" name="capacity"/);
  assert.doesNotMatch(INDEX, /name="choice"/);
  assert.match(INDEX, /<label for="waitlist-email">Email address<\/label>/);
  assert.match(INDEX, /<label class="hp" aria-hidden="true">[^<]*<input [^>]*name="website" tabindex="-1"/);
  assert.ok(INDEX.includes(
    "We store your email, your answers and the time. Sengi keeps the list on Cloudflare, shares it with nobody, sends one email when there is news, and deletes it within 12 months. To be removed, email",
  ));
  assert.doesNotMatch(INDEX, /—/, "no em dashes");
});

test("the entry module exports only the handler (workerd refuses other named exports)", async () => {
  assert.deepEqual(Object.keys(await import("../src/worker.js")), ["default"]);
});

test("the worker routes the API and the page, and sends everything else to the static assets", async () => {
  const seen = [];
  const e = env({ ASSETS: { fetch: async (req) => (seen.push(new URL(req.url).pathname), new Response("asset")) } });
  assert.equal(await (await worker.fetch(new Request("https://potluck.sh/"), e)).text(), "asset");
  assert.equal(await (await worker.fetch(new Request("https://potluck.sh/api/other"), e)).text(), "asset");
  assert.equal((await worker.fetch(post({ email: "a@b.co", hub: "yes" }), e)).status, 200);
  assert.equal((await worker.fetch(countReq(), e)).status, 401);
  assert.deepEqual(seen, ["/", "/api/other"]);
});
