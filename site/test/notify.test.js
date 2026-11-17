import { test } from "node:test";
import assert from "node:assert/strict";
import { changedUrls, KEY_URL, notify, parseSitemap, SITEMAP_URL, waitForSitemap, ENDPOINT } from "../build/notify.mjs";
import { INDEXNOW_KEY } from "../build/pages.mjs";

const sitemap = (rows) => `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n${rows.map(([l, d]) => `  <url><loc>${l}</loc><lastmod>${d}</lastmod></url>`).join("\n")}\n</urlset>\n`;
const A = ["https://potluck.sh/", "2026-11-16"];
const B = ["https://potluck.sh/blog", "2026-11-20"];
const C = ["https://potluck.sh/blog/post", "2026-11-20"];

const reply = (status, text = "") => ({ status, text: async () => text });

/** A fake network: `live` is what the sitemap URL serves (a list, one per call, the last repeating). */
function network({ live, pageStatus = {}, key = INDEXNOW_KEY, post = reply(200) }) {
  const calls = [];
  let n = 0;
  const fetchFn = async (url, init = {}) => {
    calls.push({ url, init });
    if (url === SITEMAP_URL) return reply(200, live[Math.min(n++, live.length - 1)]);
    if (url === KEY_URL) return reply(200, key);
    if (url === ENDPOINT) return post;
    return reply(pageStatus[url] ?? 200, "<html>");
  };
  return { fetchFn, calls };
}
const quiet = { log: () => {}, sleep: async () => {} };

test("parseSitemap and changedUrls compare address and lastmod only", () => {
  const prev = parseSitemap(sitemap([A, ["https://potluck.sh/blog/post", "2026-11-18"]]));
  const cur = parseSitemap(sitemap([A, B, C]));
  assert.deepEqual(changedUrls(prev, cur), [B[0], C[0]]);
  assert.deepEqual(changedUrls(cur, cur), []);
  assert.deepEqual(changedUrls(new Map(), cur), [A[0], B[0], C[0]]);
});

test("posts the changed addresses to IndexNow with the host, key and key location", async () => {
  const committed = sitemap([A, B, C]);
  const { fetchFn, calls } = network({ live: [committed] });
  const sent = await notify({ committed, previous: sitemap([A]), fetchFn, ...quiet });
  assert.deepEqual(sent, [B[0], C[0]]);
  const post = calls.find((c) => c.url === ENDPOINT);
  assert.equal(post.init.method, "POST");
  assert.deepEqual(JSON.parse(post.init.body), { host: "potluck.sh", key: INDEXNOW_KEY, keyLocation: KEY_URL, urlList: [B[0], C[0]] });
});

test("sends nothing when no address or lastmod changed", async () => {
  const committed = sitemap([A, B]);
  const { fetchFn, calls } = network({ live: [committed] });
  assert.deepEqual(await notify({ committed, previous: committed, fetchFn, ...quiet }), []);
  assert.equal(calls.length, 0);
});

test("waits for the live sitemap to match, then proceeds", async () => {
  const committed = sitemap([A, B]);
  const { fetchFn, calls } = network({ live: [sitemap([A]), sitemap([A]), committed] });
  let slept = 0;
  await notify({ committed, previous: sitemap([A]), fetchFn, log: () => {}, sleep: async () => { slept++; } });
  assert.equal(slept, 2);
  assert.ok(calls.some((c) => c.url === ENDPOINT));
});

test("gives up after a bounded number of tries and never posts", async () => {
  const committed = sitemap([A, B]);
  const { fetchFn, calls } = network({ live: [sitemap([A])] });
  await assert.rejects(waitForSitemap({ fetchFn, expected: committed, sleep: async () => {}, attempts: 4, log: () => {} }), /never matched/);
  assert.equal(calls.filter((c) => c.url === SITEMAP_URL).length, 4);
  await assert.rejects(notify({ committed, fetchFn, wait: { attempts: 2 }, ...quiet }), /never matched/);
  assert.ok(!calls.some((c) => c.url === ENDPOINT));
});

test("refuses to post when the key is wrong or a page redirects", async () => {
  const committed = sitemap([A, B]);
  let net = network({ live: [committed], key: "not-the-key" });
  await assert.rejects(notify({ committed, fetchFn: net.fetchFn, ...quiet }), /does not serve the IndexNow key/);
  net = network({ live: [committed], pageStatus: { [B[0]]: 307 } });
  await assert.rejects(notify({ committed, fetchFn: net.fetchFn, ...quiet }), /answered 307, not 200/);
  assert.ok(!net.calls.some((c) => c.url === ENDPOINT));
});

test("an IndexNow error fails; a dry run posts nothing", async () => {
  const committed = sitemap([A]);
  await assert.rejects(notify({ committed, fetchFn: network({ live: [committed], post: reply(403, "bad key") }).fetchFn, ...quiet }), /IndexNow answered 403/);
  const net = network({ live: [committed] });
  assert.deepEqual(await notify({ committed, fetchFn: net.fetchFn, dryRun: true, ...quiet }), [A[0]]);
  assert.ok(!net.calls.some((c) => c.url === ENDPOINT));
});

test("the last-sent sitemap is read back, and is empty when there is none", async () => {
  const { mkdtempSync, rmSync } = await import("node:fs");
  const { tmpdir } = await import("node:os");
  const { join } = await import("node:path");
  const { readSent, saveSent } = await import("../build/notify.mjs");
  const dir = mkdtempSync(join(tmpdir(), "potluck-sent-"));
  try {
    const file = join(dir, "nested/sent.xml");
    assert.equal(readSent(file), "");
    saveSent("<urlset/>", file);
    assert.equal(readSent(file), "<urlset/>");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
