import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { readFileSync } from "node:fs";
import worker from "../src/worker.js";
import { fakeKV } from "./fakekv.js";
import { RESEND_URL } from "../src/notify.js";

const TOKEN = "s3cret-token";
// The Worker's execution context: waitUntil keeps what was handed to it, and `settled` waits for all of it.
function context() {
  const pending = [];
  return { waitUntil: (p) => pending.push(p), settled: () => Promise.all(pending) };
}
const call = (request, e, ctx = context()) => worker.fetch(request, e, ctx);
const env = (extra = {}) => ({ CONTACT: fakeKV(), CONTACT_ADMIN_TOKEN: TOKEN, ...extra });
const good = { name: "Ada", email: "ada@example.com", topic: "run", team: "Analytical", message: "We have three Mac minis." };

function post(fields, { json = true, ip = "203.0.113.7", headers: extra = {} } = {}) {
  const body = new URLSearchParams(fields).toString();
  const headers = { "content-type": "application/x-www-form-urlencoded", "cf-connecting-ip": ip, "user-agent": "test",
    "content-length": String(Buffer.byteLength(body)), ...extra };
  if (json) headers.accept = "application/json";
  return new Request("https://potluck.sh/api/contact", { method: "POST", headers, body });
}
const read = (auth, query = "") =>
  new Request(`https://potluck.sh/api/contact${query}`, { headers: auth === undefined ? {} : { authorization: auth } });
const messages = (kv) => [...kv.store.keys()].filter((k) => k.startsWith("msg:"));

test("a message is kept and the founders can read it back, newest first", async () => {
  const e = env();
  let res = await call(post(good), e);
  assert.equal(res.status, 200);
  assert.deepEqual(await res.json(), { ok: true });
  res = await call(post({ ...good, name: "Grace", message: "Second." }), e);
  assert.equal(res.status, 200);
  res = await call(read(`Bearer ${TOKEN}`), e);
  assert.equal(res.status, 200);
  const { messages: got } = await res.json();
  assert.equal(got.length, 2);
  assert.equal(got[0].name, "Grace");
  assert.equal(got[1].name, "Ada");
  assert.equal(got[1].email, "ada@example.com");
  assert.equal(got[1].team, "Analytical");
  assert.equal(got[1].topic, "run");
  assert.equal(got[1].agent, "test");
  assert.match(got[1].at, /^\d{4}-\d\d-\d\dT/);
  res = await call(read(`Bearer ${TOKEN}`, "?limit=1"), e);
  assert.equal((await res.json()).messages.length, 1);
});

test("a plain form post goes on to the thanks page, or gets a page naming what to fix", async () => {
  const e = env();
  let res = await call(post(good, { json: false }), e);
  assert.equal(res.status, 303);
  assert.equal(res.headers.get("location"), "/thanks");
  res = await call(post({ ...good, email: "nope" }, { json: false }), e);
  assert.equal(res.status, 400);
  assert.match(res.headers.get("content-type"), /text\/html/);
  assert.match(await res.text(), /email address doesn't look right/);
});

test("bad messages are refused and nothing is stored", async () => {
  const cases = [
    [{ ...good, name: "" }, 400, "missing"],
    [{ ...good, message: "   " }, 400, "missing"],
    [{ ...good, email: "ada@example" }, 400, "email"],
    [{ ...good, email: "a da@example.com" }, 400, "email"],
    [{ ...good, topic: "sales" }, 400, "topic"],
    [{ ...good, message: "x".repeat(4001) }, 413, "long"],
    [{ ...good, team: "x".repeat(121) }, 413, "long"],
  ];
  for (const [fields, status, error] of cases) {
    const e = env();
    const res = await call(post(fields), e);
    assert.equal(res.status, status, error);
    assert.deepEqual(await res.json(), { ok: false, error });
    assert.equal(messages(e.CONTACT).length, 0, error);
  }
});

test("a filled hidden field looks like success to the bot and stores nothing", async () => {
  const e = env();
  const res = await call(post({ ...good, leave_empty: "http://spam.example" }), e);
  assert.equal(res.status, 200);
  assert.equal(e.CONTACT.store.size, 0);
});

test("one address gets ten messages an hour", async () => {
  const e = env();
  for (let i = 0; i < 10; i++) assert.equal((await call(post(good), e)).status, 200);
  const res = await call(post(good), e);
  assert.equal(res.status, 429);
  assert.deepEqual(await res.json(), { ok: false, error: "busy" });
  assert.equal((await call(post(good, { ip: "198.51.100.1" }), e)).status, 200);
  assert.equal(messages(e.CONTACT).length, 11);
});

test("storage trouble answers 503 in our own words", async () => {
  for (const e of [env({ CONTACT: undefined }), env({ CONTACT: fakeKV(1000, ["put"]) }), env({ CONTACT: fakeKV(1000, ["get"]) })]) {
    const res = await call(post(good), e);
    assert.equal(res.status, 503);
    assert.deepEqual(await res.json(), { ok: false, error: "unavailable" });
  }
  assert.equal((await call(read(`Bearer ${TOKEN}`), env({ CONTACT: fakeKV(1000, ["list"]) }))).status, 503);
});

test("reading messages needs the bearer token and fails closed without one configured", async () => {
  const e = env();
  await call(post(good), e);
  for (const auth of [undefined, "", "Bearer", "Bearer wrong", `Basic ${TOKEN}`, `Bearer ${TOKEN}x`]) {
    const res = await call(read(auth), e);
    assert.equal(res.status, 401, String(auth));
    assert.equal(res.headers.get("www-authenticate"), "Bearer");
  }
  for (const token of [undefined, ""]) {
    assert.equal((await call(read("Bearer "), env({ CONTACT_ADMIN_TOKEN: token }))).status, 401);
    assert.equal((await call(read("Bearer undefined"), env({ CONTACT_ADMIN_TOKEN: token }))).status, 401);
  }
  const put = new Request("https://potluck.sh/api/contact", { method: "PUT" });
  assert.equal((await call(put, e)).status, 405);
});

test("posts from another site, of no stated length, or too big are refused before the form is read", async () => {
  const e = env();
  let res = await call(post(good, { headers: { origin: "https://evil.example" } }), e);
  assert.equal(res.status, 403);
  assert.deepEqual(await res.json(), { ok: false, error: "origin" });
  assert.equal((await call(post(good, { headers: { origin: "https://potluck.sh" } }), e)).status, 200);
  res = await call(post(good, { headers: { "content-length": "999999" } }), e);
  assert.equal(res.status, 413);
  const bare = new Request("https://potluck.sh/api/contact", { method: "POST", headers: { accept: "application/json" }, body: "name=x" });
  assert.equal((await call(bare, e)).status, 411);
  assert.equal(messages(e.CONTACT).length, 1);
});

test("the founders read messages a page at a time, following next", async () => {
  const e = env({ CONTACT: fakeKV(2) });
  for (const name of ["a", "b", "c", "d", "e"]) await call(post({ ...good, name }), e);
  await e.CONTACT.put("msg:0000000000000000:broken", "not json");
  const seen = [];
  let cursor = "", pages = 0;
  do {
    const res = await call(read(`Bearer ${TOKEN}`, `?limit=2${cursor ? `&cursor=${cursor}` : ""}`), e);
    assert.equal(res.status, 200);
    const out = await res.json();
    seen.push(...out.messages.map((m) => m.name));
    cursor = out.next;
    pages++;
  } while (cursor && pages < 10);
  assert.deepEqual(seen.sort(), ["a", "b", "c", "d", "e"]);
  assert.equal(pages, 3);
  const res = await call(read(`Bearer ${TOKEN}`, "?limit=500"), env());
  assert.equal(res.status, 200);
});

// Resend's API in memory. Each mail env has its own key; a POST to RESEND_URL with that key lands in its `sent`, as
// the email Resend was asked to send. `fail` answers 422 as Resend does for an unverified sender; `hang` never answers.
const outboxes = new Map();
let keys = 0;
// A request of the wrong shape is recorded in `wrong` (sendNotice catches whatever fetch throws), and `after` fails on it.
const wrong = [];
const realFetch = globalThis.fetch;
before(() => {
  globalThis.fetch = async (url, init = {}) => {
    const box = outboxes.get(init.headers?.authorization);
    if (url !== RESEND_URL || init.method !== "POST" || init.headers["content-type"] !== "application/json" || !box) {
      wrong.push(`${init.method} ${url} ${JSON.stringify(init.headers ?? {}).replace(/re_test_\d+/, "<key>")}`);
      throw new Error("not a request Resend takes");
    }
    if (box.hang) return new Promise(() => {});
    if (box.fail) return new Response(JSON.stringify({ name: "validation_error", message: "The sengi.run domain is not verified." }), { status: 422 });
    const body = JSON.parse(init.body);
    box.sent.push({ ...body, replyTo: body.reply_to });
    return Response.json({ id: `m${box.sent.length}` });
  };
});
after(() => {
  globalThis.fetch = realFetch;
  assert.deepEqual(wrong, [], "requests that were not a send to Resend");
});
function mailEnv({ fail = false, hang = false, ...extra } = {}) {
  const key = `re_test_${++keys}`;
  const box = { sent: [], fail, hang };
  outboxes.set(`Bearer ${key}`, box);
  const e = env({ RESEND_API_KEY: key, NOTIFY_FROM: "Founders form <form@example.com>", NOTIFY_TO: "ops@example.com", ...extra });
  return Object.assign(e, { outbox: box });
}

test("each site's config names a sender and one recipient for the notice", () => {
  for (const file of ["../wrangler.jsonc", "../mezze/wrangler.jsonc"]) {
    const text = readFileSync(new URL(file, import.meta.url), "utf8");
    const config = JSON.parse(text.split("\n").filter((l) => !l.trim().startsWith("//")).join("\n"));
    assert.match(config.vars.NOTIFY_FROM, /^[^<>]+ <[^\s@<>]+@[^\s@<>]+>$/, file);
    assert.match(config.vars.NOTIFY_TO, /^[^\s@]+@[^\s@]+$/, file);
    assert.equal(config.send_email, undefined, file);
  }
});

test("a stored message is emailed to the founders after the answer, in plain text with Reply-To the sender", async () => {
  const e = mailEnv();
  const ctx = context();
  const res = await call(post({ ...good, country: "ignored" }), e, ctx);
  assert.equal(res.status, 200);
  await ctx.settled();
  assert.equal(e.outbox.sent.length, 1);
  const m = e.outbox.sent[0];
  assert.equal(m.from, "Founders form <form@example.com>");
  assert.deepEqual(m.to, ["ops@example.com"]);
  assert.equal(m.replyTo, "ada@example.com");
  assert.equal(m.html, undefined);
  assert.equal(m.subject, "potluck.sh: Ada (I want to run it)");
  for (const want of ["Ada <ada@example.com>", "Topic:   I want to run it", "Team:    Analytical", "We have three Mac minis."]) {
    assert.ok(m.text.includes(want), want);
  }
  assert.match(m.text, /At:      \d{4}-\d\d-\d\dT/);
});

test("the notice cannot carry extra headers or markup from what a visitor typed", async () => {
  const e = mailEnv();
  const ctx = context();
  const name = "Eve\r\nBcc: victim@example.com";
  const message = "<script>alert(1)</script>\r\nline two\u0000\u2028\u202eend";
  await call(post({ ...good, name, message, email: "eve+x@example.com" }), e, ctx);
  await ctx.settled();
  const m = e.outbox.sent[0];
  assert.doesNotMatch(m.subject, /[\r\n]/);
  assert.equal(m.subject, "potluck.sh: Eve Bcc: victim@example.com (I want to run it)");
  assert.equal(m.html, undefined, "plain text only: markup is shown as typed, never rendered");
  assert.ok(m.text.includes("<script>alert(1)</script>\nline two end"));
  assert.doesNotMatch(m.text, /\r|\u0000|\u2028|\u202e/);
  assert.match(m.text, /^From:    Eve Bcc: victim@example\.com <eve\+x@example\.com>$/m);
  assert.equal(m.replyTo, "eve+x@example.com");
  // An address the form takes but of an unusual shape is left out of Reply-To; it is still in the body.
  const odd = mailEnv();
  const ctx2 = context();
  await call(post({ ...good, email: '"a>b"@example.com' }), odd, ctx2);
  await ctx2.settled();
  assert.equal(odd.outbox.sent[0].replyTo, undefined);
  assert.ok(odd.outbox.sent[0].text.includes('"a>b"@example.com'));
});

test("refused, bot-trap and rate-limited posts send nothing", async () => {
  const e = mailEnv();
  const ctx = context();
  for (const fields of [{ ...good, leave_empty: "x" }, { ...good, email: "nope" }, { ...good, topic: "sales" }, { ...good, name: "" }]) {
    await call(post(fields), e, ctx);
  }
  await call(post(good, { headers: { origin: "https://evil.example" } }), e, ctx);
  for (let i = 0; i < 10; i++) await call(post(good, { ip: "198.51.100.9" }), e, ctx);
  await ctx.settled();
  assert.equal(e.outbox.sent.length, 10);
  const busy = await call(post(good, { ip: "198.51.100.9" }), e, ctx);
  assert.equal(busy.status, 429);
  const broken = mailEnv({ CONTACT: fakeKV(1000, ["put"]) });
  assert.equal((await call(post(good), broken, ctx)).status, 503);
  await ctx.settled();
  assert.equal(e.outbox.sent.length, 10);
  assert.equal(broken.outbox.sent.length, 0);
});

test("the answer does not wait for the email", async () => {
  const e = mailEnv({ hang: true });
  const res = await call(post(good), e);
  assert.equal(res.status, 200);
  assert.deepEqual(await res.json(), { ok: true });
  assert.equal(messages(e.CONTACT).length, 1);
});

test("a notice that cannot be sent is logged and the visitor still gets ok, with the message kept", async (t) => {
  const errors = [];
  t.mock.method(console, "error", (...args) => errors.push(args.join(" ")));
  t.mock.method(console, "warn", () => {});
  for (const e of [mailEnv({ fail: true }), mailEnv({ NOTIFY_FROM: undefined }), mailEnv({ NOTIFY_TO: "" }), env()]) {
    const ctx = context();
    let res = await call(post(good), e, ctx);
    assert.equal(res.status, 200);
    assert.deepEqual(await res.json(), { ok: true });
    res = await call(post(good, { json: false }), e, ctx);
    assert.equal(res.status, 303);
    await ctx.settled();
    assert.equal(messages(e.CONTACT).length, 2);
  }
  assert.ok(errors.some((l) => l.includes("Resend answered 422") && l.includes("not verified")));
  assert.ok(errors.some((l) => l.includes("NOTIFY_FROM")));
  assert.ok(errors.some((l) => l.includes("NOTIFY_TO")));
});
