// The /join page. An invite link is https://potluck.sh/join#v1.<code>: the code is after the "#", so the browser
// never sends it to the Worker. joinView decides what to show and is the only place the command is built; the DOM
// glue below it only sets text.
const FRAGMENT = /^#?(v1\.[A-Za-z0-9_-]{16,600})$/;
const ORIGIN = /^https?:\/\/[A-Za-z0-9.-]+(:[0-9]{1,5})?$/;
const INSTALL = "curl -fsSL https://potluck.sh/install.sh | sh -s -- --join";

/** "mac", "linux" or "other" (a phone, Windows, or anything unknown) from a platform string. */
export function platformOf(text) {
  if (/iPhone|iPad|Android|CrOS/i.test(text)) return "other";
  if (/Mac/i.test(text)) return "mac";
  if (/Linux|X11/i.test(text)) return "linux";
  return "other";
}

/**
 * What to show for `hash` (location.hash) on `origin` (location.origin) from `platform` ("mac", "linux", "other").
 * {kind:"command", command, github, link, mac} on a computer that can run it; {kind:"link", link} elsewhere, to open the
 * link on the right machine; {kind:"incomplete"} when the fragment is missing or not an invite.
 */
export function joinView(hash, origin, platform) {
  const m = FRAGMENT.exec(hash);
  if (!m || !ORIGIN.test(origin)) return { kind: "incomplete" };
  const link = `${origin}/join#${m[1]}`;
  if (platform === "other") return { kind: "link", link };
  // github: the same with GitHub sign-in (optional: the invite works without it).
  return { kind: "command", command: `${INSTALL} '${link}'`, github: `${INSTALL} '${link}' --github`, link, mac: platform === "mac" };
}

function glue() {
  const $ = (id) => document.getElementById(id);
  const view = joinView(location.hash, location.origin, platformOf(navigator.userAgentData?.platform ?? navigator.userAgent));
  const show = (id) => { $(id).hidden = false; };
  if (view.kind === "incomplete") return show("bad");
  if (view.kind === "link") {
    $("link-text").textContent = view.link;
    show("phone");
  } else {
    $("cmd").textContent = view.command;
    $("cmd-github").textContent = view.github;
    $("manual").textContent = `potluck join '${view.link}'`;
    $("mac-hint").hidden = !view.mac;
    show("ok");
  }
  for (const b of document.querySelectorAll("[data-copy]")) {
    const label = b.textContent;
    b.addEventListener("click", async () => {
      const target = $(b.dataset.copy);
      try {
        await navigator.clipboard.writeText(target.textContent);
        b.textContent = "Copied";
      } catch {
        getSelection().selectAllChildren(target);
        b.textContent = "Selected: copy it now";
      }
      setTimeout(() => { b.textContent = label; }, 1800);
    });
  }
}

if (typeof document !== "undefined") glue();
