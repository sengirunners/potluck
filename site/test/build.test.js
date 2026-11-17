import { test } from "node:test";
import assert from "node:assert/strict";
import { cpSync, existsSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { BOTS, build, drift, loadPages, loadPosts, SITE_DIR, sitemapEntries, write } from "../build/build.mjs";
import { GOOGLE_SITE_VERIFICATION, HOME, INDEXNOW_KEY, ORIGIN } from "../build/pages.mjs";
import { decodePng } from "../build/share.mjs";
import { isDay } from "../build/frontmatter.mjs";

const PUBLIC = join(SITE_DIR, "public");
const FIXTURES = join(SITE_DIR, "test/fixtures/blog");
const PAGE_FIXTURES = join(SITE_DIR, "test/fixtures/pages");
const SLUG = "run-a-coding-agent-in-a-sandbox";

/** A throwaway copy of public/ that a build with the fixture post can write into. */
function scratch(t, contentDir = FIXTURES, pagesDir = PAGE_FIXTURES) {
  const dir = mkdtempSync(join(tmpdir(), "potluck-site-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const publicDir = join(dir, "public");
  cpSync(PUBLIC, publicDir, { recursive: true });
  write({ contentDir, pagesDir, publicDir });
  return publicDir;
}

const emptyDir = (t) => {
  const dir = mkdtempSync(join(tmpdir(), "potluck-content-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  return dir;
};
const graphOf = (html) => JSON.parse(html.match(/<script type="application\/ld\+json">(.*?)<\/script>/)[1])["@graph"];

const walk = (dir, base = dir) => readdirSync(dir, { withFileTypes: true }).flatMap((e) => (e.isDirectory() ? walk(join(dir, e.name), base) : [join(dir, e.name).slice(base.length + 1)]));
const attr = (html, name) => [...html.matchAll(new RegExp(`\\s${name}="([^"]*)"`, "g"))].map((m) => m[1].replace(/&amp;/g, "&"));
const ids = (html) => new Set(attr(html, "id"));

test("the committed output matches a fresh build (run `npm run build` after changing content or templates)", () => {
  assert.deepEqual(drift(), []);
});

test("drift reports an edited, a missing and a stale generated file", async (t) => {
  const publicDir = scratch(t);
  const opts = { contentDir: FIXTURES, pagesDir: PAGE_FIXTURES, publicDir };
  assert.deepEqual(drift(opts), []);
  writeFileSync(join(publicDir, "sitemap.xml"), "edited");
  rmSync(join(publicDir, "llms.txt"));
  writeFileSync(join(publicDir, "blog/old-post.html"), "stale");
  // A page whose source is gone is the build's to delete; a hand-written page beside it is not.
  writeFileSync(join(publicDir, "old-page.html"), '<meta name="generator" content="potluck.sh build from content/pages/old-page.md">');
  writeFileSync(join(publicDir, "hand-written.html"), "<p>mine</p>");
  // A share image with other pixels differs; the same pixels compressed another way would not.
  const img = decodePng(readFileSync(join(publicDir, "og/a-test-page.png")));
  img.rgb[0] ^= 0xff;
  const { encodePng } = await import("../build/share.mjs");
  writeFileSync(join(publicDir, "og/a-test-page.png"), encodePng(img));
  const got = Object.fromEntries(drift(opts).map((p) => [p.path, p.why]));
  assert.deepEqual(got, {
    "sitemap.xml": "differs",
    "llms.txt": "missing",
    "og/a-test-page.png": "differs",
    "blog/old-post.html": "stale: nothing builds it",
    "old-page.html": "stale: nothing builds it",
  });
  write(opts);
  assert.equal(existsSync(join(publicDir, "old-page.html")), false);
  assert.equal(existsSync(join(publicDir, "hand-written.html")), true);
});

test("with no posts there is no blog page, feed or Blog link, and the sitemap lists the home page only", (t) => {
  const dir = emptyDir(t);
  const { files, posts } = build({ contentDir: dir, pagesDir: dir });
  assert.equal(posts.length, 0);
  assert.deepEqual([...files.keys()].sort(), ["index.html", "llms-full.txt", "llms.txt", "og/home.png", "robots.txt", "sitemap.xml"]);
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
  // The home page links the real pages and they link the real posts, so the scratch build keeps them all beside the
  // fixture post.
  const posts = emptyDir(t);
  cpSync(join(SITE_DIR, "content/blog"), posts, { recursive: true });
  cpSync(FIXTURES, posts, { recursive: true });
  for (const publicDir of [PUBLIC, scratch(t, posts, join(SITE_DIR, "content/pages"))]) {
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
  assert.match(short, /\[Potluck: self-hosted GitHub Actions runners[^\]]*\]\(https:\/\/potluck\.sh\/\)/);
  assert.match(short, /- \[A test page about runners\]\(https:\/\/potluck\.sh\/a-test-page\): A test page: /);
  assert.match(full, /Source: https:\/\/potluck\.sh\/a-test-page\n\n# A test page about runners\n\nUpdated 2026-11-14\. /);
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

test("the home page carries Google's site-verification meta tag, once", () => {
  const home = readFileSync(join(PUBLIC, "index.html"), "utf8");
  const tags = home.match(/<meta name="google-site-verification" content="([^"]+)">/g) ?? [];
  assert.deepEqual(tags, [`<meta name="google-site-verification" content="${GOOGLE_SITE_VERIFICATION}">`]);
});

test("a page in content/pages/ builds at /<slug> with the article layout, TechArticle and breadcrumb JSON-LD", (t) => {
  const publicDir = scratch(t);
  const page = readFileSync(join(publicDir, "a-test-page.html"), "utf8");
  assert.match(page, new RegExp(`<link rel="canonical" href="${ORIGIN}/a-test-page">`));
  assert.match(page, /<title>A test page about runners \| Potluck<\/title>/);
  assert.match(page, /<h1>A test page about runners<\/h1>/);
  assert.match(page, /<aside class="answer"/);
  assert.match(page, /Updated <time datetime="2026-11-14">November 14, 2026<\/time> · by Ana Example/);
  assert.doesNotMatch(page, /as Markdown/);
  const graph = graphOf(page);
  assert.deepEqual(graph.map((n) => n["@type"]), ["TechArticle", "BreadcrumbList"]);
  assert.deepEqual(graph[0].author, { "@type": "Person", name: "Ana Example" });
  assert.equal(graph[0].dateModified, "2026-11-14");
  assert.deepEqual(graph[1].itemListElement.map((i) => i.item), [`${ORIGIN}/`, `${ORIGIN}/a-test-page`]);
  assert.match(readFileSync(join(publicDir, "sitemap.xml"), "utf8"), /<loc>https:\/\/potluck\.sh\/a-test-page<\/loc><lastmod>2026-11-14<\/lastmod>/);
  // Pages and posts link to each other: a page lists the posts, a post lists the pages.
  assert.match(page, /<nav class="related"[\s\S]*href="\/blog\/run-a-coding-agent-in-a-sandbox"/);
  assert.match(readFileSync(join(publicDir, `blog/${SLUG}.html`), "utf8"), /<nav class="related"[\s\S]*href="\/a-test-page"/);
});

test("a page may not take a reserved slug or a hand-written file's name", (t) => {
  const dir = emptyDir(t);
  const text = readFileSync(join(PAGE_FIXTURES, "a-test-page.md"), "utf8");
  writeFileSync(join(dir, "blog.md"), text.replace("slug: a-test-page", "slug: blog"));
  assert.throws(() => loadPages(dir), /the slug blog is taken/);
  rmSync(join(dir, "blog.md"));
  writeFileSync(join(dir, "mine.md"), text.replace("slug: a-test-page", "slug: mine"));
  const publicDir = join(emptyDir(t), "public");
  cpSync(PUBLIC, publicDir, { recursive: true });
  writeFileSync(join(publicDir, "mine.html"), "<p>hand-written</p>");
  assert.throws(() => build({ contentDir: FIXTURES, pagesDir: dir, publicDir }), /public\/mine\.html is a hand-written file/);
  writeFileSync(join(dir, "mine.md"), text.replace("slug: a-test-page", "slug: mine").replace(/^target_searches:\n.*\n/m, ""));
  assert.throws(() => loadPages(dir), /missing required field target_searches/);
});

test("a post's author names a person in the byline, the feed and the JSON-LD; without one it is Sengi", (t) => {
  const dir = emptyDir(t);
  const text = readFileSync(join(FIXTURES, `${SLUG}.md`), "utf8");
  writeFileSync(join(dir, `${SLUG}.md`), text.replace("updated: 2026-11-15", "updated: 2026-11-15\nauthor: \"Jackson Cantrell\""));
  writeFileSync(join(dir, "other-post.md"), text.replace(`slug: ${SLUG}`, "slug: other-post"));
  const { files } = build({ contentDir: dir, pagesDir: emptyDir(t) });
  const page = files.get(`blog/${SLUG}.html`);
  assert.match(page, / · by Jackson Cantrell<\/p>/);
  assert.deepEqual(graphOf(page)[0].author, { "@type": "Person", name: "Jackson Cantrell" });
  assert.match(files.get("feed.xml"), /<id>https:\/\/potluck\.sh\/blog\/run-a-coding-agent-in-a-sandbox<\/id>[\s\S]*?<author><name>Jackson Cantrell<\/name><\/author>/);
  const other = files.get("blog/other-post.html");
  assert.match(other, / · by <a href="https:\/\/sengi\.run">Sengi<\/a><\/p>/);
  assert.deepEqual(graphOf(other)[0].author, { "@id": "https://sengi.run/#organization" });
});

test("every built page has its own share image, 1200x630, named in og:image and twitter:image", (t) => {
  for (const publicDir of [PUBLIC, scratch(t)]) {
    const seen = new Set();
    for (const f of walk(publicDir).filter((f) => f.endsWith(".html") && f !== "thanks.html")) {
      const html = readFileSync(join(publicDir, f), "utf8");
      const [og] = attr(html, "content").filter((c) => c.startsWith(`${ORIGIN}/og/`));
      assert.ok(og, `${f}: no share image`);
      assert.match(html, new RegExp(`<meta name="twitter:image" content="${og}">`));
      assert.ok(!seen.has(og), `${f}: shares ${og} with another page`);
      seen.add(og);
      const png = decodePng(readFileSync(join(publicDir, og.slice(ORIGIN.length + 1))));
      assert.deepEqual([png.width, png.height], [1200, 630], og);
    }
    assert.deepEqual(walk(join(publicDir, "og")).length, seen.size, "no share image without a page");
  }
});

test("a title with a character the share image cannot draw fails the build, naming it", (t) => {
  const dir = emptyDir(t);
  const text = readFileSync(join(PAGE_FIXTURES, "a-test-page.md"), "utf8");
  writeFileSync(join(dir, "a-test-page.md"), text.replace('title: "A test page about runners"', 'title: "A test page ✓"'));
  assert.throws(() => build({ contentDir: FIXTURES, pagesDir: dir }), /no glyph for "✓"; add it to CHARS/);
});

test("the home page's h1 is HOME.headline, which its share image carries", () => {
  const h1 = readFileSync(join(PUBLIC, "index.html"), "utf8").match(/<h1 id="headline">(.*?)<\/h1>/)[1].replace(/<[^>]+>/g, "");
  assert.equal(h1, HOME.headline);
});

test("the icons exist and every built page names them", () => {
  const ico = readFileSync(join(PUBLIC, "favicon.ico"));
  assert.deepEqual([ico.readUInt16LE(0), ico.readUInt16LE(2), ico.readUInt16LE(4)], [0, 1, 3], "an icon file with three pictures");
  for (let i = 0; i < 3; i++) {
    const size = ico[6 + 16 * i];
    const data = ico.subarray(ico.readUInt32LE(6 + 16 * i + 12), ico.readUInt32LE(6 + 16 * i + 12) + ico.readUInt32LE(6 + 16 * i + 8));
    assert.equal(data.readUInt32BE(16), size, `the ${size}px picture is ${size} wide`);
  }
  assert.equal(readFileSync(join(PUBLIC, "apple-touch-icon.png")).readUInt32BE(16), 180);
  const manifest = JSON.parse(readFileSync(join(PUBLIC, "site.webmanifest"), "utf8"));
  for (const icon of manifest.icons) {
    const png = readFileSync(join(PUBLIC, icon.src.slice(1)));
    assert.equal(`${png.readUInt32BE(16)}x${png.readUInt32BE(20)}`, icon.sizes, icon.src);
  }
  for (const f of walk(PUBLIC).filter((f) => f.endsWith(".html") && f !== "thanks.html")) {
    const html = readFileSync(join(PUBLIC, f), "utf8");
    for (const link of ['rel="icon" href="/favicon.ico"', 'rel="apple-touch-icon" href="/apple-touch-icon.png"', 'rel="manifest" href="/site.webmanifest"']) assert.ok(html.includes(link), `${f}: ${link}`);
  }
});

test("the home page's JSON-LD names the repository in sameAs and claims no version", () => {
  const graph = graphOf(readFileSync(join(PUBLIC, "index.html"), "utf8"));
  assert.deepEqual(graph[0].sameAs, ["https://github.com/sengirunners/potluck"]);
  assert.equal(graph[0].softwareVersion, undefined);
});

test("the getting-started prompt is one text: the home page, content/home.md and /agent-sandbox carry AGENT_PROMPT", async () => {
  const { AGENT_PROMPT, INSTALL } = await import("../build/pages.mjs");
  const unescape = (t) => t.replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&amp;/g, "&");
  const home = readFileSync(join(PUBLIC, "index.html"), "utf8").match(/<!-- agent-prompt:start[^>]*-->([\s\S]*?)<!-- agent-prompt:end -->/)[1];
  assert.equal(unescape(home), AGENT_PROMPT);
  const fence = (file) => readFileSync(join(SITE_DIR, file), "utf8").match(/```text\n([\s\S]*?)\n```/)[1];
  assert.equal(fence("content/home.md"), AGENT_PROMPT);
  assert.equal(fence("content/pages/agent-sandbox.md"), AGENT_PROMPT);
  assert.ok(AGENT_PROMPT.includes(INSTALL), "it installs with the README's command");
  assert.match(AGENT_PROMPT, /do not run sudo yourself/);
});
