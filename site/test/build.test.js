import { test } from "node:test";
import assert from "node:assert/strict";
import { cpSync, existsSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { BOTS, build, drift, loadPosts, SITE_DIR, sitemapEntries, write } from "../build/build.mjs";
import { INDEXNOW_KEY, ORIGIN } from "../build/pages.mjs";
import { isDay } from "../build/frontmatter.mjs";

const PUBLIC = join(SITE_DIR, "public");
const FIXTURES = join(SITE_DIR, "test/fixtures/blog");
const SLUG = "run-a-coding-agent-in-a-sandbox";

/** A throwaway copy of public/ that a build with the fixture post can write into. */
function scratch(t, contentDir = FIXTURES) {
  const dir = mkdtempSync(join(tmpdir(), "potluck-site-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const publicDir = join(dir, "public");
  cpSync(PUBLIC, publicDir, { recursive: true });
  write({ contentDir, publicDir });
  return publicDir;
}

const walk = (dir, base = dir) => readdirSync(dir, { withFileTypes: true }).flatMap((e) => (e.isDirectory() ? walk(join(dir, e.name), base) : [join(dir, e.name).slice(base.length + 1)]));
const attr = (html, name) => [...html.matchAll(new RegExp(`\\s${name}="([^"]*)"`, "g"))].map((m) => m[1].replace(/&amp;/g, "&"));
const ids = (html) => new Set(attr(html, "id"));

test("the committed output matches a fresh build (run `npm run build` after changing content or templates)", () => {
  assert.deepEqual(drift(), []);
});

test("drift reports an edited, a missing and a stale generated file", (t) => {
  const publicDir = scratch(t);
  assert.deepEqual(drift({ contentDir: FIXTURES, publicDir }), []);
  writeFileSync(join(publicDir, "sitemap.xml"), "edited");
  rmSync(join(publicDir, "llms.txt"));
  writeFileSync(join(publicDir, "blog/old-post.html"), "stale");
  const got = Object.fromEntries(drift({ contentDir: FIXTURES, publicDir }).map((p) => [p.path, p.why]));
  assert.deepEqual(got, { "sitemap.xml": "differs", "llms.txt": "missing", "blog/old-post.html": "stale: nothing builds it" });
});

test("with no posts there is no blog page, feed or Blog link, and the sitemap lists the home page only", (t) => {
  const dir = mkdtempSync(join(tmpdir(), "potluck-content-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const { files, posts } = build({ contentDir: dir });
  assert.equal(posts.length, 0);
  assert.deepEqual([...files.keys()].sort(), ["index.html", "llms-full.txt", "llms.txt", "robots.txt", "sitemap.xml"]);
  assert.deepEqual(sitemapEntries(posts).map((e) => e.loc), [`${ORIGIN}/`]);
  assert.doesNotMatch(files.get("index.html"), /href="\/blog"/);
  assert.doesNotMatch(files.get("llms.txt"), /## Blog/);
});

test("the fixture post builds every page, with canonical, Open Graph and JSON-LD", (t) => {
  const publicDir = scratch(t);
  for (const f of [`blog/${SLUG}.html`, `blog/${SLUG}.md`, "blog.html", "feed.xml"]) assert.ok(existsSync(join(publicDir, f)), f);
  const page = readFileSync(join(publicDir, `blog/${SLUG}.html`), "utf8");
  assert.match(page, new RegExp(`<link rel="canonical" href="${ORIGIN}/blog/${SLUG}">`));
  assert.match(page, /<title>Run a coding agent in a sandbox on a spare Mac \| Potluck<\/title>/);
  assert.match(page, /<h1>Run a coding agent in a sandbox on a spare Mac<\/h1>/);
  for (const p of ["og:title", "og:description", "og:url", "og:image", "og:type"]) assert.match(page, new RegExp(`property="${p}"`));
  assert.match(page, /name="twitter:card"/);
  assert.match(page, /<aside class="answer"/);
  assert.match(page, /github\.com\/sengirunners\/potluck/);
  const graph = JSON.parse(page.match(/<script type="application\/ld\+json">(.*?)<\/script>/)[1])["@graph"];
  const types = graph.map((n) => n["@type"]);
  assert.deepEqual(types, ["BlogPosting", "BreadcrumbList"]);
  assert.equal(graph[0].datePublished, "2026-11-12");
  assert.equal(graph[0].dateModified, "2026-11-15");
  assert.deepEqual(graph[1].itemListElement.map((i) => i.item), [`${ORIGIN}/`, `${ORIGIN}/blog`, `${ORIGIN}/blog/${SLUG}`]);
  const md = readFileSync(join(publicDir, `blog/${SLUG}.md`), "utf8");
  assert.match(md, /^# Run a coding agent in a sandbox on a spare Mac\n\nPublished 2026-11-12, updated 2026-11-15\. https:\/\/potluck\.sh\/blog\//);
});

test("the home page carries SoftwareApplication, Organization and WebSite JSON-LD, and gains a Blog link with a post", (t) => {
  const home = readFileSync(join(scratch(t), "index.html"), "utf8");
  const graph = JSON.parse(home.match(/<script type="application\/ld\+json">(.*?)<\/script>/)[1])["@graph"];
  assert.ok(graph[0]["@type"].includes("SoftwareApplication"));
  assert.equal(graph[0].offers.price, "0");
  assert.equal(graph[0].license, "https://www.apache.org/licenses/LICENSE-2.0");
  assert.equal(graph[0].codeRepository, "https://github.com/sengirunners/potluck");
  assert.deepEqual(graph.slice(1).map((n) => n["@type"]), ["Organization", "WebSite"]);
  assert.match(home, /<link rel="canonical" href="https:\/\/potluck\.sh\/">/);
  assert.match(home, /<!-- blog-link:start --><a href="\/blog">Blog<\/a><!-- blog-link:end -->/);
});

test("the sitemap lists exactly the public built pages, with real dates", (t) => {
  for (const publicDir of [PUBLIC, scratch(t)]) {
    const xml = readFileSync(join(publicDir, "sitemap.xml"), "utf8");
    const entries = [...xml.matchAll(/<url><loc>([^<]+)<\/loc><lastmod>([^<]+)<\/lastmod><\/url>/g)].map((m) => ({ loc: m[1], lastmod: m[2] }));
    for (const e of entries) assert.ok(isDay(e.lastmod), `${e.loc} lastmod ${e.lastmod}`);
    const built = walk(publicDir).filter((f) => f.endsWith(".html") && f !== "thanks.html").map((f) => (f === "index.html" ? "/" : `/${f.slice(0, -5)}`));
    assert.deepEqual(entries.map((e) => e.loc.slice(ORIGIN.length)).sort(), built.sort());
    assert.doesNotMatch(readFileSync(join(publicDir, "thanks.html"), "utf8"), /canonical/);
    assert.match(readFileSync(join(publicDir, "thanks.html"), "utf8"), /name="robots" content="noindex"/);
  }
});

test("the feed lists posts newest updated first", (t) => {
  const dir = mkdtempSync(join(tmpdir(), "potluck-content-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const text = readFileSync(join(FIXTURES, `${SLUG}.md`), "utf8");
  writeFileSync(join(dir, `${SLUG}.md`), text);
  writeFileSync(join(dir, "older-post.md"), text.replace(`slug: ${SLUG}`, "slug: older-post").replace("date: 2026-11-12", "date: 2026-11-01").replace("updated: 2026-11-15", "updated: 2026-11-20"));
  const { files } = build({ contentDir: dir });
  const feed = files.get("feed.xml");
  assert.ok(feed.indexOf("/blog/older-post<") < feed.indexOf(`/blog/${SLUG}<`), "updated 11-20 comes before updated 11-15");
  assert.match(feed, /<updated>2026-11-20T00:00:00Z<\/updated>\n {2}<author>/);
  assert.match(files.get("sitemap.xml"), /<loc>https:\/\/potluck\.sh\/blog<\/loc><lastmod>2026-11-20<\/lastmod>/);
  // The index lists by date published, newest first.
  const index = files.get("blog.html");
  assert.ok(index.indexOf(`/blog/${SLUG}"`) < index.indexOf('/blog/older-post"'));
});

test("every internal link and asset in a built page resolves to a built file or anchor", (t) => {
  for (const publicDir of [PUBLIC, scratch(t)]) {
    const pages = walk(publicDir).filter((f) => f.endsWith(".html"));
    const pageIds = new Map(pages.map((f) => [f, ids(readFileSync(join(publicDir, f), "utf8"))]));
    const target = (p) => {
      if (p === "/") return "index.html";
      const rel = p.slice(1);
      return [rel, `${rel}.html`].find((c) => existsSync(join(publicDir, c)) && statSync(join(publicDir, c)).isFile());
    };
    for (const f of pages) {
      const html = readFileSync(join(publicDir, f), "utf8");
      for (const ref of [...attr(html, "href"), ...attr(html, "src")]) {
        const href = ref.startsWith(ORIGIN) ? ref.slice(ORIGIN.length) || "/" : ref;
        if (!href.startsWith("/") && !href.startsWith("#")) continue; // off-site, data: URIs
        if (href.startsWith("//")) assert.fail(`${f}: protocol-relative link ${href}`);
        const [path, hash] = href.split("#");
        const file = path === "" ? f : target(path);
        assert.ok(file, `${f}: ${href} resolves to no built file`);
        if (hash && file.endsWith(".html")) assert.ok(pageIds.get(file).has(hash), `${f}: ${href} points at a missing anchor`);
      }
    }
  }
});

test("robots.txt allows every named bot, keeps /api/ closed and names the sitemap", () => {
  const robots = readFileSync(join(PUBLIC, "robots.txt"), "utf8");
  for (const bot of ["GPTBot", "OAI-SearchBot", "ChatGPT-User", "ClaudeBot", "Claude-SearchBot", "PerplexityBot", "Google-Extended", "Bingbot"]) {
    assert.match(robots, new RegExp(`User-agent: ${bot}\\nAllow: /\\nDisallow: /api/\\n`));
  }
  assert.equal(BOTS.length, 8);
  assert.match(robots, /^User-agent: \*\nAllow: \/\nDisallow: \/api\/\n/);
  assert.match(robots, /Sitemap: https:\/\/potluck\.sh\/sitemap\.xml\n$/);
});

test("llms.txt and llms-full.txt list the pages and carry their text", (t) => {
  const publicDir = scratch(t);
  const short = readFileSync(join(publicDir, "llms.txt"), "utf8");
  const full = readFileSync(join(publicDir, "llms-full.txt"), "utf8");
  assert.match(short, /^# Potluck\n\n> /);
  assert.match(short, /\[Potluck: run coding agents[^\]]*\]\(https:\/\/potluck\.sh\/\)/);
  assert.match(short, new RegExp(`\\(https://potluck\\.sh/blog/${SLUG}\\.md\\)`));
  assert.match(full, /Source: https:\/\/potluck\.sh\/\n/);
  assert.match(full, new RegExp(`Source: https://potluck\\.sh/blog/${SLUG}\\n\\n# Run a coding agent`));
  assert.match(full, /potluck sandbox exec <id> -- make test/);
});

test("a post outside the subset or with bad front matter fails the build loudly", (t) => {
  const dir = mkdtempSync(join(tmpdir(), "potluck-content-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const text = readFileSync(join(FIXTURES, `${SLUG}.md`), "utf8");
  writeFileSync(join(dir, `${SLUG}.md`), text + "\n<div>raw</div>\n");
  assert.throws(() => loadPosts(dir), /content\/blog\/run-a-coding.*raw HTML/);
  writeFileSync(join(dir, `${SLUG}.md`), text.replace(/^date: .*\n/m, ""));
  assert.throws(() => loadPosts(dir), /missing required field date/);
  writeFileSync(join(dir, `${SLUG}.md`), text.replace(`slug: ${SLUG}`, "slug: other"));
  assert.throws(() => loadPosts(dir), /slug other must match the file name/);
  rmSync(join(dir, `${SLUG}.md`));
  writeFileSync(join(dir, "Notes.txt"), "x");
  assert.throws(() => loadPosts(dir), /posts are <slug>\.md files/);
});

test("the IndexNow key file holds exactly the key, named for it", () => {
  assert.match(INDEXNOW_KEY, /^[0-9a-f]{32}$/);
  assert.equal(readFileSync(join(PUBLIC, `${INDEXNOW_KEY}.txt`), "utf8"), INDEXNOW_KEY);
  assert.deepEqual(walk(PUBLIC).filter((f) => /^[0-9a-f]{32}\.txt$/.test(f)), [`${INDEXNOW_KEY}.txt`]);
});

test("sitemap and canonical addresses use the form Workers assets answer with 200", (t) => {
  // Workers static assets answer /x for x.html with 200 and redirect /x.html and /x/ to /x (the README says how to
  // check that against `wrangler dev`). So the sitemap and canonical URLs have no .html and no trailing slash, except
  // the home page.
  const publicDir = scratch(t);
  for (const m of readFileSync(join(publicDir, "sitemap.xml"), "utf8").matchAll(/<loc>([^<]+)<\/loc>/g)) {
    assert.ok(m[1] === `${ORIGIN}/` || !/\.html$|\/$/.test(m[1]), m[1]);
  }
  for (const f of walk(publicDir).filter((f) => f.endsWith(".html"))) {
    const canon = readFileSync(join(publicDir, f), "utf8").match(/<link rel="canonical" href="([^"]+)">/)?.[1];
    if (f === "thanks.html") continue;
    assert.equal(canon, f === "index.html" ? `${ORIGIN}/` : `${ORIGIN}/${f.slice(0, -5)}`, f);
  }
});
