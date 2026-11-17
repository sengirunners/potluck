// A small Markdown converter for blog posts. It reads a fixed subset and refuses everything else, so a post never
// renders differently from what its author wrote:
//
//   blocks: ## to ###### headings, paragraphs, bullet lists (- or *), numbered lists (1.), fenced code (```lang),
//           blockquotes (>), pipe tables
//   inline: `code`, **bold**, *italic* (and _italic_ at a word edge), [text](url), \ escapes
//
// Not supported, and an error: a # heading (the post's title is the page's h1), nested lists, a list item with more
// than one paragraph, indented code, horizontal rules, setext headings, + bullets or indented list markers, a list
// starting anywhere but its own first line's number, lazy continuation lines (a quote line without > or a list line
// without indent), images, raw HTML and entities, strikethrough, hard line breaks, reference links. toHtml throws
// MarkdownError with the line number.

export class MarkdownError extends Error {
  constructor(message, line) {
    super(line ? `line ${line}: ${message}` : message);
    this.name = "MarkdownError";
    this.line = line;
  }
}

export const escapeHtml = (s) =>
  String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");

/** A URL a link may point at: http(s), mailto, a site path or an in-page anchor. */
export function checkHref(href, line) {
  if (!/^(https?:\/\/[^\s<>"\\]+|mailto:[^\s<>"\\]+|\/(?![/\\])[^\s<>"\\]*|#[^\s<>"\\]+)$/.test(href)) {
    throw new MarkdownError(`link address ${JSON.stringify(href)} is not an http(s) URL, a /path, a #anchor or a mailto:`, line);
  }
  return href;
}

const isWordChar = (c) => c !== undefined && /[\p{L}\p{N}]/u.test(c);

/** One run of inline Markdown to HTML. */
export function inline(text, line) {
  let out = "";
  let i = 0;
  // The index of the mark that closes what opened just before `from`, skipping escapes and code spans.
  const closer = (from, mark) => {
    for (let j = from; j < text.length; j++) {
      if (text[j] === "\\") { j++; continue; }
      if (text[j] === "`") { const e = text.indexOf("`", j + 1); if (e < 0) return -1; j = e; continue; }
      if (mark === "**" && text.startsWith("**", j)) return j;
      if (mark === "*" && text[j] === "*") {
        if (text[j + 1] === "*") { j++; continue; }
        return j;
      }
    }
    return -1;
  };
  while (i < text.length) {
    const c = text[i];
    if (c === "\\") {
      const n = text[i + 1];
      if (n === undefined) throw new MarkdownError("a line may not end in a backslash (hard line breaks are not supported)", line);
      if (!/[\\`*_{}[\]()#+\-.!|<>~]/.test(n)) throw new MarkdownError(`\\${n} is not an escape this converter knows`, line);
      out += escapeHtml(n);
      i += 2;
    } else if (c === "`") {
      const e = text.indexOf("`", i + 1);
      if (e < 0) throw new MarkdownError("a ` opens code that never closes (escape it as \\`)", line);
      if (e === i + 1) throw new MarkdownError("empty code span", line);
      out += `<code>${escapeHtml(text.slice(i + 1, e))}</code>`;
      i = e + 1;
    } else if (c === "&" && /^&(#\d+|#x[0-9a-fA-F]+|[A-Za-z][A-Za-z0-9]*);/.test(text.slice(i))) {
      throw new MarkdownError("HTML entities are not supported: write the character itself", line);
    } else if (c === "<") {
      if (/[A-Za-z/!?]/.test(text[i + 1] || "")) throw new MarkdownError("raw HTML and <autolinks> are not supported (escape a literal < as \\<)", line);
      out += "&lt;";
      i++;
    } else if (c === "!" && text[i + 1] === "[") {
      throw new MarkdownError("images are not supported", line);
    } else if (c === "~" && text[i + 1] === "~") {
      throw new MarkdownError("strikethrough is not supported", line);
    } else if (c === "[") {
      let depth = 0;
      let j = i;
      for (; j < text.length; j++) {
        if (text[j] === "\\") { j++; continue; }
        if (text[j] === "`") { const e = text.indexOf("`", j + 1); if (e < 0) break; j = e; continue; }
        if (text[j] === "[") depth++;
        else if (text[j] === "]" && --depth === 0) break;
      }
      if (j >= text.length || text[j] !== "]") throw new MarkdownError("a [ opens a link that never closes (escape a literal [ as \\[)", line);
      if (text[j + 1] !== "(") throw new MarkdownError("[text] must be followed by (address): reference links and bare brackets are not supported", line);
      let end = j + 2;
      for (let depth = 1; end < text.length; end++) {
        if (text[end] === "(") depth++;
        else if (text[end] === ")" && --depth === 0) break;
      }
      if (end >= text.length) throw new MarkdownError("a link's ( never closes", line);
      const href = text.slice(j + 2, end);
      if (/\s/.test(href)) throw new MarkdownError("link titles and spaces in link addresses are not supported", line);
      const label = text.slice(i + 1, j);
      if (/(^|[^\\])\[/.test(label) || label.includes("](")) throw new MarkdownError("a link's text may not hold another link", line);
      out += `<a href="${escapeHtml(checkHref(href, line))}">${inline(label, line)}</a>`;
      i = end + 1;
    } else if (c === "*" && text[i + 1] === "*") {
      if (/\s/.test(text[i + 2] || " ")) throw new MarkdownError("** must touch the text it makes bold (escape a literal * as \\*)", line);
      const e = closer(i + 2, "**");
      if (e < 0 || e === i + 2) throw new MarkdownError("** opens bold that never closes (escape a literal * as \\*)", line);
      out += `<strong>${inline(text.slice(i + 2, e), line)}</strong>`;
      i = e + 2;
    } else if (c === "*" && !/\s/.test(text[i + 1] || " ")) {
      const e = closer(i + 1, "*");
      if (e < 0 || e === i + 1) throw new MarkdownError("* opens italics that never close (escape a literal * as \\*)", line);
      out += `<em>${inline(text.slice(i + 1, e), line)}</em>`;
      i = e + 1;
    } else if (c === "_" && !isWordChar(text[i - 1]) && !/\s/.test(text[i + 1] || " ")) {
      // _ only opens italics at the start of a word, so snake_case stays text.
      let e = -1;
      for (let j = i + 1; j < text.length; j++) {
        if (text[j] === "\\") { j++; continue; }
        if (text[j] === "`") { const k = text.indexOf("`", j + 1); if (k < 0) break; j = k; continue; }
        if (text[j] === "_" && !isWordChar(text[j + 1]) && !/\s/.test(text[j - 1])) { e = j; break; }
      }
      if (e < 0) throw new MarkdownError("_ opens italics that never close (escape a literal _ as \\_)", line);
      out += `<em>${inline(text.slice(i + 1, e), line)}</em>`;
      i = e + 1;
    } else {
      out += escapeHtml(c);
      i++;
    }
  }
  return out;
}

/** A post body without its first line when that line is a `# title` heading equal to the post's title (a draft may repeat it). */
export function dropTitleHeading(body, title) {
  const m = String(body).match(/^\s*#[ \t]+(.*?)[ \t]*\n+/);
  return m && m[1] === title.trim() ? String(body).slice(m[0].length) : body;
}

// ids the page template itself uses, so a heading never takes one
const RESERVED_IDS = ["cta-h"];
const SLUG_STRIP = /[^\p{L}\p{N} -]/gu;
/** The text of a heading with its inline marks removed (for its id). */
const plainText = (s) => s.replace(/\[([^\]]*)\]\([^)]*\)/g, "$1").replace(/[`*]/g, "").replace(/\\(.)/g, "$1");

const isFence = (l) => /^```/.test(l);
const isTableSep = (l) => /^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$/.test(l) && l.includes("|");
const splitRow = (l) => {
  let s = l.trim();
  if (s.startsWith("|")) s = s.slice(1);
  if (s.endsWith("|") && !s.endsWith("\\|")) s = s.slice(0, -1);
  const cells = [];
  let cur = "";
  for (let i = 0; i < s.length; i++) {
    if (s[i] === "\\" && s[i + 1] === "|") { cur += "\\|"; i++; continue; }
    if (s[i] === "`") { const e = s.indexOf("`", i + 1); if (e > 0) { cur += s.slice(i, e + 1); i = e; continue; } }
    if (s[i] === "|") { cells.push(cur.trim()); cur = ""; continue; }
    cur += s[i];
  }
  cells.push(cur.trim());
  return cells.map((c) => c.replace(/\\\|/g, "|"));
};

/**
 * Markdown to HTML. `headings`, when given, collects {level, id, text} for each heading, so a caller can build a
 * table of contents.
 */
export function toHtml(source, { headings = [], inQuote = false } = {}) {
  const lines = String(source).replace(/\r\n?/g, "\n").split("\n");
  const html = [];
  const used = new Set(RESERVED_IDS);
  let i = 0;

  // A heading's id is its text, made unique among this post's ids and the ids the page template uses.
  const idFor = (text) => {
    const base = plainText(text).toLowerCase().replace(SLUG_STRIP, "").trim().replace(/\s+/g, "-") || "section";
    let id = base;
    for (let n = 2; used.has(id); n++) id = `${base}-${n}`;
    used.add(id);
    return id;
  };

  const MARKER = /^([-*]|\d+\.)\s+/;
  const isRule = (l) => /^\s*([-*_])(\s*\1){2,}\s*$/.test(l);
  // What may interrupt a paragraph: a heading, fence, quote, bullet, "1." or a rule (a later number never does).
  const startsBlock = (l) => /^#{1,6}\s/.test(l) || isFence(l) || /^>/.test(l) || /^\s*[-*]\s/.test(l) || /^\s*1\.\s/.test(l) || isRule(l);

  while (i < lines.length) {
    const raw = lines[i];
    const ln = i + 1;
    if (/^\s*$/.test(raw)) { i++; continue; }
    if (/\t/.test(raw.match(/^\s*/)[0])) throw new MarkdownError("tabs for indentation are not supported", ln);

    const h = raw.match(/^(#{1,6})\s+(.*?)(?:\s+#+)?\s*$/);
    if (h) {
      if (inQuote) throw new MarkdownError("headings inside a blockquote are not supported", ln);
      const level = h[1].length;
      if (level === 1) throw new MarkdownError("a # heading is not allowed: the post's title is the page's h1; start at ##", ln);
      if (!h[2]) throw new MarkdownError("empty heading", ln);
      const id = idFor(h[2]);
      headings.push({ level, id, text: plainText(h[2]) });
      html.push(`<h${level} id="${id}">${inline(h[2], ln)}</h${level}>`);
      i++; continue;
    }

    if (isFence(raw)) {
      const lang = raw.slice(3).trim();
      if (lang && !/^[A-Za-z0-9+#._-]+$/.test(lang)) throw new MarkdownError(`code fence info ${JSON.stringify(lang)} is not a single language name`, ln);
      const body = [];
      i++;
      while (i < lines.length && !/^```\s*$/.test(lines[i])) { body.push(lines[i]); i++; }
      if (i >= lines.length) throw new MarkdownError("a code fence never closes", ln);
      i++;
      html.push(`<pre><code${lang ? ` class="language-${escapeHtml(lang)}"` : ""}>${escapeHtml(body.join("\n"))}\n</code></pre>`);
      continue;
    }

    if (isRule(raw)) throw new MarkdownError("horizontal rules are not supported", ln);
    if (/^=+\s*$/.test(raw)) throw new MarkdownError("setext headings are not supported: use ## Heading", ln);
    if (/^\s{1,3}([-*+]|\d+\.)\s/.test(raw) || /^\+\s/.test(raw)) throw new MarkdownError("start a list item at the left margin and use - or *, never +", ln);

    if (/^ {4}/.test(raw)) throw new MarkdownError("indented code is not supported: use a ``` fence", ln);

    if (raw.startsWith(">")) {
      const inner = [];
      const start = ln;
      while (i < lines.length && lines[i].startsWith(">")) { inner.push(lines[i].replace(/^> ?/, "")); i++; }
      if (i < lines.length && !/^\s*$/.test(lines[i])) throw new MarkdownError("every line of a blockquote must start with > (a line right after it would be taken into it by other readers)", i + 1);
      if (inner.some((l) => l.startsWith(">"))) throw new MarkdownError("nested blockquotes are not supported", start);
      const sub = toHtml(inner.join("\n"), { inQuote: true });
      html.push(`<blockquote>\n${sub}\n</blockquote>`);
      continue;
    }

    const li = raw.match(/^([-*]|\d+\.)\s+(.*)$/);
    if (li) {
      const ordered = /\d/.test(li[1]);
      const first = ordered ? parseInt(li[1], 10) : 0;
      const items = [];
      while (i < lines.length) {
        const m = lines[i].match(/^([-*]|\d+\.)\s+(.*)$/);
        if (!m) break;
        if (/\d/.test(m[1]) !== ordered) throw new MarkdownError("a list may not mix bullets and numbers", i + 1);
        if (ordered && parseInt(m[1], 10) !== first + items.length) throw new MarkdownError(`numbered items must count up from the first (expected ${first + items.length}.)`, i + 1);
        let text = m[2];
        const at = i + 1;
        i++;
        while (i < lines.length && /^\s+\S/.test(lines[i])) {
          if (/^\s+([-*]|\d+\.)\s/.test(lines[i])) throw new MarkdownError("nested lists are not supported", i + 1);
          text += " " + lines[i].trim();
          i++;
        }
        if (i < lines.length && !/^\s*$/.test(lines[i]) && !MARKER.test(lines[i]) && !startsBlock(lines[i]) && !lines[i].includes("|")) {
          throw new MarkdownError("a line after a list item must be indented to continue it, or follow a blank line", i + 1);
        }
        if (i < lines.length && /^\s*$/.test(lines[i])) {
          // A blank line is fine between items, but not before a continuation.
          let k = i;
          while (k < lines.length && /^\s*$/.test(lines[k])) k++;
          if (k < lines.length && /^\s+\S/.test(lines[k]) && !/^([-*]|\d+\.)\s/.test(lines[k])) throw new MarkdownError("a list item with more than one paragraph is not supported", k + 1);
          if (k < lines.length && /^([-*]|\d+\.)\s/.test(lines[k])) i = k;
        }
        items.push(`<li>${inline(text, at)}</li>`);
      }
      html.push(ordered ? `<ol${first === 1 ? "" : ` start="${first}"`}>\n${items.join("\n")}\n</ol>` : `<ul>\n${items.join("\n")}\n</ul>`);
      continue;
    }

    if (raw.includes("|") && i + 1 < lines.length && isTableSep(lines[i + 1])) {
      const head = splitRow(raw);
      const sep = splitRow(lines[i + 1]);
      if (head.length !== sep.length) throw new MarkdownError("a table's header and separator rows have different column counts", ln);
      const align = sep.map((s) => (s.startsWith(":") && s.endsWith(":") ? "center" : s.endsWith(":") ? "right" : s.startsWith(":") ? "left" : ""));
      const cell = (tag, text, k, at) => `<${tag}${align[k] ? ` style="text-align:${align[k]}"` : ""}>${inline(text, at)}</${tag}>`;
      const rows = [];
      i += 2;
      while (i < lines.length && lines[i].includes("|") && !/^\s*$/.test(lines[i])) {
        const cells = splitRow(lines[i]);
        if (cells.length !== head.length) throw new MarkdownError(`a table row has ${cells.length} cells; the header has ${head.length}`, i + 1);
        rows.push(`<tr>${cells.map((c, k) => cell("td", c, k, i + 1)).join("")}</tr>`);
        i++;
      }
      html.push(`<div class="table-wrap"><table>\n<thead><tr>${head.map((c, k) => cell("th", c, k, ln)).join("")}</tr></thead>\n<tbody>\n${rows.join("\n")}\n</tbody>\n</table></div>`);
      continue;
    }

    // A paragraph runs to the next blank line or block start.
    const para = [raw.trim()];
    const start = ln;
    i++;
    while (i < lines.length && !/^\s*$/.test(lines[i]) && !startsBlock(lines[i])) {
      if (lines[i].includes("|") && i + 1 < lines.length && isTableSep(lines[i + 1])) break;
      if (/^=+\s*$/.test(lines[i])) throw new MarkdownError("setext headings are not supported: use ## Heading", i + 1);
      if (/ {2,}$/.test(lines[i - 1])) throw new MarkdownError("a line ending in two spaces is a hard line break, which is not supported", i);
      para.push(lines[i].trim());
      i++;
    }
    html.push(`<p>${inline(para.join(" "), start)}</p>`);
  }
  return html.join("\n");
}
