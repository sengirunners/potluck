// mezze.sh: Mezze's own site, a second content root built by the same no-dependency build as potluck.sh and served
// by its own Worker (mezze/wrangler.jsonc, src/mezze.js). One hand-written page for now, mezze/public/index.html;
// the build writes its <head> tags between the seo markers, robots.txt, sitemap.xml, llms.txt, llms-full.txt, and
// copies the font from public/fonts so there is one copy of it in the repository.
//
// Hand-written under mezze/public/: index.html (outside its markers), site.css, scenes.js, thanks.html,
// site.webmanifest, img/robot.webp and og/home.png (from Mezze's private design source, its social-preview assets, the image Jackson picked), the
// icons (`npm run og-assets` draws them from the olive below) and the IndexNow key file. Drawn by hand-run tools and
// committed: 3d/robot.bin (build/mezze-robot.py) and img/props/ (build/mezze-props.mjs).
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { diffFiles, robotsTxt, urlsetXml, writeFiles } from "./output.mjs";
import { fileURLToPath } from "node:url";
import { SENGI_ORG, seoTags } from "./layout.mjs";

export const ORIGIN = "https://mezze.sh";
const SITE_DIR = fileURLToPath(new URL("..", import.meta.url));
export const MEZZE_DIR = join(SITE_DIR, "mezze");
export const PUBLIC_DIR = join(MEZZE_DIR, "public");

// The olive with its pimento: Mezze's mark (the design source's social-preview assets, `mark`), on a 32-unit square. The page's
// header, its SVG favicon and the drawn icons all use it.
export const OLIVE_SVG = `<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'><g transform='rotate(-24 16 16)'><ellipse cx='16' cy='16.8' rx='11.4' ry='8.4' fill='#5E7A2C'/><ellipse cx='15.5' cy='16' rx='11.1' ry='7.7' fill='#7C9A3D'/><ellipse cx='12' cy='12.5' rx='3.5' ry='1.4' fill='#B9CF7A' opacity='0.85'/><ellipse cx='22.8' cy='16.3' rx='3.5' ry='4' fill='#C8402F'/><ellipse cx='22.2' cy='15.2' rx='1.4' ry='1.1' fill='#E8735F'/></g></svg>`;

// The share image is the picture Jackson picked (1280x640), not one the build draws.
export const SHARE_SIZE = [1280, 640];

export const HOME = {
  path: "/",
  lastmod: "2026-11-18",
  title: "Mezze: mail, work items with leases and phone approvals for teams of coding agents",
  headline: "Run a brigade of coding agents without losing track of them.",
  description:
    "Mezze is one local daemon and one command, zm: mail between agents and people, work items whose claims expire, and approvals from your phone, for Claude Code and Codex on every machine you run. Apache-2.0.",
  imageAlt: "Mezze: Direct every agent, on every machine, from any app. A robot spins six plates of tapas labelled with what Mezze carries.",
  markdown: "mezze/content/home.md",
};

/** The home page's structured data: the software, the company that makes it, and the site. */
export function homeGraph() {
  return {
    "@context": "https://schema.org",
    "@graph": [
      {
        "@type": "SoftwareApplication",
        "@id": `${ORIGIN}/#software`,
        name: "Mezze",
        alternateName: "zm",
        url: `${ORIGIN}/`,
        description: HOME.description,
        applicationCategory: "DeveloperApplication",
        operatingSystem: "macOS, Linux",
        license: "https://www.apache.org/licenses/LICENSE-2.0",
        image: `${ORIGIN}/og/home.png`,
        author: { "@id": SENGI_ORG["@id"] },
        publisher: { "@id": SENGI_ORG["@id"] },
      },
      SENGI_ORG,
      { "@type": "WebSite", "@id": `${ORIGIN}/#website`, url: `${ORIGIN}/`, name: "Mezze", publisher: { "@id": SENGI_ORG["@id"] } },
    ],
  };
}

const read = (rel) => readFileSync(join(SITE_DIR, rel), "utf8");

/** The home page with its seo region rewritten. */
export function homePage(source) {
  const re = /(<!-- seo:start[^>]*-->)[\s\S]*?(<!-- seo:end -->)/;
  if (!re.test(source)) throw new Error("mezze/public/index.html has no <!-- seo:start --> ... <!-- seo:end --> markers");
  const tags = seoTags({ title: HOME.title, description: HOME.description, path: HOME.path, image: HOME.imageAlt, graph: homeGraph(), origin: ORIGIN, siteName: "Mezze", imageSize: SHARE_SIZE });
  return source.replace(re, (_, a, b) => `${a}\n${tags}\n${b}`);
}

export function llmsTxt() {
  return `${read("mezze/content/llms-intro.md").trim()}\n\n## Pages\n\n- [${HOME.title}](${ORIGIN}/): ${HOME.description}\n\n## Full text\n\n- [llms-full.txt](${ORIGIN}/llms-full.txt): the text of every page above in one file\n`;
}

export function llmsFullTxt() {
  return `${read("mezze/content/llms-intro.md").trim()}\n\n---\n\nSource: ${ORIGIN}/\n\n${read(HOME.markdown).trim()}\n`;
}

const FONTS = ["fonts/newsreader-roman.woff2", "fonts/newsreader-italic.woff2", "fonts/OFL-Newsreader.txt"];

/** Every file the build writes under mezze/public/, as Map<path, text or Buffer>. */
export function build() {
  const out = new Map();
  out.set("index.html", homePage(readFileSync(join(PUBLIC_DIR, "index.html"), "utf8")));
  out.set("robots.txt", robotsTxt(ORIGIN));
  out.set("sitemap.xml", urlsetXml([{ loc: `${ORIGIN}/`, lastmod: HOME.lastmod }]));
  out.set("llms.txt", llmsTxt());
  out.set("llms-full.txt", llmsFullTxt());
  for (const f of FONTS) out.set(f, readFileSync(join(SITE_DIR, "public", f)));
  return out;
}

// Every file this build writes is written on every build, so there is nothing stale to delete yet; when mezze.sh gets
// Markdown pages, list what the build owns here as build.mjs's ownedPaths does.
export const drift = () => diffFiles(build(), PUBLIC_DIR, []);

export function write() {
  const files = build();
  writeFiles(files, PUBLIC_DIR, []);
  return [...files.keys()];
}
