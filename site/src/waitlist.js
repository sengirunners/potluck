// The closed waitlist. The page no longer signs anyone up; the list it collected stays in KV until it is deleted
// (the page promised within 12 months of sign-up), and its counts can still be read. src/worker.js routes here;
// this lives apart because a Worker's entry module may export only handlers.
//
//   POST /api/waitlist        410 {"ok":false,"error":"closed"}: sign-ups are closed (an old copy of the page may post)
//   GET  /api/waitlist/count  Authorization: Bearer $WAITLIST_ADMIN_TOKEN; {total, hub, capacity, both}, never emails
//
// Binding: WAITLIST (KV). Secret: WAITLIST_ADMIN_TOKEN (the count route refuses everyone without it).
// KV: one key per address, `email:<address>` -> {"hub","capacity","joined","updated"}, with hub and capacity
// also in the key's metadata so counting needs only list calls.

export function handleClosed() {
  return json({ ok: false, error: "closed" }, 410);
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
