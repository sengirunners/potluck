// What both sites' builds share (potluck.sh's in build.mjs, mezze.sh's in mezze.mjs): robots.txt, a sitemap's XML,
// and comparing or writing a build's output under a public directory. It imports neither build.
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { escapeHtml as esc } from "./markdown.mjs";
import { samePixels } from "./share.mjs";

// Bots that crawl for search and AI assistants, each allowed by name (a named group replaces the * group, so each
// repeats the one disallowed path).
export const BOTS = ["GPTBot", "OAI-SearchBot", "ChatGPT-User", "ClaudeBot", "Claude-SearchBot", "PerplexityBot", "Google-Extended", "Bingbot"];

/** robots.txt for the site at `origin`. */
export function robotsTxt(origin) {
  const group = (agent) => `User-agent: ${agent}\nAllow: /\nDisallow: /api/\n`;
  return `${["*", ...BOTS].map(group).join("\n")}\nSitemap: ${origin}/sitemap.xml\n`;
}

/** A sitemap of `entries` ({loc, lastmod}). */
export function urlsetXml(entries) {
  const urls = entries.map((e) => `  <url><loc>${esc(e.loc)}</loc><lastmod>${e.lastmod}</lastmod></url>`);
  return `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n${urls.join("\n")}\n</urlset>\n`;
}

/** True when what is on disk is what the build makes: the same text, or for a PNG the same pixels. */
export function same(file, made) {
  const disk = readFileSync(file);
  if (typeof made === "string") return disk.toString("utf8") === made;
  return file.endsWith(".png") ? samePixels(disk, made) : disk.equals(made);
}

/**
 * Differences between `files` (a build's output) and `publicDir`, where `owned` are the paths a build may delete:
 * [{path, why}]. Shared by potluck.sh's build and mezze.sh's (build/mezze.mjs).
 */
export function diffFiles(files, publicDir, owned) {
  const problems = [];
  for (const [path, text] of files) {
    const file = join(publicDir, path);
    if (!existsSync(file)) problems.push({ path, why: "missing" });
    else if (!same(file, text)) problems.push({ path, why: "differs" });
  }
  for (const path of owned) if (!files.has(path)) problems.push({ path, why: "stale: nothing builds it" });
  return problems;
}

/** Writes `files` under `publicDir` and deletes each of `owned` that nothing builds now. */
export function writeFiles(files, publicDir, owned) {
  for (const path of owned) if (!files.has(path)) rmSync(join(publicDir, path), { recursive: true, force: true });
  for (const [path, text] of files) {
    const file = join(publicDir, path);
    // A PNG with the same pixels is left alone, so a different zlib does not rewrite every picture.
    if (typeof text !== "string" && existsSync(file) && same(file, text)) continue;
    mkdirSync(dirname(file), { recursive: true });
    writeFileSync(file, text);
  }
}
