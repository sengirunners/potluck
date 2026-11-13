// potluck.sh: static assets plus the waitlist API (src/waitlist.js). The entry module exports only the handler.
import { handleCount, handlePage, handleWaitlist } from "./waitlist.js";

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname === "/api/waitlist") return handleWaitlist(request, env);
    if (url.pathname === "/api/waitlist/count") return handleCount(request, env);
    if (url.pathname === "/") return handlePage(request, env);
    return env.ASSETS.fetch(request);
  },
};
