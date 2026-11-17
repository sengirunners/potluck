// Headless Chrome for the pictures drawn by hand (`npm run readme-hero`, `npm run og-assets`): find the binary, run
// it with a time limit, and serve it pages and the site's own files on a loopback port. Never used by the build.
import { spawn, spawnSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { createServer } from "node:http";
import { extname, join, normalize, sep } from "node:path";
import { fileURLToPath } from "node:url";

export const SITE_DIR = fileURLToPath(new URL("..", import.meta.url));
export const PUBLIC_DIR = join(SITE_DIR, "public");

const TYPES = { ".html": "text/html; charset=utf-8", ".css": "text/css", ".js": "text/javascript", ".woff2": "font/woff2", ".png": "image/png", ".svg": "image/svg+xml", ".ico": "image/x-icon", ".webmanifest": "application/manifest+json" };

/**
 * Serves `pages` (path to [content type, body]) and the site's public files for everything else, on a free loopback
 * port. A request it cannot answer goes on server.missed: the picture would be missing whatever asked for it.
 */
export function serve(pages) {
  const server = createServer((req, res) => {
    const path = new URL(req.url, "http://x").pathname;
    const send = (type, body) => res.writeHead(200, { "content-type": type }).end(body);
    if (pages[path]) return send(...pages[path]);
    const file = join(PUBLIC_DIR, normalize(path));
    if (!file.startsWith(PUBLIC_DIR + sep) || !existsSync(file) || !TYPES[extname(file)]) {
      server.missed.push(path);
      return res.writeHead(404).end();
    }
    send(TYPES[extname(file)], readFileSync(file));
  });
  server.missed = [];
  return new Promise((resolve) => server.listen(0, "127.0.0.1", () => resolve(server)));
}

export const HTML = TYPES[".html"];
export const CSS = TYPES[".css"];

export function chrome(who) {
  const named = process.env.CHROME;
  if (named) return named;
  const mac = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
  if (existsSync(mac)) return mac;
  for (const name of ["google-chrome", "chromium", "chromium-browser"]) {
    if (spawnSync(name, ["--version"]).status === 0) return name;
  }
  throw new Error(`${who}: no Chrome found; set CHROME to its binary`);
}

/** Runs `cmd` without blocking this process (the page it loads may be served from here); resolves to its stdout. */
export function run(who, cmd, args) {
  return new Promise((resolve, reject) => {
    const child = spawn(cmd, args, { stdio: ["ignore", "pipe", "pipe"], timeout: 60_000 });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (d) => (stdout += d));
    child.stderr.on("data", (d) => (stderr += d));
    child.on("error", (e) => reject(new Error(`${who}: could not run ${cmd}: ${e.message}`)));
    child.on("close", (code, signal) => {
      if (code === 0) return resolve(stdout);
      reject(new Error(`${who}: ${cmd} ${signal ? `was stopped after a minute (${signal})` : `failed (exit ${code})`}:\n${stderr}`));
    });
  });
}
