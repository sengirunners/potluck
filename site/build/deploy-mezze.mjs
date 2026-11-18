// `npm run deploy:mezze`: mezze.sh's deploy, the same steps as build/deploy.mjs for its own Worker
// (mezze/wrangler.jsonc): check the committed build is current, remember the live sitemap, deploy, check the live
// site, and tell IndexNow what changed. Any failing step stops the run with its message.
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { SITE_DIR } from "./build.mjs";
import { drift, ORIGIN } from "./mezze.mjs";
import { MEZZE_SENT_FILE, notify, readSent, saveSent, TIMEOUT_MS } from "./notify.mjs";

const fail = (msg) => {
  console.error(`deploy:mezze: ${msg}`);
  process.exit(1);
};

const problems = drift();
if (problems.length) fail(`the committed build output is out of date (${problems.map((p) => `mezze/public/${p.path}: ${p.why}`).join("; ")}); run \`npm run build\` and commit it`);

const dirty = spawnSync("git", ["status", "--porcelain", "--", "mezze", "build", "src"], { cwd: SITE_DIR, encoding: "utf8" });
if (dirty.status !== 0) fail("could not run git status");
if (dirty.stdout.trim()) fail(`uncommitted changes would go live:\n${dirty.stdout}Commit them first.`);

let previous = readSent(MEZZE_SENT_FILE);
if (!previous) {
  try {
    const res = await fetch(`${ORIGIN}/sitemap.xml`, { redirect: "manual", signal: AbortSignal.timeout(TIMEOUT_MS) });
    if (res.status === 200) {
      previous = await res.text();
      saveSent(previous, MEZZE_SENT_FILE);
    }
  } catch {
    // Not live yet (the first deploy): every page is sent.
  }
}

const wrangler = spawnSync("npx", ["wrangler", "deploy", "-c", "mezze/wrangler.jsonc"], { cwd: SITE_DIR, stdio: "inherit" });
if (wrangler.status !== 0) fail("wrangler deploy failed");

const check = async (label, want, url, init) => {
  const res = await fetch(url, { redirect: "manual", signal: AbortSignal.timeout(TIMEOUT_MS), ...init });
  const ok = res.status === want;
  console.log(`${ok ? "ok  " : "FAIL"} ${label} ${res.status}${ok ? "" : ` (want ${want})`}`);
  return ok;
};
let ok = true;
for (const h of ["mezze.sh", "www.mezze.sh"]) ok = (await check(`${h} page`, 200, `https://${h}/`)) && ok;
for (const p of ["/favicon.ico", "/og/home.png", "/robots.txt", "/llms.txt"]) ok = (await check(p, 200, `${ORIGIN}${p}`)) && ok;
ok = (await check("contact read without a token", 401, `${ORIGIN}/api/contact`)) && ok;
if (!ok) fail("a post-deploy check failed (IndexNow was not told)");

try {
  const committed = readFileSync(join(SITE_DIR, "mezze/public/sitemap.xml"), "utf8");
  await notify({ committed, previous, origin: ORIGIN });
  saveSent(committed, MEZZE_SENT_FILE);
} catch (e) {
  fail(`${e.message} (the deploy itself went out)`);
}
