// The installer's one address, and potluck.sh/install.sh, which sends curl there. The installer itself lives in the
// repository (install.sh at its root); this site never serves a copy, so there is one installer to trust.
export const INSTALLER_URL = "https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh";

// 302, not 301: a permanent redirect is cached by clients for good, and the installer's address may move.
export function handleInstall() {
  return new Response(null, { status: 302, headers: { location: INSTALLER_URL, "cache-control": "public, max-age=300" } });
}
