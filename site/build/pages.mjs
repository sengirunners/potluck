// The facts every generated file shares, and the one list of hand-written pages. A page's `lastmod` is the day its
// content last changed in a way a search engine should recrawl; it is committed here, never read from git or the clock.
// Blog posts are not listed here: each carries its own `date` and `updated` (content/blog/<slug>.md).
import { INSTALLER_URL } from "../src/install.js";
import { MEZZE_URL } from "../src/mezze-redirect.js";

export const ORIGIN = "https://potluck.sh";
export const REPO = "https://github.com/sengirunners/potluck";
export const SENGI = "https://sengi.run";

// Mezze's own site, which every potluck.sh page links to: the address is the one /mezze redirects to, and each page
// names it in words that say what it is.
export { MEZZE_URL };
export const MEZZE_FOOTER_TEXT = "Mezze: mail and work items for coding agents";
export const MEZZE_NAV_TEXT = "Mezze for agents";

export const INSTALL = `curl -fsSL ${INSTALLER_URL} | sh`;

// The getting-started prompt a person pastes into their coding agent. One copy: the build writes it into the home page
// (between its agent-prompt markers) and a test holds content/home.md and content/pages/agent-sandbox.md to it. Every
// command in it is one the README's quick start documents.
export const AGENT_PROMPT = `Set up Potluck (https://github.com/sengirunners/potluck) on this machine so I can try it alone. Read https://potluck.sh/llms-full.txt first. Then:
1. Install: ${INSTALL} (if it says ~/.local/bin is not on PATH, run the line it prints).
2. Run \`potluck doctor\` and tell me what is missing. On a Mac that is usually Tart, Softnet and about 60 GB of free disk. Show me any sudo line it prints; do not run sudo yourself.
3. When doctor is clean: potluck create trial --via tunnel, then potluck host --reclaim-idle-s 0 (on a Mac the first run pulls about 27 GB).
4. potluck sandbox create, then potluck sandbox exec <id> -- uname -a, then potluck sandbox rm <id>.
Stop and ask me before anything that needs a password, a sign-in or deleting files. If a command fails, show me its exact output.`;

// IndexNow's key. It is public on purpose: the protocol has the site serve it at /<key>.txt to prove it owns the host.
export const INDEXNOW_KEY = "7b1a88c94e3d01e8f2150746ddb8a872";

// Google Search Console's ownership proof for the property https://potluck.sh/ (the shared growth service account
// verified it with the Site Verification API's META method). Public on purpose; removing it un-verifies the site.
export const GOOGLE_SITE_VERIFICATION = "VtxYvTdYwVWYZa_DXm-MVSC0k4lRAy65l4ujzYZXrcI";

// `headline` is the page's h1 (public/index.html), which its share image carries; `title` is the <title>.
export const HOME = {
  path: "/",
  lastmod: "2026-11-18",
  title: "Potluck: self-hosted GitHub Actions runners and AI agent sandboxes on your team's idle machines",
  headline: "Put your team's idle machines to work.",
  description:
    "Potluck puts your team's idle Macs and Linux boxes to work: self-hosted GitHub Actions runners and sandboxes for coding agents, each job in a fresh VM or sandbox. Free and open source (Apache-2.0), early.",
  markdown: "content/home.md",
};

export const BLOG = {
  path: "/blog",
  title: "Potluck blog",
  description: "Guides and notes on running GitHub Actions jobs and coding agents on your team's idle machines, from the people who make Potluck.",
};

/** Pages with a hand-written source, in sitemap order. The pages in content/pages/ follow them. */
export const PAGES = [HOME];

/** Page slugs content/pages/ may not take: hand-written files, generated ones, and the Worker's routes. */
export const RESERVED_SLUGS = ["index", "thanks", "join", "blog", "feed", "sitemap", "robots", "llms", "llms-full", "og", "fonts", "api", "home", "mezze"];
