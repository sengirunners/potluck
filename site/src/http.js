// What the Worker's routes share: JSON answers and the admin bearer-token check.

// Compares digests so the time taken does not depend on how much of the token matched. A token that is not set
// refuses everyone.
export async function authorized(header, token) {
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

export function json(data, status = 200, headers = {}) {
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
