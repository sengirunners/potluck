import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { FrontMatterError, isDay, parsePost } from "../build/frontmatter.mjs";

const GOOD = {
  slug: "a-post", kind: "new", engine: "claude", title: '"A post"', page_title: '"A post | Potluck"', page_description: '"About a post."',
  model: "claude-opus-5-5", date: "2026-11-12", updated: "2026-11-15",
};
const lists = "target_searches:\n  - \"a search\" # 120/month\n  - another search\ndata_sources:\n  - \"a source\"\n";
const file = (over = {}, drop = [], extra = "") =>
  `---\n${Object.entries({ ...GOOD, ...over }).filter(([k]) => !drop.includes(k)).map(([k, v]) => `${k}: ${v}`).join("\n")}\n${lists}${extra}---\n\n## Body\n\ntext\n`;

test("reads what the content job writes, plus date and updated", () => {
  const { meta, body } = parsePost(file({ title: '"Say \\"hi\\": a title"' }, [], "drafted: 2026-11-10\nsummary: \"Short.\"\n"), "p.md");
  assert.equal(meta.title, 'Say "hi": a title');
  assert.deepEqual(meta.target_searches, ["a search", "another search"]);
  assert.deepEqual(meta.data_sources, ["a source"]);
  assert.equal(meta.drafted, "2026-11-10");
  assert.equal(body, "## Body\n\ntext\n");
});

test("a missing required field fails and names the file and the field", () => {
  for (const k of ["slug", "kind", "engine", "title", "page_title", "page_description", "model", "date", "updated"]) {
    assert.throws(() => parsePost(file({}, [k]), "p.md"), new RegExp(`p\\.md: missing required field ${k}`));
  }
  assert.throws(() => parsePost(file().replace(/target_searches:[\s\S]*data_sources:/, "data_sources:"), "p.md"), /missing required field target_searches/);
  assert.throws(() => parsePost(file().replace("  - \"a source\"\n", ""), "p.md"), /data_sources must list at least one item/);
});

test("malformed fields fail", () => {
  const bad = (over, re, extra) => assert.throws(() => parsePost(file(over, [], extra), "p.md"), (e) => e instanceof FrontMatterError && re.test(e.message));
  bad({ slug: "Bad Slug" }, /slug/);
  bad({ kind: "draft" }, /kind must be/);
  bad({ engine: "gemini" }, /engine must be/);
  bad({ date: "2026-02-30" }, /date must be a real/);
  bad({ updated: "2026-11-01" }, /updated is before date/);
  bad({ date: "11/12/2026" }, /date must be/);
  bad({ title: "" }, /title is empty/);
  bad({ page_title: '"unclosed' }, /never closes/);
  bad({}, /unknown field channel/, "channel: devto\n");
  bad({}, /appears twice/, "model: x\n");
  bad({ kind: "refresh" }, /path \/blog\/other must be \/blog\/a-post/, "path: /blog/other\n");
  bad({ title: '"a\\u0008b"' }, /control character/);
  assert.throws(() => parsePost(file().replace(/\n---\n\n/, "\n---garbage\n\n"), "p.md"), /closed by a line of exactly ---/);
  assert.throws(() => parsePost(file().replace("  - another search", "  -another search"), "p.md"), /not "key: value" or a list item/);
  assert.throws(() => parsePost("no front matter\n", "p.md"), /must start with a --- front matter block/);
});

test("a trailing # comment ends a plain value, as the README's example shows", () => {
  const { meta } = parsePost(file({ slug: "a-post      # the file name", model: "claude-opus-5-5   # the model" }), "p.md");
  assert.equal(meta.slug, "a-post");
  assert.equal(meta.model, "claude-opus-5-5");
  assert.equal(parsePost(file({ title: '"Chapter #1 # not a comment"' }), "p.md").meta.title, "Chapter #1 # not a comment");
});

test("the README's front matter example is accepted", () => {
  const readme = readFileSync(new URL("../README.md", import.meta.url), "utf8");
  const example = readme.match(/```yaml\n(---\n[\s\S]*?\n---)\n```/)[1];
  assert.equal(parsePost(`${example}\n\nBody\n`, "README").meta.slug, "run-a-coding-agent-in-a-sandbox");
});

test("dates must be real days", () => {
  assert.ok(isDay("2026-11-16") && isDay("2028-02-29"));
  assert.ok(!isDay("2026-02-29") && !isDay("2026-13-01") && !isDay("2026-1-1"));
});
