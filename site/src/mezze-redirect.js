// potluck.sh/mezze moved to Mezze's own site (mezze/, a second Worker): a permanent redirect, so search engines move
// the page's standing there. Kept apart from src/worker.js because a Worker's entry module may export only handlers.
export const MEZZE_URL = "https://mezze.sh/";

// The page was public/mezze.html, so its .html address (which Workers assets redirected to /mezze) moves too.
const MOVED = new Set(["/mezze", "/mezze/", "/mezze.html", "/mezze.html/"]);

export const isMezzePath = (pathname) => MOVED.has(pathname);

export const handleMezze = () => Response.redirect(MEZZE_URL, 301);
