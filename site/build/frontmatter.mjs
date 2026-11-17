// Front matter of a blog post: the fields the growth content job writes (farm/sengi-content-job in agent-hub:
// slug, kind, engine, path, title, page_title, page_description, target_searches, data_sources, drafted, model),
// plus `date` (first published) and `updated` (last changed), both written by hand and committed, never read from git,
// and an optional `summary` (the short answer shown above the post; without it the post's first paragraph leads).
//
// The syntax is the small YAML subset the job writes: `key: value`, values plain or "double quoted" (JSON escapes),
// and lists as `  - item` lines. A " #" after a value starts a comment (so quote a value that contains one).

export class FrontMatterError extends Error {
  constructor(message) {
    super(message);
    this.name = "FrontMatterError";
  }
}

export const KINDS = ["new", "refresh"];
export const ENGINES = ["claude", "chatgpt"]; // farm/growth/article-policy.mjs

const SCALARS = ["slug", "kind", "engine", "path", "title", "page_title", "page_description", "model", "date", "updated", "drafted", "summary"];
const LISTS = ["target_searches", "data_sources"];
const REQUIRED = ["slug", "kind", "engine", "title", "page_title", "page_description", "target_searches", "data_sources", "model", "date", "updated"];

// Characters XML 1.0 (the feed) and JSON-LD readers do not accept, other than tab, newline and carriage return.
const CONTROL = /[\u0000-\u0008\u000B\u000C\u000E-\u001F\u007F]/;

export const isDay = (s) => {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(s)) return false;
  const d = new Date(`${s}T00:00:00Z`);
  return !Number.isNaN(d.getTime()) && d.toISOString().slice(0, 10) === s;
};

function scalar(text, where) {
  const t = text.trim();
  if (t.startsWith('"')) {
    let end = -1;
    for (let i = 1; i < t.length; i++) {
      if (t[i] === "\\") { i++; continue; }
      if (t[i] === '"') { end = i; break; }
    }
    if (end < 0) throw new FrontMatterError(`${where}: a quoted value never closes`);
    const rest = t.slice(end + 1).trim();
    if (rest && !rest.startsWith("#")) throw new FrontMatterError(`${where}: text after the closing quote`);
    try {
      return JSON.parse(t.slice(0, end + 1));
    } catch {
      throw new FrontMatterError(`${where}: the quoted value is not a valid string`);
    }
  }
  if (t.startsWith("'") || t.startsWith("[") || t.startsWith("{") || t.startsWith("|") || t.startsWith(">")) {
    throw new FrontMatterError(`${where}: use a plain value or a "double quoted" one`);
  }
  // A plain value ends where a " #" starts a comment; quote a value that holds one.
  return t.replace(/\s+#.*$/, "");
}

/** Splits a post file into its parsed front matter and its Markdown body. */
export function parsePost(text, file = "post") {
  const src = String(text).replace(/\r\n?/g, "\n");
  const all = src.split("\n");
  const close = all[0] === "---" ? all.indexOf("---", 1) : -1;
  if (close < 0) throw new FrontMatterError(`${file}: the file must start with a --- front matter block, closed by a line of exactly ---`);
  const m = [null, all.slice(1, close).join("\n"), all.slice(close + 1).join("\n")];
  const data = {};
  let list = null;
  for (const [n, line] of m[1].split("\n").entries()) {
    const where = `${file}: front matter line ${n + 2}`;
    if (!line.trim() || line.trim().startsWith("#")) continue;
    const item = line.match(/^\s+-(?:\s+(.*))?$/);
    if (item) {
      if (!list) throw new FrontMatterError(`${where}: a list item with no list key above it`);
      list.push(scalar(item[1] ?? "", where));
      continue;
    }
    const kv = line.match(/^([a-z_]+):(?:\s+(.*))?$/);
    if (!kv) throw new FrontMatterError(`${where}: not "key: value" or a list item`);
    const [, key, value = ""] = kv;
    if (key in data) throw new FrontMatterError(`${where}: ${key} appears twice`);
    if (LISTS.includes(key)) {
      if (value.trim()) throw new FrontMatterError(`${where}: ${key} is a list; put each item on its own "  - " line`);
      list = data[key] = [];
    } else if (SCALARS.includes(key)) {
      list = null;
      data[key] = scalar(value, where);
    } else {
      throw new FrontMatterError(`${where}: unknown field ${key} (known: ${[...SCALARS, ...LISTS].join(", ")})`);
    }
  }
  return { meta: validate(data, file), body: m[2].replace(/^\n+/, "").replace(/\s+$/, "") + "\n" };
}

/** Checks a post's fields; returns them normalised. Throws FrontMatterError naming the file and the field. */
export function validate(d, file = "post") {
  const bad = (msg) => {
    throw new FrontMatterError(`${file}: ${msg}`);
  };
  for (const k of REQUIRED) if (d[k] === undefined) bad(`missing required field ${k}`);
  for (const k of [...SCALARS, ...LISTS]) {
    if (d[k] === undefined) continue;
    if (LISTS.includes(k)) {
      if (!Array.isArray(d[k]) || !d[k].length) bad(`${k} must list at least one item`);
      d[k].forEach((s) => { if (typeof s !== "string" || !s.trim() || /\n/.test(s) || CONTROL.test(s)) bad(`${k} has an empty, multi-line or control-character item`); });
    } else if (typeof d[k] !== "string" || !d[k].trim()) {
      bad(`${k} is empty`);
    } else if (CONTROL.test(d[k])) {
      bad(`${k} holds a control character`);
    } else if (k !== "summary" && /\n/.test(d[k])) {
      bad(`${k} must be one line`);
    }
  }
  if (!/^[a-z0-9]+(-[a-z0-9]+)*$/.test(d.slug)) bad(`slug ${JSON.stringify(d.slug)} must be lowercase letters, digits and single hyphens`);
  if (!KINDS.includes(d.kind)) bad(`kind must be one of ${KINDS.join(", ")}`);
  if (!ENGINES.includes(d.engine)) bad(`engine must be one of ${ENGINES.join(", ")}`);
  for (const k of ["date", "updated", "drafted"]) if (d[k] !== undefined && !isDay(d[k])) bad(`${k} must be a real YYYY-MM-DD date`);
  if (d.updated < d.date) bad("updated is before date");
  if (d.path !== undefined && d.path !== `/blog/${d.slug}`) bad(`path ${d.path} must be /blog/${d.slug}`);
  return d;
}
