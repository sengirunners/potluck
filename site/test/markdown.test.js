import { test } from "node:test";
import assert from "node:assert/strict";
import { dropTitleHeading, inline, MarkdownError, toHtml } from "../build/markdown.mjs";

const html = (s) => toHtml(s);
const refuses = (s, re) => assert.throws(() => toHtml(s), (e) => e instanceof MarkdownError && re.test(e.message), s);

test("paragraphs join their lines and escape HTML characters", () => {
  assert.equal(html("one\ntwo\n\nthree & 1 < 2 > 0 \"q\""), "<p>one two</p>\n<p>three &amp; 1 &lt; 2 &gt; 0 &quot;q&quot;</p>");
});

test("headings get ids, duplicates get a suffix, and # is refused", () => {
  const headings = [];
  assert.equal(toHtml("## What's *new*?\n\n### Same\n\n### Same", { headings }),
    '<h2 id="whats-new">What\'s <em>new</em>?</h2>\n<h3 id="same">Same</h3>\n<h3 id="same-2">Same</h3>');
  assert.deepEqual(headings.map((h) => [h.level, h.id]), [[2, "whats-new"], [3, "same"], [3, "same-2"]]);
  refuses("# Title", /h1/);
});

test("a draft's repeated title heading can be dropped before converting", () => {
  assert.equal(dropTitleHeading("# My title\n\nBody", "My title"), "Body");
  assert.equal(dropTitleHeading("# Other\n\nBody", "My title"), "# Other\n\nBody");
});

test("inline marks", () => {
  assert.equal(inline("**b** *i* _j_ `c<d>` [a](https://x.test/a?b=1&c=2) \\*lit\\*"),
    '<strong>b</strong> <em>i</em> <em>j</em> <code>c&lt;d&gt;</code> <a href="https://x.test/a?b=1&amp;c=2">a</a> *lit*');
  assert.equal(inline("**bold with *nested* italics**"), "<strong>bold with <em>nested</em> italics</strong>");
  assert.equal(inline("snake_case_name and 2 * 3 and a < b"), "snake_case_name and 2 * 3 and a &lt; b");
  assert.equal(inline("`**not bold**`"), "<code>**not bold**</code>");
  assert.equal(inline("[**bold link**](/x)"), '<a href="/x"><strong>bold link</strong></a>');
});

test("lists: bullets, numbers, wrapped items, blank lines between items", () => {
  assert.equal(html("- a\n  wrapped\n- b\n\n- c"), "<ul>\n<li>a wrapped</li>\n<li>b</li>\n<li>c</li>\n</ul>");
  assert.equal(html("1. one\n2. two"), "<ol>\n<li>one</li>\n<li>two</li>\n</ol>");
  assert.equal(html("* star bullet"), "<ul>\n<li>star bullet</li>\n</ul>");
});

test("code blocks keep their text and escape it", () => {
  assert.equal(html("```sh\na <b> & c\n\n  indented\n```"), '<pre><code class="language-sh">a &lt;b&gt; &amp; c\n\n  indented\n</code></pre>');
  assert.equal(html("```\nplain\n```"), "<pre><code>plain\n</code></pre>");
});

test("blockquotes hold paragraphs", () => {
  assert.equal(html("> one\n> two\n>\n> three"), "<blockquote>\n<p>one two</p>\n<p>three</p>\n</blockquote>");
});

test("tables: alignment, inline marks, a pipe inside code", () => {
  const out = html("| A | B |\n| :-- | --: |\n| `x|y` | **2** |");
  assert.match(out, /<th style="text-align:left">A<\/th><th style="text-align:right">B<\/th>/);
  assert.match(out, /<td style="text-align:left"><code>x\|y<\/code><\/td><td style="text-align:right"><strong>2<\/strong><\/td>/);
  assert.match(out, /^<div class="table-wrap"><table>/);
});

test("a paragraph stops where a list, fence or table starts", () => {
  assert.equal(html("text\n- item"), "<p>text</p>\n<ul>\n<li>item</li>\n</ul>");
  assert.match(html("text\n```\ncode\n```"), /^<p>text<\/p>\n<pre>/);
  assert.match(html("intro\n| a | b |\n|---|---|\n| 1 | 2 |"), /^<p>intro<\/p>\n<div class="table-wrap">/);
});

test("inputs other readers would render differently are errors or render as authored", () => {
  assert.equal(html("## C#"), '<h2 id="c">C#</h2>');
  assert.equal(html("## Using F# ##"), '<h2 id="using-f">Using F#</h2>');
  assert.equal(html("3. three\n4. four"), '<ol start="3">\n<li>three</li>\n<li>four</li>\n</ol>');
  assert.equal(html("In\n2024. was a year"), "<p>In 2024. was a year</p>");
  assert.equal(html("[w](https://en.wikipedia.org/wiki/Foo_(bar))"), '<p><a href="https://en.wikipedia.org/wiki/Foo_(bar)">w</a></p>');
  assert.equal(html("## Same\n\n## Same\n\n## Same 2\n\n## cta h").match(/id="[^"]+"/g).join(" "), 'id="same" id="same-2" id="same-2-2" id="cta-h-2"');
  refuses("1. a\n3. b", /count up/);
  refuses("> a\nb", /must start with >/);
  refuses("- a\nb", /indented to continue/);
  refuses("- - -", /horizontal rules/);
  refuses("* * *", /horizontal rules/);
  refuses("a  \nb", /hard line break/);
  refuses("text\n===", /setext/);
  refuses("+ item", /never \+/);
  refuses(" - indented", /left margin/);
  refuses("** x**", /touch the text/);
  refuses("&copy; 2026", /entities/);
  refuses("[x](/\\evil.test)", /link address/);
  refuses("> ## heading", /inside a blockquote/);
});

test("everything outside the subset is an error, with its line", () => {
  refuses("<div>html</div>", /raw HTML/);
  refuses("see <https://x.test>", /raw HTML|autolinks/);
  refuses("![alt](/x.png)", /images/);
  refuses("~~gone~~", /strikethrough/);
  refuses("---", /horizontal rules/);
  refuses("    indented code", /indented code/);
  refuses("- a\n  - nested", /nested lists/);
  refuses("- a\n\n  second paragraph", /more than one paragraph/);
  refuses("- a\n1. b", /mix/);
  refuses("> a\n>> b", /nested blockquotes/);
  refuses("```\nnever closed", /never closes/);
  refuses("```two words\nx\n```", /language/);
  refuses("**open", /bold that never closes/);
  refuses("*open", /italics that never close/);
  refuses("_open word", /italics that never close/);
  refuses("`open", /code that never closes/);
  refuses("[ref][1]", /reference links|never closes|followed by/);
  refuses("[x](javascript:alert(1))", /link address/);
  refuses("[x](//evil.test)", /link address/);
  refuses('[x](/a "title")', /titles/);
  refuses("line\\", /backslash/);
  refuses("a\n\n| a | b |\n|---|---|\n| 1 |", /cells/);
  refuses("\ttabbed", /tabs/);
  assert.throws(() => toHtml("ok\n\nok\n\n<b>bad</b>"), /line 5:/);
});
