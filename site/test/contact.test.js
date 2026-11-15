import assert from "node:assert/strict";
import { test } from "node:test";
import worker from "../src/worker.js";
import { fakeKV } from "./fakekv.js";

const TOKEN = "s3cret-token";
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
  let res = await worker.fetch(post(good), e);
  assert.equal(res.status, 200);
  assert.deepEqual(await res.json(), { ok: true });
  res = await worker.fetch(post({ ...good, name: "Grace", message: "Second." }), e);
  assert.equal(res.status, 200);
  res = await worker.fetch(read(`Bearer ${TOKEN}`), e);
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
  res = await worker.fetch(read(`Bearer ${TOKEN}`, "?limit=1"), e);
  assert.equal((await res.json()).messages.length, 1);
});

test("a plain form post goes on to the thanks page, or gets a page naming what to fix", async () => {
  const e = env();
  let res = await worker.fetch(post(good, { json: false }), e);
  assert.equal(res.status, 303);
  assert.equal(res.headers.get("location"), "/thanks");
  res = await worker.fetch(post({ ...good, email: "nope" }, { json: false }), e);
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
    const res = await worker.fetch(post(fields), e);
    assert.equal(res.status, status, error);
    assert.deepEqual(await res.json(), { ok: false, error });
    assert.equal(messages(e.CONTACT).length, 0, error);
  }
});

test("a filled hidden field looks like success to the bot and stores nothing", async () => {
  const e = env();
  const res = await worker.fetch(post({ ...good, leave_empty: "http://spam.example" }), e);
  assert.equal(res.status, 200);
  assert.equal(e.CONTACT.store.size, 0);
});

test("one address gets ten messages an hour", async () => {
  const e = env();
  for (let i = 0; i < 10; i++) assert.equal((await worker.fetch(post(good), e)).status, 200);
  const res = await worker.fetch(post(good), e);
  assert.equal(res.status, 429);
  assert.deepEqual(await res.json(), { ok: false, error: "busy" });
  assert.equal((await worker.fetch(post(good, { ip: "198.51.100.1" }), e)).status, 200);
  assert.equal(messages(e.CONTACT).length, 11);
});

test("storage trouble answers 503 in our own words", async () => {
  for (const e of [env({ CONTACT: undefined }), env({ CONTACT: fakeKV(1000, ["put"]) }), env({ CONTACT: fakeKV(1000, ["get"]) })]) {
    const res = await worker.fetch(post(good), e);
    assert.equal(res.status, 503);
    assert.deepEqual(await res.json(), { ok: false, error: "unavailable" });
  }
  assert.equal((await worker.fetch(read(`Bearer ${TOKEN}`), env({ CONTACT: fakeKV(1000, ["list"]) }))).status, 503);
});

test("reading messages needs the bearer token and fails closed without one configured", async () => {
  const e = env();
  await worker.fetch(post(good), e);
  for (const auth of [undefined, "", "Bearer", "Bearer wrong", `Basic ${TOKEN}`, `Bearer ${TOKEN}x`]) {
    const res = await worker.fetch(read(auth), e);
    assert.equal(res.status, 401, String(auth));
    assert.equal(res.headers.get("www-authenticate"), "Bearer");
  }
  for (const token of [undefined, ""]) {
    assert.equal((await worker.fetch(read("Bearer "), env({ CONTACT_ADMIN_TOKEN: token }))).status, 401);
    assert.equal((await worker.fetch(read("Bearer undefined"), env({ CONTACT_ADMIN_TOKEN: token }))).status, 401);
  }
  const put = new Request("https://potluck.sh/api/contact", { method: "PUT" });
  assert.equal((await worker.fetch(put, e)).status, 405);
});

test("posts from another site, of no stated length, or too big are refused before the form is read", async () => {
  const e = env();
  let res = await worker.fetch(post(good, { headers: { origin: "https://evil.example" } }), e);
  assert.equal(res.status, 403);
  assert.deepEqual(await res.json(), { ok: false, error: "origin" });
  assert.equal((await worker.fetch(post(good, { headers: { origin: "https://potluck.sh" } }), e)).status, 200);
  res = await worker.fetch(post(good, { headers: { "content-length": "999999" } }), e);
  assert.equal(res.status, 413);
  const bare = new Request("https://potluck.sh/api/contact", { method: "POST", headers: { accept: "application/json" }, body: "name=x" });
  assert.equal((await worker.fetch(bare, e)).status, 411);
  assert.equal(messages(e.CONTACT).length, 1);
});

test("the founders read messages a page at a time, following next", async () => {
  const e = env({ CONTACT: fakeKV(2) });
  for (const name of ["a", "b", "c", "d", "e"]) await worker.fetch(post({ ...good, name }), e);
  await e.CONTACT.put("msg:0000000000000000:broken", "not json");
  const seen = [];
  let cursor = "", pages = 0;
  do {
    const res = await worker.fetch(read(`Bearer ${TOKEN}`, `?limit=2${cursor ? `&cursor=${cursor}` : ""}`), e);
    assert.equal(res.status, 200);
    const out = await res.json();
    seen.push(...out.messages.map((m) => m.name));
    cursor = out.next;
    pages++;
  } while (cursor && pages < 10);
  assert.deepEqual(seen.sort(), ["a", "b", "c", "d", "e"]);
  assert.equal(pages, 3);
  const res = await worker.fetch(read(`Bearer ${TOKEN}`, "?limit=500"), env());
  assert.equal(res.status, 200);
});
