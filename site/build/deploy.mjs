// `npm run deploy`: check the committed build is current, remember the sitemap that is live now, deploy, run the
// post-deploy checks, and tell IndexNow what changed. Any failing step stops the run with its message.
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { drift, SITE_DIR } from "./build.mjs";
import { notify, readSent, saveSent, SITEMAP_URL, TIMEOUT_MS } from "./notify.mjs";

const fail = (msg) => {
  console.error(`deploy: ${msg}`);
  process.exit(1);
};

const problems = drift();
if (problems.length) fail(`the committed build output is out of date (${problems.map((p) => `public/${p.path}: ${p.why}`).join("; ")}); run \`npm run build\` and commit it`);

// What the build checked is the working tree; what goes live must be what is committed.
const dirty = spawnSync("git", ["status", "--porcelain", "--", "public", "content", "build", "src", "wrangler.jsonc"], { cwd: SITE_DIR, encoding: "utf8" });
if (dirty.status !== 0) fail("could not run git status");
if (dirty.stdout.trim()) fail(`uncommitted changes would go live:\n${dirty.stdout}Commit them first.`);

// "Changed" is measured against what IndexNow was last told (.wrangler/indexnow-sent.xml). With no such file, the
// sitemap live now stands in for it, and is saved, so a notify that fails below can be retried with `npm run notify`.
let previous = readSent();
if (!previous) {
  try {
    const res = await fetch(SITEMAP_URL, { redirect: "manual", signal: AbortSignal.timeout(TIMEOUT_MS) });
    if (res.status === 200) {
      previous = await res.text();
      saveSent(previous);
    } else if (res.status !== 404) {
      console.warn(`deploy: the live sitemap answered ${res.status}; every page will be sent to IndexNow`);
    }
  } catch (e) {
    console.warn(`deploy: could not read the live sitemap (${e.message}); every page will be sent to IndexNow`);
  }
}

const wrangler = spawnSync("npx", ["wrangler", "deploy"], { cwd: SITE_DIR, stdio: "inherit" });
if (wrangler.status !== 0) fail("wrangler deploy failed");

// The README's checks: the page on both hosts, the closed waitlist, the favicon, the installer's redirect, and the two
// admin reads (when their tokens are set).
const check = async (label, want, url, init) => {
  const res = await fetch(url, { redirect: "manual", signal: AbortSignal.timeout(TIMEOUT_MS), ...init });
  const ok = res.status === want;
  console.log(`${ok ? "ok  " : "FAIL"} ${label} ${res.status}${ok ? "" : ` (want ${want})`}`);
  return ok;
};
let ok = true;
for (const h of ["potluck.sh", "www.potluck.sh"]) {
  ok = (await check(`${h} page`, 200, `https://${h}/`)) && ok;
  ok = (await check(`${h} waitlist post`, 410, `https://${h}/api/waitlist`, { method: "POST" })) && ok;
}
ok = (await check("favicon", 200, "https://potluck.sh/favicon.ico")) && ok;
ok = (await check("install.sh redirect", 302, "https://potluck.sh/install.sh")) && ok;
const auth = (token) => ({ headers: { authorization: `Bearer ${token}` } });
if (process.env.WAITLIST_ADMIN_TOKEN) ok = (await check("waitlist count", 200, "https://potluck.sh/api/waitlist/count", auth(process.env.WAITLIST_ADMIN_TOKEN))) && ok;
else console.log("skip waitlist count (WAITLIST_ADMIN_TOKEN not set)");
if (process.env.CONTACT_ADMIN_TOKEN) ok = (await check("contact read", 200, "https://potluck.sh/api/contact", auth(process.env.CONTACT_ADMIN_TOKEN))) && ok;
else console.log("skip contact read (CONTACT_ADMIN_TOKEN not set)");
if (!ok) fail("a post-deploy check failed (IndexNow was not told)");

try {
  const committed = readFileSync(join(SITE_DIR, "public/sitemap.xml"), "utf8");
  await notify({ committed, previous });
  saveSent(committed);
} catch (e) {
  fail(`${e.message} (the deploy itself went out; run \`npm run notify\` once it is fixed)`);
}
