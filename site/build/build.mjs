// The site's build: Markdown posts in content/blog/ and pages in content/pages/ become pages under public/, with a
// share image for every page, a sitemap, feed, robots.txt, llms.txt and llms-full.txt. No dependencies (Node's
// standard library). The output is committed, so a deploy is
// still `wrangler deploy`; `npm test` fails when the committed output differs from a fresh build.
//
//   node build/build.mjs           write the output
//   node build/build.mjs --check   exit 1 if the committed output differs from a fresh build
//
// Generated: public/blog.html, public/blog/<slug>.html and .md, public/<page>.html, public/og/**.png,
// public/sitemap.xml, feed.xml, llms.txt, llms-full.txt, robots.txt, and the parts of public/index.html between its
// `seo:`, `blog-link:`, `mezze-nav:`, `mezze-footer:` and `agent-prompt:` markers. Everything else under public/ is hand-written (the icons and the template the
// share images are drawn on come from `npm run og-assets`).
import { existsSync, mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { articlePage, authorName, BLOG_LINK, FEED_LINK, homeGraph, indexPage, MEZZE_FOOTER_LINK, MEZZE_NAV_LINK, PAGE_MARKER_PREFIX, seoTags, shareImagePath, url } from "./layout.mjs";
import { dropTitleHeading, escapeHtml as esc, inline, MarkdownError, toHtml } from "./markdown.mjs";
import { parsePage, parsePost } from "./frontmatter.mjs";
import { AGENT_PROMPT, BLOG, GOOGLE_SITE_VERIFICATION, HOME, ORIGIN, PAGES, RESERVED_SLUGS } from "./pages.mjs";
import { shareImage } from "./share.mjs";
import { BOTS, diffFiles, robotsTxt as robotsFor, same, urlsetXml, writeFiles } from "./output.mjs";
import * as mezze from "./mezze.mjs";

export { BOTS, diffFiles, same, urlsetXml, writeFiles };
export const robotsTxt = () => robotsFor(ORIGIN);

export const SITE_DIR = join(dirname(fileURLToPath(import.meta.url)), "..");

/** Generated paths under public/ that exist only when there is something to say; stale copies are deleted. */
const GENERATED_ROOT_FILES = ["blog.html", "sitemap.xml", "feed.xml", "llms.txt", "llms-full.txt", "robots.txt"];
const GENERATED_DIRS = ["blog", "og"];


/**
 * Reads and checks every Markdown file in `dir` (`content/<kind>/`): each is <slug>.md, its front matter parsed by
 * `parse`, its body converted. A file that is not <slug>.md, or whose front matter or body fails, throws.
 */
function loadMarkdown(dir, rel, parse, what) {
  if (!existsSync(dir)) return [];
  const items = [];
  for (const name of readdirSync(dir).sort()) {
    if (name.startsWith(".")) continue;
    const m = name.match(/^([a-z0-9]+(?:-[a-z0-9]+)*)\.md$/);
    if (!m) throw new Error(`${dir}/${name}: ${what} are <slug>.md files (lowercase letters, digits, hyphens)`);
    const file = `${rel}/${name}`;
    const { meta, body } = parse(readFileSync(join(dir, name), "utf8"), file);
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
    items.push({ meta, file, text, html, summaryHtml });
  }
  return items;
}

/** Reads and checks every post in `dir`, newest published first. */
export function loadPosts(dir) {
  const posts = loadMarkdown(dir, "content/blog", parsePost, "posts").map((p) => {
    const path = `/blog/${p.meta.slug}`;
    const m = p.meta;
    const markdown = `# ${m.title}\n\nPublished ${m.date}${m.updated !== m.date ? `, updated ${m.updated}` : ""}. ${url(path)}\n\n${p.text.trim()}\n`;
    return { ...p, kind: "post", path, markdown };
  });
  // Newest published first; ties by slug so the order never depends on the file system.
  return posts.sort((a, b) => (a.meta.date < b.meta.date ? 1 : a.meta.date > b.meta.date ? -1 : a.meta.slug < b.meta.slug ? -1 : 1));
}

/** Reads and checks every page in `dir` (content/pages/<slug>.md, served at /<slug>), in slug order. */
export function loadPages(dir) {
  return loadMarkdown(dir, "content/pages", parsePage, "pages").map((p) => {
    if (RESERVED_SLUGS.includes(p.meta.slug)) throw new Error(`${p.file}: the slug ${p.meta.slug} is taken (build/pages.mjs RESERVED_SLUGS)`);
    const path = `/${p.meta.slug}`;
    const markdown = `# ${p.meta.title}\n\nUpdated ${p.meta.updated}. ${url(path)}\n\n${p.text.trim()}\n`;
    return { ...p, kind: "page", path, markdown };
  });
}

const maxDay = (days) => days.reduce((a, b) => (a > b ? a : b));

/** Every page that is public: {loc, lastmod}. The blog index is listed only when a post exists. */
export function sitemapEntries(posts, pages = []) {
  const entries = PAGES.map((p) => ({ loc: url(p.path), lastmod: p.lastmod }));
  for (const p of pages) entries.push({ loc: url(p.path), lastmod: p.meta.updated });
  if (posts.length) entries.push({ loc: url(BLOG.path), lastmod: maxDay(posts.map((p) => p.meta.updated)) });
  for (const p of posts) entries.push({ loc: url(p.path), lastmod: p.meta.updated });
  return entries;
}


export const sitemapXml = (posts, pages) => urlsetXml(sitemapEntries(posts, pages));

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
    <author><name>${esc(authorName(p.meta))}</name></author>
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

const linkText = (t) => t.replace(/[[\]]/g, "\\$&");

export function llmsTxt(siteDir, posts, contentPages = []) {
  const intro = read(siteDir, "content/llms-intro.md").trim();
  const pages = [
    ...PAGES.map((p) => `- [${linkText(p.title)}](${url(p.path)}): ${p.description}`),
    ...contentPages.map((p) => `- [${linkText(p.meta.title)}](${url(p.path)}): ${p.meta.page_description}`),
  ];
  const blog = posts.map((p) => `- [${linkText(p.meta.title)}](${url(p.path)}.md): ${p.meta.page_description}`);
  return `${intro}\n\n## Pages\n\n${pages.join("\n")}\n${posts.length ? `\n## Blog\n\n${blog.join("\n")}\n` : ""}\n## Full text\n\n- [llms-full.txt](${url("/llms-full.txt")}): the text of every page above in one file\n`;
}

export function llmsFullTxt(siteDir, posts, pages = []) {
  const intro = read(siteDir, "content/llms-intro.md").trim();
  const sections = [
    ...PAGES.map((p) => `Source: ${url(p.path)}\n\n${read(siteDir, p.markdown).trim()}`),
    ...pages.map((p) => `Source: ${url(p.path)}\n\n${p.markdown.trim()}`),
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
    image: HOME.headline,
    graph: homeGraph(),
    extra: [`<meta name="google-site-verification" content="${GOOGLE_SITE_VERIFICATION}">`, ...(posts.length ? [FEED_LINK] : [])],
  });
  region("seo:start", "seo:end", `\n${tags}\n`);
  region("blog-link:start", "blog-link:end", posts.length ? BLOG_LINK : "");
  region("mezze-nav:start", "mezze-nav:end", MEZZE_NAV_LINK);
  region("mezze-footer:start", "mezze-footer:end", MEZZE_FOOTER_LINK);
  region("agent-prompt:start", "agent-prompt:end", esc(AGENT_PROMPT));
  return source;
}

/**
 * The generated files as Map<path under public/, text or Buffer>, from the posts in `contentDir`, the pages in
 * `pagesDir` and the home page in `publicDir`.
 */
export function build({ siteDir = SITE_DIR, contentDir = join(siteDir, "content/blog"), pagesDir = join(siteDir, "content/pages"), publicDir = join(siteDir, "public") } = {}) {
  const posts = loadPosts(contentDir);
  const pages = loadPages(pagesDir);
  const home = readFileSync(join(publicDir, "index.html"), "utf8");
  const iconLine = home.match(/<link rel="icon" href="data:[^\n]*>/);
  if (!iconLine) throw new Error("public/index.html has no <link rel=\"icon\" href=\"data:...\"> line");
  for (const p of pages) {
    const file = join(publicDir, `${p.meta.slug}.html`);
    if (existsSync(file) && !readFileSync(file, "utf8").includes(PAGE_MARKER_PREFIX)) throw new Error(`${p.file}: public/${p.meta.slug}.html is a hand-written file; pick another slug`);
  }
  const out = new Map();
  const image = (path, title) => out.set(shareImagePath(path), shareImage({ title, address: url(path).replace(/^https:\/\//, "").replace(/\/$/, ""), where: path }));
  out.set("index.html", homePage(home, posts));
  image(HOME.path, HOME.headline);
  out.set("robots.txt", robotsTxt());
  out.set("sitemap.xml", sitemapXml(posts, pages));
  out.set("llms.txt", llmsTxt(siteDir, posts, pages));
  out.set("llms-full.txt", llmsFullTxt(siteDir, posts, pages));
  // Each article lists the pages, and a page also lists the posts, so every one is a link away from the rest.
  const pageLinks = pages.map((p) => ({ path: p.path, title: p.meta.title }));
  const postLinks = posts.map((p) => ({ path: p.path, title: p.meta.title }));
  for (const p of pages) {
    out.set(`${p.meta.slug}.html`, articlePage(p, iconLine[0], [...pageLinks, ...postLinks].filter((l) => l.path !== p.path)));
    image(p.path, p.meta.title);
  }
  if (posts.length) {
    out.set("feed.xml", feedXml(posts));
    out.set("blog.html", indexPage(posts, iconLine[0]));
    image(BLOG.path, BLOG.title);
    for (const p of posts) {
      out.set(`blog/${p.meta.slug}.html`, articlePage(p, iconLine[0], pageLinks));
      out.set(`blog/${p.meta.slug}.md`, p.markdown);
      image(p.path, p.meta.title);
    }
  }
  return { files: out, posts, pages };
}

const walk = (dir, base) => readdirSync(dir, { withFileTypes: true }).flatMap((e) => (e.isDirectory() ? walk(join(dir, e.name), `${base}/${e.name}`) : [`${base}/${e.name}`]));

/**
 * Paths under publicDir that a build owns and may delete: the generated files, everything in public/blog/ and
 * public/og/, and every top-level .html file that carries the page marker.
 */
export function ownedPaths(publicDir) {
  const owned = GENERATED_ROOT_FILES.filter((f) => existsSync(join(publicDir, f)));
  for (const d of GENERATED_DIRS) if (existsSync(join(publicDir, d))) owned.push(...walk(join(publicDir, d), d));
  for (const f of readdirSync(publicDir)) {
    if (f.endsWith(".html") && readFileSync(join(publicDir, f), "utf8").includes(PAGE_MARKER_PREFIX)) owned.push(f);
  }
  return owned;
}



/** Differences between a fresh build and what is on disk: [{path, why}]. Empty when the committed output is current. */
export function drift({ siteDir = SITE_DIR, ...opts } = {}) {
  const publicDir = opts.publicDir ?? join(siteDir, "public");
  return diffFiles(build({ siteDir, ...opts }).files, publicDir, ownedPaths(publicDir));
}

export function write({ siteDir = SITE_DIR, ...opts } = {}) {
  const publicDir = opts.publicDir ?? join(siteDir, "public");
  const { files, posts, pages } = build({ siteDir, ...opts });
  writeFiles(files, publicDir, ownedPaths(publicDir));
  return { written: [...files.keys()], posts: posts.length, pages: pages.length };
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  try {
    if (process.argv.includes("--check")) {
      const problems = [...drift().map((p) => ({ ...p, path: `public/${p.path}` })), ...mezze.drift().map((p) => ({ ...p, path: `mezze/public/${p.path}` }))];
      if (problems.length) {
        for (const p of problems) console.error(`${p.path}: ${p.why}`);
        console.error("The committed output is out of date: run `npm run build` and commit the result.");
        process.exit(1);
      }
      console.log("build output is current");
    } else {
      const { written, posts, pages } = write();
      console.log(`built ${posts} post${posts === 1 ? "" : "s"} and ${pages} page${pages === 1 ? "" : "s"}; wrote ${written.map((p) => relative(".", join("public", p))).join(", ")}`);
      console.log(`built mezze.sh; wrote ${mezze.write().map((p) => relative(".", join("mezze/public", p))).join(", ")}`);
    }
  } catch (e) {
    console.error(`build failed: ${e.message}`);
    process.exit(1);
  }
}
