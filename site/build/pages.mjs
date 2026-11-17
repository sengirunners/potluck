// The facts every generated file shares, and the one list of hand-written pages. A page's `lastmod` is the day its
// content last changed in a way a search engine should recrawl; it is committed here, never read from git or the clock.
// Blog posts are not listed here: each carries its own `date` and `updated` (content/blog/<slug>.md).

export const ORIGIN = "https://potluck.sh";
export const REPO = "https://github.com/sengirunners/potluck";
export const SENGI = "https://sengi.run";
export const INSTALL = "curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh";
export const OG_IMAGE = `${ORIGIN}/og.png`;

// IndexNow's key. It is public on purpose: the protocol has the site serve it at /<key>.txt to prove it owns the host.
export const INDEXNOW_KEY = "7b1a88c94e3d01e8f2150746ddb8a872";

export const HOME = {
  path: "/",
  lastmod: "2026-11-16",
  title: "Potluck: run coding agents and builds on the computers you already own",
  description:
    "Potluck pools your team's spare Macs and Linux boxes and runs coding agents and builds on them, each in a fresh VM or sandbox. Free and open source. Made by Sengi.",
  markdown: "content/home.md",
};

export const BLOG = {
  path: "/blog",
  title: "Potluck blog",
  description: "Guides and notes on running coding agents and builds on the machines you already own, from the people who make Potluck.",
};

/** Pages with a hand-written source, in sitemap order. */
export const PAGES = [HOME];
