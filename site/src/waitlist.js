// The waitlist API and the page's no-JavaScript result line. src/worker.js routes to these; they live here because a
// Worker's entry module may export only handlers.
//
//   POST /api/waitlist        form or JSON: email, hub, capacity (both optional ticks), website (honeypot)
//   GET  /api/waitlist/count  Authorization: Bearer $WAITLIST_ADMIN_TOKEN; {total, hub, capacity, both}, never emails
//   GET  /?joined=1, /?error=<code>   the page with the waitlist result filled in (the no-JavaScript path)
//
// A post whose Accept header names application/json (the page's script) gets JSON back. Any other post (a plain
// form post) gets a 303 to /?joined=1#waitlist or /?error=<code>#waitlist.
//
// Bindings: ASSETS (static files), WAITLIST (KV), WAITLIST_LIMITER (rate limit, optional).
// Secret: WAITLIST_ADMIN_TOKEN (the count route refuses everyone without it).
// KV: one key per address, `email:<address>` -> {"hub","capacity","joined","updated"}, with hub and capacity
// also in the key's metadata so counting needs only list calls.

const MAX_BODY_BYTES = 2048;

// Local part: no quoting, no @, and no leading = + - (they start formulas when the list is opened in a sheet).
const LOCAL = /^[A-Za-z0-9._%+'!#$&*/=?^`{|}~-]{1,64}$/;
const DOMAIN = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+$/;
const PRINTABLE_ASCII = /^[\x21-\x7e]+$/;

// What the page says for each result. The page's script has the same words.
export const MESSAGES = {
  joined: "You're on the list. Thanks.",
  email: "That email address doesn't look right.",
  rate: "Too many tries. Wait a minute and try again.",
};
const FALLBACK = "That didn't work. Try again, or email ops@sengi.run.";
// The empty result line in public/index.html; the no-JavaScript path fills it in.
export const RESULT_SLOT = '<p class="result" role="status" aria-live="polite"></p>';

export async function handleWaitlist(request, env, now = new Date()) {
  if (request.method !== "POST") return json({ ok: false, error: "method" }, 405, { allow: "POST" });

  const wantsJson = (request.headers.get("accept") ?? "").toLowerCase().includes("application/json");
  const done = () => (wantsJson ? json({ ok: true }) : back("joined=1"));
  const fail = (error, status) =>
    wantsJson ? json({ ok: false, error }, status) : back(`error=${encodeURIComponent(error)}`);

  // A browser posting from another site: refuse. Clients that send no Origin (curl) are let through.
  const origin = request.headers.get("origin");
  if (origin !== null && !sameHost(origin, request.url)) return fail("origin", 403);

  if (!env.WAITLIST) return fail("unavailable", 503);

  if (env.WAITLIST_LIMITER) {
    const ip = request.headers.get("cf-connecting-ip") ?? "";
    let allowed;
    try {
      ({ success: allowed } = await env.WAITLIST_LIMITER.limit({ key: ip }));
    } catch {
      return fail("unavailable", 503);
    }
    if (!allowed) return fail("rate", 429);
  }

  const body = await readCapped(request, MAX_BODY_BYTES);
  if (body === null) return fail("size", 413);

  const fields = await parseFields(request.headers.get("content-type") ?? "", body);
  if (fields === null) return fail("body", 400);

  // Bots fill the hidden field. Answer as if it worked so they learn nothing.
  if (str(fields.website) !== "") return done();

  const email = normaliseEmail(fields.email);
  if (email === null) return fail("email", 400);

  const key = `email:${email}`;
  const at = now.toISOString();
  try {
    const prev = await readRecord(env.WAITLIST, key);
    // A repeat sign-up may add an interest but never clears one, so nobody can zero another person's answer.
    const hub = prev?.hub === true || ticked(fields.hub);
    const capacity = prev?.capacity === true || ticked(fields.capacity);
    if (prev === null || prev.hub !== hub || prev.capacity !== capacity) {
      const record = { hub, capacity, joined: prev?.joined ?? at, updated: at };
      await env.WAITLIST.put(key, JSON.stringify(record), { metadata: { hub, capacity } });
    }
  } catch {
    // KV over its daily write quota, or down: answer in our own words, never with Cloudflare's error page.
    return fail("unavailable", 503);
  }

  // Same answer for a new address and a known one: the response never says who is on the list.
  return done();
}

export async function handleCount(request, env) {
  if (request.method !== "GET") return json({ ok: false, error: "method" }, 405, { allow: "GET" });
  if (!(await authorized(request.headers.get("authorization"), env.WAITLIST_ADMIN_TOKEN))) {
    return json({ ok: false, error: "unauthorized" }, 401, { "www-authenticate": "Bearer" });
  }
  if (!env.WAITLIST) return json({ ok: false, error: "unavailable" }, 503);

  const counts = { total: 0, hub: 0, capacity: 0, both: 0 };
  try {
    let cursor;
    do {
      const page = await env.WAITLIST.list({ prefix: "email:", cursor });
      for (const k of page.keys) {
        let m = k.metadata;
        if (typeof m?.hub !== "boolean" || typeof m?.capacity !== "boolean") {
          m = (await readRecord(env.WAITLIST, k.name)) ?? {};
        }
        const hub = m.hub === true;
        const capacity = m.capacity === true;
        counts.total++;
        if (hub) counts.hub++;
        if (capacity) counts.capacity++;
        if (hub && capacity) counts.both++;
      }
      cursor = page.list_complete ? undefined : page.cursor;
    } while (cursor);
  } catch {
    return json({ ok: false, error: "unavailable" }, 503);
  }

  return json(counts);
}

// The home page. With ?joined=1 or ?error=<code> (where a plain form post lands) the result line says what
// happened, so the form works without JavaScript. The words come from MESSAGES, never from the query string.
export async function handlePage(request, env) {
  const url = new URL(request.url);
  const joined = url.searchParams.get("joined") === "1";
  const error = url.searchParams.get("error");
  if (request.method !== "GET" || (!joined && error === null)) return env.ASSETS.fetch(request);

  const asset = await env.ASSETS.fetch(request);
  if (!asset.ok || !(asset.headers.get("content-type") ?? "").includes("text/html")) return asset;
  const html = await asset.text();
  const filled = joined
    ? `<p class="result" role="status" aria-live="polite">${MESSAGES.joined}</p>`
    : `<p class="result bad" role="status" aria-live="polite">${escapeHtml(messageFor(error))}</p>`;
  const headers = new Headers(asset.headers);
  headers.delete("content-length");
  headers.delete("etag");
  headers.set("cache-control", "no-store");
  return new Response(html.replace(RESULT_SLOT, filled), { status: 200, headers });
}

export function messageFor(code) {
  return code !== "joined" && Object.hasOwn(MESSAGES, code) ? MESSAGES[code] : FALLBACK;
}

// Trimmed and lower-cased, or null when it is not a plain ASCII address we accept.
export function normaliseEmail(raw) {
  if (typeof raw !== "string") return null;
  const trimmed = raw.trim();
  // Checked before lower-casing: some non-ASCII letters lower-case to ASCII ones (U+212A, the Kelvin sign, to k).
  if (trimmed.length > 254 || !PRINTABLE_ASCII.test(trimmed)) return null;
  const email = trimmed.toLowerCase();
  const at = email.indexOf("@");
  if (at < 1) return null;
  const local = email.slice(0, at);
  const domain = email.slice(at + 1);
  if (!LOCAL.test(local) || "=+-@".includes(local[0]) || !DOMAIN.test(domain)) return null;
  return email;
}

function ticked(v) {
  if (v === true) return true;
  return typeof v === "string" && ["on", "1", "true", "yes"].includes(v.toLowerCase());
}

// Throws when KV does; null when the key is missing or its value is not JSON.
async function readRecord(kv, key) {
  const raw = await kv.get(key);
  if (raw === null) return null;
  try {
    const v = JSON.parse(raw);
    return v !== null && typeof v === "object" ? v : null;
  } catch {
    return null;
  }
}

async function parseFields(contentType, body) {
  const type = contentType.split(";")[0].trim().toLowerCase();
  try {
    if (type === "application/json") {
      const v = JSON.parse(new TextDecoder().decode(body));
      return v !== null && typeof v === "object" && !Array.isArray(v) ? v : null;
    }
    if (type === "application/x-www-form-urlencoded" || type === "multipart/form-data") {
      const form = await new Response(body, { headers: { "content-type": contentType } }).formData();
      return Object.fromEntries(form);
    }
  } catch {
    return null;
  }
  return null;
}

function str(v) {
  return typeof v === "string" ? v : "";
}

function escapeHtml(s) {
  return s.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);
}

function sameHost(origin, url) {
  try {
    return new URL(origin).host === new URL(url).host;
  } catch {
    return false;
  }
}

// Compares digests so the time taken does not depend on how much of the token matched.
async function authorized(header, token) {
  if (typeof token !== "string" || token === "" || typeof header !== "string") return false;
  const m = /^Bearer\s+(\S+)\s*$/i.exec(header);
  if (!m) return false;
  const [a, b] = await Promise.all([sha256(m[1]), sha256(token)]);
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a[i] ^ b[i];
  return diff === 0;
}

async function sha256(text) {
  return new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text)));
}

// Reads the body without trusting content-length (chunked uploads omit it); null when over the cap.
async function readCapped(request, max) {
  if (Number(request.headers.get("content-length") ?? 0) > max) return null;
  if (!request.body) return new Uint8Array();
  const reader = request.body.getReader();
  const chunks = [];
  let size = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > max) {
      await reader.cancel();
      return null;
    }
    chunks.push(value);
  }
  const out = new Uint8Array(size);
  let offset = 0;
  for (const c of chunks) {
    out.set(c, offset);
    offset += c.byteLength;
  }
  return out;
}

// A 303 back to the page's waitlist section, for plain form posts.
function back(query) {
  return new Response(null, { status: 303, headers: { location: `/?${query}#waitlist`, "cache-control": "no-store" } });
}

function json(data, status = 200, headers = {}) {
  return new Response(JSON.stringify(data), {
    status,
    headers: {
      "content-type": "application/json; charset=utf-8",
      "cache-control": "no-store",
      "x-content-type-options": "nosniff",
      ...headers,
    },
  });
}
