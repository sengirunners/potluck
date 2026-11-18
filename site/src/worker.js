// potluck.sh: static assets, the founders form (src/contact.js), the closed waitlist's two routes
// (src/waitlist.js), /install.sh, a redirect to the installer (src/install.js), and /mezze, a permanent redirect to
// Mezze's own site (src/mezze-redirect.js). The entry module exports only the handler.
import { handleContact } from "./contact.js";
import { handleInstall } from "./install.js";
import { handleMezze, isMezzePath } from "./mezze-redirect.js";
import { handleClosed, handleCount } from "./waitlist.js";

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);
    if (url.pathname === "/api/waitlist") return handleClosed();
    if (url.pathname === "/api/contact") return handleContact(request, env, ctx);
    if (url.pathname === "/api/waitlist/count") return handleCount(request, env);
    if (url.pathname === "/install.sh") return handleInstall();
    if (isMezzePath(url.pathname)) return handleMezze();
    return env.ASSETS.fetch(request);
  },
};
