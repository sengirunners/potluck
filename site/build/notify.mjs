// After a deploy: tell IndexNow (Bing, and the engines that share it) which pages are new or changed.
//
//   node build/notify.mjs [--previous <file>] [--dry-run]
//
// "Before" is the sitemap IndexNow was last told about, kept in .wrangler/indexnow-sent.xml (not committed) and
// written after each successful send; `npm run deploy` seeds it from the live sitemap when the file is missing, so a
// notify that failed after a deploy can be retried and still sends the right pages. `--previous <file>` names another
// "before"; with neither, every page counts as new. The notifier first waits, a bounded time, for the live sitemap to equal the committed one (an edge
// can serve the previous build for a short while after a deploy), checks the IndexNow key and that each page to
// send answers 200 with no redirect, and only then posts. A page is "changed" when its address is new or its
// <lastmod> differs: nothing else is compared. The idea is Sengi's deploy/blog-discovery-notify.py.
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { INDEXNOW_KEY, ORIGIN } from "./pages.mjs";
import { SITE_DIR } from "./build.mjs";

export const ENDPOINT = "https://api.indexnow.org/indexnow";
// Each site serves the same key file and its own sitemap (mezze.sh passes its origin; potluck.sh's is the default).
const keyUrl = (origin) => `${origin}/${INDEXNOW_KEY}.txt`;
const sitemapUrl = (origin) => `${origin}/sitemap.xml`;
export const KEY_URL = keyUrl(ORIGIN);
export const SITEMAP_URL = sitemapUrl(ORIGIN);
export const WAIT = { attempts: 13, intervalMs: 10_000 }; // about two minutes
export const TIMEOUT_MS = 15_000; // for each request, so the wait above is bounded in time as well as in tries
export const SENT_FILE = join(SITE_DIR, ".wrangler/indexnow-sent.xml");
export const MEZZE_SENT_FILE = join(SITE_DIR, ".wrangler/indexnow-sent-mezze.xml");

/** The sitemap IndexNow was last told about, or "" when there is none. */
export const readSent = (file = SENT_FILE) => (existsSync(file) ? readFileSync(file, "utf8") : "");
export const saveSent = (xml, file = SENT_FILE) => {
  mkdirSync(dirname(file), { recursive: true });
  writeFileSync(file, xml);
};

/** {address: lastmod} from a sitemap this site's build wrote. */
export function parseSitemap(xml) {
  const pages = new Map();
  for (const m of String(xml).matchAll(/<url>\s*<loc>([^<]+)<\/loc>(?:\s*<lastmod>([^<]*)<\/lastmod>)?\s*<\/url>/g)) {
    pages.set(m[1].replace(/&amp;/g, "&"), m[2] ?? "");
  }
  return pages;
}

/** Addresses in `current` that are not in `previous` or whose lastmod differs. */
export const changedUrls = (previous, current) => [...current].filter(([loc, lastmod]) => previous.get(loc) !== lastmod).map(([loc]) => loc);

const get = async (fetchFn, url) => {
  const res = await fetchFn(url, { redirect: "manual", headers: { "user-agent": "potluck-site-notify/1" }, signal: AbortSignal.timeout(TIMEOUT_MS) });
  return { status: res.status, text: res.status === 200 ? await res.text() : "" };
};

/** Waits until the live sitemap equals `expected`; throws after `attempts` tries. */
export async function waitForSitemap({ fetchFn, expected, sleep, attempts = WAIT.attempts, intervalMs = WAIT.intervalMs, log = () => {}, origin = ORIGIN }) {
  const SITEMAP_URL = sitemapUrl(origin);
  let last = "";
  for (let i = 1; i <= attempts; i++) {
    try {
      const { status, text } = await get(fetchFn, SITEMAP_URL);
      if (status === 200 && text === expected) return;
      last = status === 200 ? "it differs from the committed sitemap (the previous build may still be served)" : `it answered ${status}`;
    } catch (e) {
      last = `it could not be fetched: ${e.message}`;
    }
    if (i < attempts) {
      log(`live sitemap not current yet (${last}); try ${i} of ${attempts}`);
      await sleep(intervalMs);
    }
  }
  throw new Error(`${SITEMAP_URL} never matched the committed one: ${last}`);
}

/** Notifies IndexNow of what changed. Returns the addresses sent (none: nothing was posted). */
export async function notify({ committed, previous = "", fetchFn = fetch, sleep = (ms) => new Promise((r) => setTimeout(r, ms)), dryRun = false, log = console.log, wait = {}, origin = ORIGIN }) {
  const KEY_URL = keyUrl(origin);
  const current = parseSitemap(committed);
  if (!current.size) throw new Error("the committed sitemap lists no pages");
  const urls = changedUrls(parseSitemap(previous), current);
  if (!urls.length) {
    log("no page is new or changed; nothing to send");
    return [];
  }
  await waitForSitemap({ fetchFn, expected: committed, sleep, log, origin, ...wait });
  const key = await get(fetchFn, KEY_URL);
  if (key.status !== 200 || key.text.trim() !== INDEXNOW_KEY) throw new Error(`${KEY_URL} does not serve the IndexNow key (status ${key.status})`);
  for (const url of urls) {
    const { status } = await get(fetchFn, url);
    if (status !== 200) throw new Error(`${url} answered ${status}, not 200: not sending it to IndexNow`);
  }
  if (dryRun) {
    log(`dry run: would send ${urls.length} address${urls.length === 1 ? "" : "es"}:\n${urls.join("\n")}`);
    return urls;
  }
  const res = await fetchFn(ENDPOINT, {
    method: "POST",
    headers: { "content-type": "application/json; charset=utf-8", "user-agent": "potluck-site-notify/1" },
    signal: AbortSignal.timeout(TIMEOUT_MS),
    body: JSON.stringify({ host: new URL(origin).host, key: INDEXNOW_KEY, keyLocation: KEY_URL, urlList: urls }),
  });
  if (![200, 202].includes(res.status)) throw new Error(`IndexNow answered ${res.status}: ${(await res.text()).slice(0, 200)}`);
  log(`IndexNow accepted ${urls.length} address${urls.length === 1 ? "" : "es"} (${res.status})`);
  return urls;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const args = process.argv.slice(2);
  const at = args.indexOf("--previous");
  try {
    const committed = readFileSync(join(SITE_DIR, "public/sitemap.xml"), "utf8");
    const dryRun = args.includes("--dry-run");
    await notify({ committed, previous: at >= 0 ? readFileSync(args[at + 1], "utf8") : readSent(), dryRun });
    if (!dryRun) saveSent(committed);
  } catch (e) {
    console.error(`notify failed: ${e.message}`);
    process.exit(1);
  }
}
