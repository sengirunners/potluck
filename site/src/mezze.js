// mezze.sh: static assets and the founders form (src/contact.js, shared with potluck.sh). The entry module exports
// only the handler.
import { handleContact } from "./contact.js";

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);
    if (url.pathname === "/api/contact") return handleContact(request, env, ctx);
    if (url.pathname.startsWith("/api/")) return new Response("Not found", { status: 404 });
    return env.ASSETS.fetch(request);
  },
};
