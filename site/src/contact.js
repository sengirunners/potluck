// The page's "Talk to the founders" form. Messages are kept in KV for the founders to read; nothing is sent on.
//
//   POST /api/contact   form fields name, email, topic, team (optional), message, and leave_empty (a field people
//                       never see: a filled one is a bot, answered as if it worked and not stored). Refused: a body
//                       over 16 KB or of no stated length, and a post from another site (its Origin names another host). With
//                       `accept: application/json` (the page's script) the answer is {"ok":true} or
//                       {"ok":false,"error":<code>}; a plain form post (no script) is sent on to /thanks, or gets a
//                       short page naming what to fix.
//   GET  /api/contact   Authorization: Bearer $CONTACT_ADMIN_TOKEN; {"messages":[...],"next":<cursor or null>},
//                       newest first, ?limit= of them (default and most 40: each is one KV read, and a Worker gets 50
//                       subrequests on the free plan), from ?cursor= (the last answer's next).
//
// Binding: CONTACT (KV). Secret: CONTACT_ADMIN_TOKEN (the read route refuses everyone without it).
// KV: `msg:<inverted time>:<random>` -> {name, email, topic, team, message, at, country, agent}; the inverted time
// makes a plain list newest first. `rate:<ip>:<hour>` counts one address's posts in an hour (expires after two).

import { authorized, json } from "./http.js";

// What the topic select offers; anything else is refused.
export const TOPICS = ["run", "broke", "build", "dish", "other"];
const LIMITS = { name: 100, email: 200, team: 120, message: 4000 };
const PER_HOUR = 10;
const MAX_BODY = 16 * 1024;
const PAGE = 40;
// Syntax only: something@something.something with no spaces.
const EMAIL = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

export async function handleContact(request, env) {
  if (request.method === "GET") return list(request, env);
  if (request.method !== "POST") return json({ ok: false, error: "method" }, 405, { allow: "GET, POST" });
  const wantsJSON = (request.headers.get("accept") ?? "").includes("application/json");
  const answer = (error) => {
    if (wantsJSON) return json(error ? { ok: false, error } : { ok: true }, error ? STATUS[error] : 200);
    if (!error) return new Response(null, { status: 303, headers: { location: "/thanks" } });
    return page(STATUS[error], WORDS[error]);
  };

  const origin = request.headers.get("origin");
  if (origin !== null && origin !== new URL(request.url).origin) return answer("origin");
  const size = Number(request.headers.get("content-length"));
  if (!request.headers.has("content-length") || !Number.isFinite(size)) return answer("length");
  if (size > MAX_BODY) return answer("long");
  let form;
  try {
    form = await request.formData();
  } catch {
    return answer("form");
  }
  const field = (k) => (typeof form.get(k) === "string" ? form.get(k).trim() : "");
  if (field("leave_empty") !== "") return answer(null);
  const msg = { name: field("name"), email: field("email"), topic: field("topic"), team: field("team"), message: field("message") };
  if (!msg.name || !msg.message) return answer("missing");
  if (!EMAIL.test(msg.email)) return answer("email");
  if (!TOPICS.includes(msg.topic)) return answer("topic");
  for (const [k, max] of Object.entries(LIMITS)) if (msg[k].length > max) return answer("long");
  if (!env.CONTACT) return answer("unavailable");

  try {
    const ip = request.headers.get("cf-connecting-ip") ?? "unknown";
    const now = new Date();
    const rateKey = `rate:${ip}:${now.toISOString().slice(0, 13)}`;
    const seen = Number(await env.CONTACT.get(rateKey)) || 0;
    if (seen >= PER_HOUR) return answer("busy");
    await env.CONTACT.put(rateKey, String(seen + 1), { expirationTtl: 7200 });
    const record = {
      ...msg,
      at: now.toISOString(),
      country: request.cf?.country ?? null,
      agent: (request.headers.get("user-agent") ?? "").slice(0, 200),
    };
    const key = `msg:${String(9e15 - now.getTime()).padStart(16, "0")}:${crypto.randomUUID().slice(0, 8)}`;
    await env.CONTACT.put(key, JSON.stringify(record));
  } catch {
    return answer("unavailable");
  }
  return answer(null);
}

const STATUS = { origin: 403, length: 411, form: 400, missing: 400, email: 400, topic: 400, long: 413, busy: 429, unavailable: 503 };
const WORDS = {
  origin: "That form was sent from another site.",
  length: "We couldn't read that form.",
  form: "We couldn't read that form.",
  missing: "Please add your name and a message.",
  email: "That email address doesn't look right.",
  topic: "Please pick what it's about.",
  long: "That's longer than the form takes. Please shorten it.",
  busy: "That's a lot of messages from one place. Please try again in an hour.",
  unavailable: "Messages aren't being saved right now. Please try again later.",
};

async function list(request, env) {
  if (!(await authorized(request.headers.get("authorization"), env.CONTACT_ADMIN_TOKEN))) {
    return json({ ok: false, error: "unauthorized" }, 401, { "www-authenticate": "Bearer" });
  }
  if (!env.CONTACT) return json({ ok: false, error: "unavailable" }, 503);
  const params = new URL(request.url).searchParams;
  const asked = Number(params.get("limit"));
  const limit = Number.isInteger(asked) && asked > 0 ? Math.min(asked, PAGE) : PAGE;
  try {
    const page = await env.CONTACT.list({ prefix: "msg:", cursor: params.get("cursor") || undefined, limit });
    const messages = [];
    for (const k of page.keys) {
      const raw = await env.CONTACT.get(k.name);
      if (raw === null) continue;
      try {
        messages.push(JSON.parse(raw));
      } catch {
        // A value that is not JSON was not written by this route; skip it.
      }
    }
    return json({ messages, next: page.list_complete ? null : page.cursor });
  } catch {
    return json({ ok: false, error: "unavailable" }, 503);
  }
}

// The answer to a plain form post that could not be kept: what to fix, and a way back.
function page(status, words) {
  const body = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Potluck: message not sent</title><link rel="stylesheet" href="/site.css">
<main class="wrap sent"><h1>Not sent yet</h1><p>${words}</p><p><a href="/#founders">Back to the form</a></p></main>`;
  return new Response(body, { status, headers: { "content-type": "text/html; charset=utf-8", "cache-control": "no-store" } });
}
