// potluck.sh: static assets, the founders form (src/contact.js) and the closed waitlist's two routes
// (src/waitlist.js). The entry module exports only the handler.
import { handleContact } from "./contact.js";
import { handleClosed, handleCount } from "./waitlist.js";

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);
    if (url.pathname === "/api/waitlist") return handleClosed();
    if (url.pathname === "/api/contact") return handleContact(request, env, ctx);
    if (url.pathname === "/api/waitlist/count") return handleCount(request, env);
    return env.ASSETS.fetch(request);
  },
};
