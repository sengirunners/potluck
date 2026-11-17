// The site's build: Markdown posts in content/blog/ become pages under public/, with a sitemap, feed, robots.txt,
// llms.txt and llms-full.txt. No dependencies (Node's standard library). The output is committed, so a deploy is
// still `wrangler deploy`; `npm test` fails when the committed output differs from a fresh build.
//
//   node build/build.mjs           write the output
//   node build/build.mjs --check   exit 1 if the committed output differs from a fresh build
//
// Generated: public/blog.html, public/blog/<slug>.html and .md, public/sitemap.xml, feed.xml, llms.txt,
// llms-full.txt, robots.txt, and the parts of public/index.html between its `seo:` and `blog-link:` markers.
// Everything else under public/ is hand-written.
import { existsSync, mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { articlePage, BLOG_LINK, FEED_LINK, homeGraph, indexPage, seoTags, url } from "./layout.mjs";
import { dropTitleHeading, escapeHtml as esc, inline, MarkdownError, toHtml } from "./markdown.mjs";
import { parsePost } from "./frontmatter.mjs";
import { BLOG, HOME, ORIGIN, PAGES } from "./pages.mjs";

export const SITE_DIR = join(dirname(fileURLToPath(import.meta.url)), "..");

/** Generated paths under public/ that exist only when there is something to say; stale copies are deleted. */
const GENERATED_ROOT_FILES = ["blog.html", "sitemap.xml", "feed.xml", "llms.txt", "llms-full.txt", "robots.txt"];
const GENERATED_DIR = "blog";

// Bots that crawl for search and AI assistants, each allowed by name (a named group replaces the * group, so each
// repeats the one disallowed path).
export const BOTS = ["GPTBot", "OAI-SearchBot", "ChatGPT-User", "ClaudeBot", "Claude-SearchBot", "PerplexityBot", "Google-Extended", "Bingbot"];

export function robotsTxt() {
  const group = (agent) => `User-agent: ${agent}\nAllow: /\nDisallow: /api/\n`;
  return `${["*", ...BOTS].map(group).join("\n")}\nSitemap: ${ORIGIN}/sitemap.xml\n`;
}

/** Reads and checks every post in `dir`. A file that is not <slug>.md, or whose front matter or body fails, throws. */
export function loadPosts(dir) {
  if (!existsSync(dir)) return [];
  const posts = [];
  for (const name of readdirSync(dir).sort()) {
    if (name.startsWith(".")) continue;
    const m = name.match(/^([a-z0-9]+(?:-[a-z0-9]+)*)\.md$/);
    if (!m) throw new Error(`${dir}/${name}: posts are <slug>.md files (lowercase letters, digits, hyphens)`);
    const file = `content/blog/${name}`;
    const { meta, body } = parsePost(readFileSync(join(dir, name), "utf8"), file);
    if (meta.slug !== m[1]) throw new Error(`${file}: slug ${meta.slug} must match the file name`);
    const text = dropTitleHeading(body, meta.title);
    let html;
    let summaryHtml = "";
    try {
      html = toHtml(text);
      if (meta.summary) summaryHtml = `<p>${inline(meta.summary.replace(/\s+/g, " ").trim())}</p>`;
    } catch (e) {
      if (e instanceof MarkdownError) throw new Error(`${file}: ${e.message}`);
      throw e;
    }
    const path = `/blog/${meta.slug}`;
    const markdown = `# ${meta.title}\n\nPublished ${meta.date}${meta.updated !== meta.date ? `, updated ${meta.updated}` : ""}. ${url(path)}\n\n${text.trim()}\n`;
    posts.push({ meta, path, html, summaryHtml, markdown });
  }
  // Newest published first; ties by slug so the order never depends on the file system.
  return posts.sort((a, b) => (a.meta.date < b.meta.date ? 1 : a.meta.date > b.meta.date ? -1 : a.meta.slug < b.meta.slug ? -1 : 1));
}

const maxDay = (days) => days.reduce((a, b) => (a > b ? a : b));

/** Every page that is public: {loc, lastmod}. The blog index is listed only when a post exists. */
export function sitemapEntries(posts) {
  const entries = PAGES.map((p) => ({ loc: url(p.path), lastmod: p.lastmod }));
  if (posts.length) entries.push({ loc: url(BLOG.path), lastmod: maxDay(posts.map((p) => p.meta.updated)) });
  for (const p of posts) entries.push({ loc: url(p.path), lastmod: p.meta.updated });
  return entries;
}

export function sitemapXml(posts) {
  const urls = sitemapEntries(posts).map((e) => `  <url><loc>${esc(e.loc)}</loc><lastmod>${e.lastmod}</lastmod></url>`);
  return `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n${urls.join("\n")}\n</urlset>\n`;
}

export function feedXml(posts) {
  const byUpdated = [...posts].sort((a, b) => (a.meta.updated < b.meta.updated ? 1 : a.meta.updated > b.meta.updated ? -1 : a.meta.slug < b.meta.slug ? -1 : 1));
  const stamp = (d) => `${d}T00:00:00Z`;
  const entries = byUpdated.map(
    (p) => `  <entry>
    <id>${esc(url(p.path))}</id>
    <title>${esc(p.meta.title)}</title>
    <link rel="alternate" type="text/html" href="${esc(url(p.path))}"/>
    <published>${stamp(p.meta.date)}</published>
    <updated>${stamp(p.meta.updated)}</updated>
    <author><name>Sengi</name></author>
    <summary>${esc(p.meta.page_description)}</summary>
    <content type="html">${esc(p.html)}</content>
  </entry>`,
  );
  return `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <id>${url(BLOG.path)}</id>
  <title>${esc(BLOG.title)}</title>
  <subtitle>${esc(BLOG.description)}</subtitle>
  <link rel="alternate" type="text/html" href="${url(BLOG.path)}"/>
  <link rel="self" type="application/atom+xml" href="${url("/feed.xml")}"/>
  <updated>${stamp(maxDay(posts.map((p) => p.meta.updated)))}</updated>
  <author><name>Sengi</name></author>
${entries.join("\n")}
</feed>
`;
}

const read = (siteDir, rel) => readFileSync(join(siteDir, rel), "utf8");

export function llmsTxt(siteDir, posts) {
  const intro = read(siteDir, "content/llms-intro.md").trim();
  const pages = PAGES.map((p) => `- [${p.title}](${url(p.path)}): ${p.description}`);
  const blog = posts.map((p) => `- [${p.meta.title.replace(/[[\]]/g, "\\$&")}](${url(p.path)}.md): ${p.meta.page_description}`);
  return `${intro}\n\n## Pages\n\n${pages.join("\n")}\n${posts.length ? `\n## Blog\n\n${blog.join("\n")}\n` : ""}\n## Full text\n\n- [llms-full.txt](${url("/llms-full.txt")}): the text of every page above in one file\n`;
}

export function llmsFullTxt(siteDir, posts) {
  const intro = read(siteDir, "content/llms-intro.md").trim();
  const sections = [
    ...PAGES.map((p) => `Source: ${url(p.path)}\n\n${read(siteDir, p.markdown).trim()}`),
    ...posts.map((p) => `Source: ${url(p.path)}\n\n${p.markdown.trim()}`),
  ];
  return `${intro}\n\n${sections.map((s) => `---\n\n${s}`).join("\n\n")}\n`;
}

/** The home page with its generated regions rewritten. The rest of the file is hand-written and kept as is. */
export function homePage(source, posts) {
  const region = (start, end, inner) => {
    const re = new RegExp(`(<!-- ${start}[^>]*-->)[\\s\\S]*?(<!-- ${end} -->)`);
    if (!re.test(source)) throw new Error(`public/index.html has no <!-- ${start} --> ... <!-- ${end} --> markers`);
    source = source.replace(re, (_, a, b) => `${a}${inner}${b}`);
  };
  const tags = seoTags({
    title: HOME.title,
    description: HOME.description,
    path: HOME.path,
    graph: homeGraph(),
    extra: posts.length ? [FEED_LINK] : [],
  });
  region("seo:start", "seo:end", `\n${tags}\n`);
  region("blog-link:start", "blog-link:end", posts.length ? BLOG_LINK : "");
  return source;
}

/** The generated files as Map<path under public/, text>, from the posts in `contentDir` and the page in `publicDir`. */
export function build({ siteDir = SITE_DIR, contentDir = join(siteDir, "content/blog"), publicDir = join(siteDir, "public") } = {}) {
  const posts = loadPosts(contentDir);
  const home = readFileSync(join(publicDir, "index.html"), "utf8");
  const iconLine = home.match(/<link rel="icon"[^\n]*>/);
  if (!iconLine) throw new Error("public/index.html has no <link rel=\"icon\"> line");
  const out = new Map();
  out.set("index.html", homePage(home, posts));
  out.set("robots.txt", robotsTxt());
  out.set("sitemap.xml", sitemapXml(posts));
  out.set("llms.txt", llmsTxt(siteDir, posts));
  out.set("llms-full.txt", llmsFullTxt(siteDir, posts));
  if (posts.length) {
    out.set("feed.xml", feedXml(posts));
    out.set("blog.html", indexPage(posts, iconLine[0]));
    for (const p of posts) {
      out.set(`blog/${p.meta.slug}.html`, articlePage(p, p.html, iconLine[0]));
      out.set(`blog/${p.meta.slug}.md`, p.markdown);
    }
  }
  return { files: out, posts };
}

/** Paths under publicDir that a build owns and may delete: the generated files and everything in public/blog/. */
export function ownedPaths(publicDir) {
  const owned = GENERATED_ROOT_FILES.filter((f) => existsSync(join(publicDir, f)));
  const dir = join(publicDir, GENERATED_DIR);
  if (existsSync(dir)) for (const f of readdirSync(dir)) owned.push(`${GENERATED_DIR}/${f}`);
  return owned;
}

/** Differences between a fresh build and what is on disk: [{path, why}]. Empty when the committed output is current. */
export function drift({ siteDir = SITE_DIR, ...opts } = {}) {
  const publicDir = opts.publicDir ?? join(siteDir, "public");
  const { files } = build({ siteDir, ...opts });
  const problems = [];
  for (const [path, text] of files) {
    const file = join(publicDir, path);
    if (!existsSync(file)) problems.push({ path, why: "missing" });
    else if (readFileSync(file, "utf8") !== text) problems.push({ path, why: "differs" });
  }
  for (const path of ownedPaths(publicDir)) if (!files.has(path)) problems.push({ path, why: "stale: nothing builds it" });
  return problems;
}

export function write({ siteDir = SITE_DIR, ...opts } = {}) {
  const publicDir = opts.publicDir ?? join(siteDir, "public");
  const { files, posts } = build({ siteDir, ...opts });
  for (const path of ownedPaths(publicDir)) if (!files.has(path)) rmSync(join(publicDir, path), { recursive: true, force: true });
  for (const [path, text] of files) {
    mkdirSync(dirname(join(publicDir, path)), { recursive: true });
    writeFileSync(join(publicDir, path), text);
  }
  return { written: [...files.keys()], posts: posts.length };
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  try {
    if (process.argv.includes("--check")) {
      const problems = drift();
      if (problems.length) {
        for (const p of problems) console.error(`public/${p.path}: ${p.why}`);
        console.error("The committed output is out of date: run `npm run build` and commit the result.");
        process.exit(1);
      }
      console.log("build output is current");
    } else {
      const { written, posts } = write();
      console.log(`built ${posts} post${posts === 1 ? "" : "s"}; wrote ${written.map((p) => relative(".", join("public", p))).join(", ")}`);
    }
  } catch (e) {
    console.error(`build failed: ${e.message}`);
    process.exit(1);
  }
}
