// potluck.sh: static assets plus the closed waitlist's two routes (src/waitlist.js). The entry module exports only
// the handler.
import { handleClosed, handleCount } from "./waitlist.js";

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname === "/api/waitlist") return handleClosed();
    if (url.pathname === "/api/waitlist/count") return handleCount(request, env);
    return env.ASSETS.fetch(request);
  },
};
