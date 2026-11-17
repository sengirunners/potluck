import assert from "node:assert/strict";
import { existsSync, readFileSync, statSync } from "node:fs";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { heroPage, HERO_PATH } from "../build/readme-hero.mjs";

// The README's hero picture is drawn from the home page (build/readme-hero.mjs). These hold the page it is drawn
// from; the picture itself is drawn by hand with `npm run readme-hero`.
const INDEX = readFileSync(new URL("../public/index.html", import.meta.url), "utf8");

test("the hero's page is the home page with the hero stylesheet, the logo and one line under the headline", () => {
  const page = heroPage(INDEX);
  assert.match(page, /<link rel="stylesheet" href="\/site\.css"><link rel="stylesheet" href="\/readme-hero\.css">/);
  assert.match(page, /<div class="rh-brand">.*Potluck<\/div><h1 id="headline">/);
  assert.match(page, /<p class="rh-sub">Free and open source · macOS and Linux<\/p>/);
  assert.doesNotMatch(page, /class="jump"/);
});

test("a home page without the text an edit hangs on is an error, not a picture with a piece missing", () => {
  const once = (at) => new RegExp(`must hold ${at.replace(/[.*+?^${}()|[\]\\/]/g, "\\$&")} exactly once`);
  for (const at of ['<link rel="stylesheet" href="/site.css">', '<h1 id="headline">', '<a class="jump" href="#install">Install</a>']) {
    assert.throws(() => heroPage(INDEX.replace(at, "")), once(at), `without ${at}`);
    assert.throws(() => heroPage(INDEX.replace(at, at + at)), once(at), `with ${at} twice`);
  }
});

// A picture the README shows must be in the repository: GitHub draws a broken image for a path that is not.
test("every picture the README shows from this repository is committed and not empty", () => {
  const readme = readFileSync(new URL("../../README.md", import.meta.url), "utf8");
  const local = [...readme.matchAll(/<img src="([^"]+)"/g)].map((m) => m[1]).filter((src) => !/^https?:/.test(src));
  assert.ok(local.some((src) => fileURLToPath(new URL(`../../${src}`, import.meta.url)) === HERO_PATH), "the README does not show the hero picture");
  for (const src of local) {
    const file = new URL(`../../${src}`, import.meta.url);
    assert.ok(existsSync(file) && statSync(file).size > 0, `${src} is missing or empty (the hero: \`npm run readme-hero\`)`);
  }
});
