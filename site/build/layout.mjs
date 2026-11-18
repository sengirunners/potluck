// HTML for the pages the build writes: the shared <head> tags (also written into the home page between its seo
// markers), the article (a blog post or a page from content/pages/), the blog index, and the JSON-LD.
import { BLOG, HOME, INSTALL, MEZZE_FOOTER_TEXT, MEZZE_NAV_TEXT, MEZZE_URL, ORIGIN, REPO, SENGI } from "./pages.mjs";
import { escapeHtml as esc } from "./markdown.mjs";

export const url = (path) => `${ORIGIN}${path}`;

/** JSON for a <script type="application/ld+json">: "<" is escaped so the data can never close the tag. */
export const ldScript = (data) => `<script type="application/ld+json">${JSON.stringify(data).replace(/</g, "\\u003c")}</script>`;

export const SENGI_ORG = { "@type": "Organization", "@id": `${SENGI}/#organization`, name: "Sengi", url: SENGI };

/** Where a page's share image is published: og/home.png, og/blog.png, og/blog/<slug>.png, og/<slug>.png. */
export function shareImagePath(path) {
  if (path === "/") return "og/home.png";
  return `og${path}.png`;
}

/** A post's or page's author in JSON-LD: the person its `author` names, else Sengi. */
const authorOf = (m) => (m.author ? { "@type": "Person", name: m.author } : { "@id": SENGI_ORG["@id"] });
/** The name a feed and a byline show. */
export const authorName = (m) => m.author ?? "Sengi";

/** The home page's structured data: the software, the company that makes it, and the site. */
export function homeGraph() {
  return {
    "@context": "https://schema.org",
    "@graph": [
      {
        "@type": ["SoftwareApplication", "SoftwareSourceCode"],
        "@id": `${ORIGIN}/#software`,
        name: "Potluck",
        url: `${ORIGIN}/`,
        description: HOME.description,
        applicationCategory: "DeveloperApplication",
        operatingSystem: "macOS, Linux",
        isAccessibleForFree: true,
        offers: { "@type": "Offer", price: "0", priceCurrency: "USD" },
        license: "https://www.apache.org/licenses/LICENSE-2.0",
        codeRepository: REPO,
        programmingLanguage: "Go",
        downloadUrl: `${REPO}/releases`,
        image: url(`/${shareImagePath("/")}`),
        sameAs: [REPO],
        author: { "@id": SENGI_ORG["@id"] },
        publisher: { "@id": SENGI_ORG["@id"] },
      },
      SENGI_ORG,
      {
        "@type": "WebSite",
        "@id": `${ORIGIN}/#website`,
        url: `${ORIGIN}/`,
        name: "Potluck",
        publisher: { "@id": SENGI_ORG["@id"] },
        // Mezze is a separate product, not another profile of Potluck, so it is mentioned (a CreativeWork property),
        // never listed in sameAs. Its @id is the one mezze.sh gives its own software node.
        mentions: { "@type": "SoftwareApplication", "@id": `${MEZZE_URL}#software`, name: "Mezze", url: MEZZE_URL },
      },
    ],
  };
}

const breadcrumb = (trail) => ({
  "@type": "BreadcrumbList",
  itemListElement: trail.map(([name, path], i) => ({ "@type": "ListItem", position: i + 1, name, item: url(path) })),
});

export function postGraph(post) {
  const m = post.meta;
  return {
    "@context": "https://schema.org",
    "@graph": [
      {
        "@type": "BlogPosting",
        "@id": `${url(post.path)}#post`,
        mainEntityOfPage: url(post.path),
        url: url(post.path),
        headline: m.title,
        description: m.page_description,
        datePublished: m.date,
        dateModified: m.updated,
        inLanguage: "en",
        keywords: m.target_searches.join(", "),
        image: url(`/${shareImagePath(post.path)}`),
        author: authorOf(m),
        publisher: SENGI_ORG,
        isPartOf: { "@type": "Blog", "@id": `${url(BLOG.path)}#blog`, name: BLOG.title, url: url(BLOG.path) },
      },
      breadcrumb([["Potluck", "/"], ["Blog", BLOG.path], [m.title, post.path]]),
    ],
  };
}

/** A page from content/pages/: a TechArticle, and its place under the home page. */
export function pageGraph(page) {
  const m = page.meta;
  return {
    "@context": "https://schema.org",
    "@graph": [
      {
        "@type": "TechArticle",
        "@id": `${url(page.path)}#article`,
        mainEntityOfPage: url(page.path),
        url: url(page.path),
        headline: m.title,
        description: m.page_description,
        datePublished: m.date,
        dateModified: m.updated,
        inLanguage: "en",
        keywords: m.target_searches.join(", "),
        image: url(`/${shareImagePath(page.path)}`),
        author: authorOf(m),
        publisher: SENGI_ORG,
        about: { "@id": `${ORIGIN}/#software` },
      },
      breadcrumb([["Potluck", "/"], [m.title, page.path]]),
    ],
  };
}

export function indexGraph(posts) {
  return {
    "@context": "https://schema.org",
    "@graph": [
      {
        "@type": "Blog",
        "@id": `${url(BLOG.path)}#blog`,
        url: url(BLOG.path),
        name: BLOG.title,
        description: BLOG.description,
        publisher: SENGI_ORG,
        blogPost: posts.map((p) => ({ "@type": "BlogPosting", url: url(p.path), headline: p.meta.title, datePublished: p.meta.date, dateModified: p.meta.updated })),
      },
      breadcrumb([["Potluck", "/"], ["Blog", BLOG.path]]),
    ],
  };
}

// The icons every page names besides the SVG one in its hand-written <head>: the .ico browsers ask for on their own,
// the home-screen icon, and the manifest (all drawn by `npm run og-assets`). The SVG stays on purpose: browsers that
// read it draw the tab icon sharp at any size, and the .ico serves the rest.
export const ICON_LINKS = [
  `<link rel="icon" href="/favicon.ico" sizes="16x16 32x32 48x48">`,
  `<link rel="apple-touch-icon" href="/apple-touch-icon.png">`,
  `<link rel="manifest" href="/site.webmanifest">`,
];

/**
 * Title, description, canonical, Open Graph, Twitter, icons and JSON-LD for one page. `image` is the share image's
 * alt text (the words drawn on it); `extra` is more <head> lines. `origin`, `siteName` and `imageSize` name the
 * site the page is on (potluck.sh unless given; build/mezze.mjs passes mezze.sh's).
 */
export function seoTags({ title, description, path, image, type = "website", graph, extra = [], article, origin = ORIGIN, siteName = "Potluck", imageSize = [1200, 630] }) {
  const at = (p) => `${origin}${p}`;
  const img = at(`/${shareImagePath(path)}`);
  return [
    `<title>${esc(title)}</title>`,
    `<meta name="description" content="${esc(description)}">`,
    `<link rel="canonical" href="${esc(at(path))}">`,
    `<meta property="og:site_name" content="${esc(siteName)}">`,
    `<meta property="og:type" content="${type}">`,
    `<meta property="og:title" content="${esc(title)}">`,
    `<meta property="og:description" content="${esc(description)}">`,
    `<meta property="og:url" content="${esc(at(path))}">`,
    `<meta property="og:image" content="${esc(img)}">`,
    `<meta property="og:image:width" content="${imageSize[0]}">`,
    `<meta property="og:image:height" content="${imageSize[1]}">`,
    `<meta property="og:image:alt" content="${esc(image)}">`,
    ...(article ? [`<meta property="article:published_time" content="${article.date}">`, `<meta property="article:modified_time" content="${article.updated}">`] : []),
    `<meta name="twitter:card" content="summary_large_image">`,
    `<meta name="twitter:title" content="${esc(title)}">`,
    `<meta name="twitter:description" content="${esc(description)}">`,
    `<meta name="twitter:image" content="${esc(img)}">`,
    ...ICON_LINKS,
    ...extra,
    ldScript(graph),
  ].join("\n");
}

export const FEED_LINK = `<link rel="alternate" type="application/atom+xml" title="Potluck blog" href="/feed.xml">`;

const longDay = (d) => new Date(`${d}T00:00:00Z`).toLocaleDateString("en-US", { year: "numeric", month: "long", day: "numeric", timeZone: "UTC" });

export const BLOG_LINK = `<a href="${BLOG.path}">Blog</a>`;
/** The links to mezze.sh in every page's header and footer; the home page's are written between its `mezze-nav` and `mezze-footer` markers. */
export const MEZZE_NAV_LINK = `<a class="sec mezze" href="${MEZZE_URL}">${MEZZE_NAV_TEXT}</a>`;
export const MEZZE_FOOTER_LINK = `<a href="${MEZZE_URL}">${MEZZE_FOOTER_TEXT}</a>`;

function footer(hasPosts) {
  return `<footer class="wrap">
  <span>Potluck is open source under Apache-2.0, made by <a href="${SENGI}">Sengi</a>. This site has no tracking.</span>
  <nav aria-label="Footer">
    <a href="${REPO}">GitHub</a>
    <a href="${REPO}/releases">Releases</a>
    ${hasPosts ? `${BLOG_LINK}\n    ` : ""}<a href="/#founders">Contact</a>
    ${MEZZE_FOOTER_LINK}
    <a href="${SENGI}">Sengi</a>
  </nav>
</footer>`;
}

const LOGO = `<a class="logo" href="/" aria-label="Potluck home">
    <svg viewBox="0 0 32 32" aria-hidden="true"><path d="M4 14h24a12 10 0 0 1-24 0z" fill="#C8402F"/><rect x="13" y="9" width="6" height="3" rx="1.5" fill="#2F6B34"/></svg>
    Potluck
  </a>`;

/** A whole built page (a post, a page or the blog index). `icon` is the favicon <link> line, taken from the home page so there is one copy. */
function shell({ head, main, icon }) {
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
${head}
<meta name="color-scheme" content="light">
${icon}
<link rel="preload" href="/fonts/newsreader-roman.woff2" as="font" type="font/woff2" crossorigin>
<link rel="stylesheet" href="/site.css">
<link rel="stylesheet" href="/blog.css">
</head>
<body class="blog">
<header class="masthead wrap">
  ${LOGO}
  <nav aria-label="Main">
    <a href="${BLOG.path}">Blog</a>
    ${MEZZE_NAV_LINK}
    <a class="sec" href="${REPO}">GitHub</a>
    <a class="btn line" href="/#install">Install</a>
  </nav>
</header>
${main}
${footer(true)}
</body>
</html>
`;
}

const cta = `<aside class="cta" aria-labelledby="cta-h">
  <h2 id="cta-h">Try Potluck on your own machines</h2>
  <p>Potluck is free and open source (Apache-2.0). Install it on macOS or Linux:</p>
  <pre><code>${esc(INSTALL)}</code></pre>
  <p class="actions"><a class="btn solid" href="${REPO}">Code on GitHub</a> <a class="btn line" href="/">What Potluck does</a></p>
</aside>`;

/** The marker a page built from content/pages/<slug>.md carries, which is how the build knows it may delete it. */
export const pageMarker = (slug) => `<meta name="generator" content="potluck.sh build from content/pages/${slug}.md">`;
export const PAGE_MARKER_PREFIX = `<meta name="generator" content="potluck.sh build from content/pages/`;

/** The guides list at the foot of an article: links to other pages and posts, so each one is reachable from the rest. */
function related(links) {
  if (!links.length) return "";
  return `<nav class="related" aria-labelledby="related-h">
  <h2 id="related-h">More from Potluck</h2>
  <ul>
${links.map((l) => `    <li><a href="${l.path}">${esc(l.title)}</a></li>`).join("\n")}
  </ul>
</nav>`;
}

/**
 * The article page for a post (`kind: "post"`, under /blog) or a page from content/pages/ (`kind: "page"`).
 * `item.html` is its body, already converted; `links` are the other articles the foot of the page lists.
 */
export function articlePage(item, icon, links = []) {
  const m = item.meta;
  const isPost = item.kind === "post";
  const head = seoTags({
    title: m.page_title,
    description: m.page_description,
    path: item.path,
    image: m.title,
    type: "article",
    article: m,
    graph: isPost ? postGraph(item) : pageGraph(item),
    extra: isPost ? [FEED_LINK, `<link rel="alternate" type="text/markdown" href="${item.path}.md">`] : [pageMarker(m.slug)],
  });
  const changed = m.updated !== m.date;
  const by = m.author ? esc(m.author) : `<a href="${SENGI}">Sengi</a>`;
  const crumbs = isPost
    ? `<a href="/">Potluck</a><span aria-hidden="true"> / </span><a href="${BLOG.path}">Blog</a>`
    : `<a href="/">Potluck</a>`;
  const dates = isPost
    ? `<time datetime="${m.date}">${longDay(m.date)}</time>${changed ? ` · updated <time datetime="${m.updated}">${longDay(m.updated)}</time>` : ""}`
    : `Updated <time datetime="${m.updated}">${longDay(m.updated)}</time>`;
  const main = `<main class="wrap">
<article class="post">
  <nav class="crumbs" aria-label="Breadcrumb">${crumbs}</nav>
  <h1>${esc(m.title)}</h1>
  <p class="meta">${dates} · by ${by}</p>
${item.summaryHtml ? `  <aside class="answer" aria-label="Short answer"><p class="kicker">Short answer</p>\n${item.summaryHtml}\n  </aside>\n` : ""}  <div class="prose">
${item.html}
  </div>
${cta}
${related(links)}
${isPost ? `  <p class="alt"><a href="${item.path}.md">This post as Markdown</a></p>\n` : ""}</article>
</main>`;
  return shell({ head, main, icon });
}

/** The blog index: every post, newest published first. */
export function indexPage(posts, icon) {
  const head = seoTags({ title: BLOG.title, description: BLOG.description, path: BLOG.path, image: BLOG.title, graph: indexGraph(posts), extra: [FEED_LINK] });
  const items = posts
    .map(
      (p) => `  <li>
    <h2><a href="${p.path}">${esc(p.meta.title)}</a></h2>
    <p class="meta"><time datetime="${p.meta.date}">${longDay(p.meta.date)}</time></p>
    <p>${esc(p.meta.page_description)}</p>
  </li>`,
    )
    .join("\n");
  const main = `<main class="wrap">
<section class="index">
  <nav class="crumbs" aria-label="Breadcrumb"><a href="/">Potluck</a><span aria-hidden="true"> / </span><span>Blog</span></nav>
  <h1>Potluck blog</h1>
  <p class="lede">${esc(BLOG.description)} <a href="/feed.xml">Atom feed</a></p>
  <ol class="posts">
${items}
  </ol>
${cta}
</section>
</main>`;
  return shell({ head, main, icon });
}
