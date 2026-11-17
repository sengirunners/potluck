// HTML for the pages the build writes: the shared <head> tags (also written into the home page between its seo
// markers), the article, the blog index, and the JSON-LD.
import { BLOG, HOME, INSTALL, ORIGIN, OG_IMAGE, REPO, SENGI } from "./pages.mjs";
import { escapeHtml as esc } from "./markdown.mjs";

export const url = (path) => `${ORIGIN}${path}`;

/** JSON for a <script type="application/ld+json">: "<" is escaped so the data can never close the tag. */
export const ldScript = (data) => `<script type="application/ld+json">${JSON.stringify(data).replace(/</g, "\\u003c")}</script>`;

const SENGI_ORG = { "@type": "Organization", "@id": `${SENGI}/#organization`, name: "Sengi", url: SENGI };

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
        author: { "@id": SENGI_ORG["@id"] },
        publisher: { "@id": SENGI_ORG["@id"] },
      },
      SENGI_ORG,
      { "@type": "WebSite", "@id": `${ORIGIN}/#website`, url: `${ORIGIN}/`, name: "Potluck", publisher: { "@id": SENGI_ORG["@id"] } },
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
        image: OG_IMAGE,
        author: { "@id": SENGI_ORG["@id"] },
        publisher: SENGI_ORG,
        isPartOf: { "@type": "Blog", "@id": `${url(BLOG.path)}#blog`, name: BLOG.title, url: url(BLOG.path) },
      },
      breadcrumb([["Potluck", "/"], ["Blog", BLOG.path], [m.title, post.path]]),
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

/** Title, description, canonical, Open Graph, Twitter and JSON-LD for one page. `extra` is more <head> lines. */
export function seoTags({ title, description, path, type = "website", graph, extra = [], article }) {
  return [
    `<title>${esc(title)}</title>`,
    `<meta name="description" content="${esc(description)}">`,
    `<link rel="canonical" href="${esc(url(path))}">`,
    `<meta property="og:site_name" content="Potluck">`,
    `<meta property="og:type" content="${type}">`,
    `<meta property="og:title" content="${esc(title)}">`,
    `<meta property="og:description" content="${esc(description)}">`,
    `<meta property="og:url" content="${esc(url(path))}">`,
    `<meta property="og:image" content="${OG_IMAGE}">`,
    `<meta property="og:image:width" content="1200">`,
    `<meta property="og:image:height" content="630">`,
    `<meta property="og:image:alt" content="Potluck: run coding agents and builds on the computers you already own">`,
    ...(article ? [`<meta property="article:published_time" content="${article.date}">`, `<meta property="article:modified_time" content="${article.updated}">`] : []),
    `<meta name="twitter:card" content="summary_large_image">`,
    `<meta name="twitter:title" content="${esc(title)}">`,
    `<meta name="twitter:description" content="${esc(description)}">`,
    `<meta name="twitter:image" content="${OG_IMAGE}">`,
    ...extra,
    ldScript(graph),
  ].join("\n");
}

export const FEED_LINK = `<link rel="alternate" type="application/atom+xml" title="Potluck blog" href="/feed.xml">`;

const longDay = (d) => new Date(`${d}T00:00:00Z`).toLocaleDateString("en-US", { year: "numeric", month: "long", day: "numeric", timeZone: "UTC" });

export const BLOG_LINK = `<a href="${BLOG.path}">Blog</a>`;

function footer(hasPosts) {
  return `<footer class="wrap">
  <span>Potluck is open source under Apache-2.0, made by <a href="${SENGI}">Sengi</a>. This site has no tracking.</span>
  <nav aria-label="Footer">
    <a href="${REPO}">GitHub</a>
    <a href="${REPO}/releases">Releases</a>
    ${hasPosts ? `${BLOG_LINK}\n    ` : ""}<a href="/#founders">Contact</a>
    <a href="${SENGI}">Sengi</a>
  </nav>
</footer>`;
}

const LOGO = `<a class="logo" href="/" aria-label="Potluck home">
    <svg viewBox="0 0 32 32" aria-hidden="true"><path d="M4 14h24a12 10 0 0 1-24 0z" fill="#C8402F"/><rect x="13" y="9" width="6" height="3" rx="1.5" fill="#2F6B34"/></svg>
    Potluck
  </a>`;

/** A whole blog page. `icon` is the favicon <link> line, taken from the home page so there is one copy. */
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

/** The article page for one post. `html` is its body, already converted. */
export function articlePage(post, html, icon) {
  const m = post.meta;
  const head = seoTags({
    title: m.page_title,
    description: m.page_description,
    path: post.path,
    type: "article",
    article: m,
    graph: postGraph(post),
    extra: [FEED_LINK, `<link rel="alternate" type="text/markdown" href="${post.path}.md">`],
  });
  const changed = m.updated !== m.date;
  const main = `<main class="wrap">
<article class="post">
  <nav class="crumbs" aria-label="Breadcrumb"><a href="/">Potluck</a><span aria-hidden="true"> / </span><a href="${BLOG.path}">Blog</a></nav>
  <h1>${esc(m.title)}</h1>
  <p class="meta"><time datetime="${m.date}">${longDay(m.date)}</time>${changed ? ` · updated <time datetime="${m.updated}">${longDay(m.updated)}</time>` : ""} · by <a href="${SENGI}">Sengi</a></p>
${post.summaryHtml ? `  <aside class="answer" aria-label="Short answer"><p class="kicker">Short answer</p>\n${post.summaryHtml}\n  </aside>\n` : ""}  <div class="prose">
${html}
  </div>
${cta}
  <p class="alt"><a href="${post.path}.md">This post as Markdown</a></p>
</article>
</main>`;
  return shell({ head, main, icon });
}

/** The blog index: every post, newest published first. */
export function indexPage(posts, icon) {
  const head = seoTags({ title: BLOG.title, description: BLOG.description, path: BLOG.path, graph: indexGraph(posts), extra: [FEED_LINK] });
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
