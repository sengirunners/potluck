# potluck.sh

One static home page (`public/index.html`, with its stylesheet `public/site.css` and script `public/site.js`), Markdown pages (`content/pages/`, built into `public/<slug>.html`) and a Markdown blog (`content/blog/`, built into `public/blog/`; see "The blog and indexing") served by a Cloudflare Worker (`src/worker.js`) with static assets. The Worker
answers the founders form (`src/contact.js`), the two routes of the closed waitlist (`src/waitlist.js`) and
`/install.sh`, a 302 to the installer in the repository (`src/install.js`; the site never serves a copy of it), kept
apart from `src/worker.js` because a Worker's entry module may export only handlers; every other path is a static
file. No framework, no dependencies, no tracking scripts, and no
third-party requests: the font (Newsreader, OFL, latin subset, variable, one file each for roman and italic) is
self-hosted in `public/fonts` with its licence. The picnic scene at the top is built in CSS 3D: the machines are boxes
whose sides darken as they turn away from a fixed sun (CSS `cos()`, with fixed shading where that is missing), and the
food and cloud are inline SVG cut-outs that keep facing the reader; there are no people. The page's only script,
`site.js`, runs the scene (pick a machine, drag the lawn or press the arrows to turn it, all the way round), places the machines' labels in a flat layer
from where each machine lands on screen (front to back, each at the cheapest spot: never over another label, the card
or the arrows, and, where there is room, off every machine and with a leader line that runs over no other machine,
label or leader), the three How it works figures (a blanket that unfolds with scroll, then, at each click on it, deals Emily's and Leo's invites, folds up and lays out again, machines that drop into a
row and spring to life when pointed at, focused or tapped, over brackets that show their pools, a MacBook whose Potluck menu works: the reader picks Keep lending, Wind down or Stop now, the jobs answer and the matching command lights up, and if nobody picks, the pointer winds it down; their classes start `bk-`, `sk-` and `mb-`), the cost slider, the copy button
and the form. Without JavaScript the page still reads in full; the labels, turn arrows and copy button stay hidden,
the figures show their finished state, and the form posts as a plain form. With `prefers-reduced-motion`, nothing
moves on its own. The card's numbers are half of each machine's cores and memory,
because `potluck host` shares half unless told otherwise (`--share`).

## The blog and indexing

Posts are Markdown files, `content/blog/<slug>.md`, built into static pages by `npm run build` (`build/build.mjs`, Node's
standard library only). The built files are committed under `public/`, so a deploy stays `npx wrangler deploy`;
`npm test` fails when the committed files differ from a fresh build, so run `npm run build` after any change to
`content/`, `build/` or `index.html`'s seo block, and commit the result. With no post there is no placeholder: the
blog page, the feed and the home page's Blog link are not built.

A post starts with front matter in the fields the growth content job writes (`farm/sengi-content-job` in agent-hub), plus
`date` and `updated`:

```yaml
---
slug: run-a-coding-agent-in-a-sandbox      # the file name, without .md
kind: new                                   # new | refresh
engine: claude                              # claude | chatgpt
title: "The page's h1"
page_title: "The <title> | Potluck"
page_description: "The meta description (one line)."
target_searches:
  - "a search this post answers" # 320/month
data_sources:
  - "where the numbers came from"
model: claude-opus-5-5
date: 2026-11-12                            # first published
updated: 2026-11-15                         # last changed: sitemap <lastmod>, feed <updated>; committed, never from git
summary: "Optional. The short answer shown above the post."
---
```

`drafted:` and `path:` (a refresh's `/blog/<slug>`) are accepted too, and `author:` (a person's name, for an essay
someone signs): the byline, the feed's `<author>` and the JSON-LD (a `Person`) name them; without it all three name
Sengi. A missing or malformed field, an unknown field, or
a slug that is not the file name fails the build. The body is the Markdown subset `build/markdown.mjs` reads: `##` to
`######` headings (not `#`: the title is the page's h1; a first line that repeats the title is dropped), paragraphs,
bullet and numbered lists, fenced code, blockquotes, pipe tables, and inline code, bold, italic and links. Anything
else (raw HTML, images, nested lists, rules, indented code, strikethrough) fails the build with the line number rather
than rendering wrong. Put the answer first: the first paragraph, or `summary`, is what a search result or an assistant
quotes.

A page is the same kind of file in `content/pages/<slug>.md`, served at `/<slug>`: an answer-first guide that is not
dated news. It takes `slug`, `title`, `page_title`, `page_description`, `target_searches`, `date` and `updated`,
and optionally `summary`, `author` and `data_sources` (name the sources and the day each fact was read). It renders
with the article layout, a TechArticle and BreadcrumbList JSON-LD, and no Markdown copy; it is in the sitemap,
`llms.txt` and `llms-full.txt`. A slug in `RESERVED_SLUGS` (`build/pages.mjs`) or the name of a hand-written file in
`public/` fails the build. Every article ends with "More from Potluck": a post lists the pages, a page lists the other
pages and the posts.

What the build writes:

| file | what |
| --- | --- |
| `public/blog/<slug>.html`, `public/blog.html` | the post and the index, served at `/blog/<slug>` and `/blog` (no trailing slash: Workers assets redirect the `.html` and slash forms, so the canonical address is the 200 one) |
| `public/blog/<slug>.md` | the post as Markdown, for assistants |
| `public/<slug>.html` | a page from `content/pages/`, served at `/<slug>`; it carries a `generator` meta tag naming its source, which is how the build knows it may delete it when the source goes |
| `public/og/` | one share image per page (1200x630 PNG): `home.png`, `blog.png`, `blog/<slug>.png`, `<slug>.png`, named in that page's `og:image` and `twitter:image` |
| `public/sitemap.xml` | every public page with `<lastmod>`: the home page's comes from `HOME.lastmod` in `build/pages.mjs` (bump it when the home page's content changes), pages' and posts' from `updated`; `/blog` only when a post exists |
| `public/feed.xml` | Atom, posts newest updated first |
| `public/robots.txt` | allow all; the search and AI crawlers named (GPTBot, OAI-SearchBot, ChatGPT-User, ClaudeBot, Claude-SearchBot, PerplexityBot, Google-Extended, Bingbot); `/api/` closed; the sitemap |
| `public/llms.txt`, `public/llms-full.txt` | `content/llms-intro.md` plus the page list; and the intro plus every page's Markdown (`content/home.md` for the home page; keep it in step with `index.html`) |
| `public/index.html` (between its markers) | title, description, canonical, Open Graph, Twitter tags, the icon links and JSON-LD (SoftwareApplication with `sameAs` the repository, Organization, WebSite that mentions Mezze), the footer's Blog link, and the header's and footer's links to mezze.sh (`MEZZE_NAV_LINK`, `MEZZE_FOOTER_LINK` in `build/layout.mjs`, the same ones the built pages use; `public/thanks.html` carries its own footer link) |

The share images are drawn by the build itself (`build/share.mjs`, standard library only): the page's title (the h1;
for the home page `HOME.headline`, which must match `index.html`'s h1, and a test checks it) in the site's font on
`build/og/template.png`, at the largest of three sizes it fits, with the page's address under it. A title with a
character the font set lacks fails the build, naming it. What the build cannot make without a font renderer is drawn
once by hand: `npm run og-assets` (`build/og-assets.mjs`; it needs Chrome) writes the blank template, the glyphs and
kerning of each size (`build/og/glyphs.json`), `public/favicon.ico` (16, 32 and 48 px),
`public/apple-touch-icon.png` (180 px), and `public/icon-192.png` and `icon-512.png` for `public/site.webmanifest`
(hand-written). Run it after the font, the logo or the quilt changes, or when `CHARS` in `build/share.mjs` gains a
character, then `npm run build`, and commit both. The drift check compares share images by their pixels, so a
different zlib does not count as a change.

`public/blog.css` is hand-made. `public/og.png`, the old hand-made share image that every page named before the
build drew one per page, stays for one release so share cards made before then keep their picture; nothing names it
now, so delete it after the next release. `docs/img/readme-hero.webp`, the
picture at the top of the repository's README, is drawn from the home page's headline and picnic by `npm run readme-hero`
(`build/readme-hero.mjs` and `build/readme-hero.css`; it needs Chrome and `cwebp`). Run it after the home page's hero
changes and commit the picture. `public/7b1a88c94e3d01e8f2150746ddb8a872.txt`
is the IndexNow key: public on purpose (the protocol serves it from the site to prove ownership), and a test checks it.
`public/BingSiteAuth.xml` proves to Bing Webmaster Tools that we own the site (the Bing account that also holds sengi.run);
it is public on purpose too. Removing it un-verifies the site in Bing.

`thanks.html` is `noindex`, and is not in the sitemap.

After adding a post, run `npx wrangler dev --local` and check that its canonical address answers 200 and not a redirect:
`curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8787/blog/<slug>` (no `-L`), likewise for `/blog` and `/blog/<slug>.md`.

`npm run notify` tells IndexNow (api.indexnow.org, which shares with Bing and others) which pages are new or changed:
it waits, for up to about two minutes, until the live `sitemap.xml` equals the committed one (an edge can serve the
previous build for a while), checks the key and that each page to send answers 200 with no redirect, then posts the
addresses whose `<lastmod>` is new or different from the sitemap IndexNow was last told about. That sitemap is kept in
`.wrangler/indexnow-sent.xml` (not committed) and rewritten after each successful send; `npm run deploy` creates it from
the live sitemap when it is missing, so if a notify fails after a deploy, `npm run notify` sends the right pages. With
no such file every page is sent. `--previous <file>` names a different "before"; `--dry-run` posts nothing. A deleted
post is not announced (IndexNow checks live pages here); a deleted page just drops out of the sitemap.

## mezze.sh

Mezze's own site is a second content root in this directory, built by the same `npm run build` and served by its
own Worker, `mezze-site` (`mezze/wrangler.jsonc`, entry `src/mezze.js`), on mezze.sh and www.mezze.sh. potluck.sh/mezze
answers a 301 to https://mezze.sh/ (`src/mezze-redirect.js`; `wrangler.jsonc` runs the Worker first for that path).

| file | what |
| --- | --- |
| `mezze/public/index.html` | the page, hand-written; the build writes its `<head>` tags between the seo markers from `HOME` in `build/mezze.mjs` |
| `mezze/content/home.md`, `mezze/content/llms-intro.md` | the page as Markdown for `llms-full.txt` (keep it in step with `index.html`), and the llms files' intro |
| `mezze/public/site.css`, `thanks.html`, `site.webmanifest` | hand-written |
| `mezze/public/og/home.png`, `mezze/public/img/robot.webp` | the share image Jackson picked and the robot from it without words (Mezze's private design source, its `social-preview` assets: `mezze-social-preview.png` and `-render.png` at 880 px wide) |
| `mezze/public/robot.js`, `mezze/public/3d/robot.bin` | the live robot: the picture's robot as a model (`robot.bin`, written by `blender -b --python build/mezze-robot.py` from the same construction as the design source's scene script, fewer segments) drawn with plain WebGL by `robot.js` (no library). It sways at rest; the pointer pushes whatever part it touches (an arm, a plate, the head) the way it moves, harder the faster, and every joint springs back. Angles and plate slides are clamped, so a plate never comes off its stick (`npm test` checks the limits). It starts after the load event when the browser is idle, draws over the picture in the same pose, and does nothing for reduced motion or without WebGL, where the picture stays |
| `mezze/public/scenes.js`, `mezze/public/img/props/` | the lower page's scenes, one per beat: props built from the robot's own shapes (`build/mezze_shapes.py`, shared with `build/mezze-robot.py`, including the triangle walk and colour encoding both files use) by `build/mezze-props.py`, drawn with `robot.js`'s shader and the robot's lights by `node build/mezze-props.mjs` (needs Blender 5.2, cwebp and Google Chrome; run by hand and commit what it writes). Each plate has a small still picture, which the page shows first, and a sheet of turns (`plate-<food>-turns.webp`; `TURNS`, `COLS` and `SPIN` in `mezze/public/plates.js`, shared with `robot.js`) that `scenes.js` loads when its scene comes near and steps through at the robot's spin. The plates sway, the pointer pushes them, and each beat plays one short loop while it is on screen; with no scene on screen nothing runs. `index.html` loads `scenes.js` with `robot.js`, only when motion is welcome; if the reader turns reduced motion on, the scenes go back to as drawn |
| `mezze/public/favicon.ico`, `apple-touch-icon.png`, `icon-192.png`, `icon-512.png` | drawn by `npm run og-assets` from the olive (`OLIVE_SVG` in `build/mezze.mjs`) |
| `mezze/public/robots.txt`, `sitemap.xml`, `llms.txt`, `llms-full.txt`, `fonts/` | generated (the font is copied from `public/fonts`, so there is one copy to change) |

Every claim on the page comes from Mezze's own README and code, as for potluck.sh. The page says the source is not
public yet and promises no date. The founders form posts to mezze.sh's own `/api/contact`, the same handler and the
same `CONTACT` store and email as potluck.sh's; each kept message records its `site`, and the email's subject names it.

Deploy with `npm run deploy:mezze` (`build/deploy-mezze.mjs`): the same checks as `npm run deploy`, then
`wrangler deploy -c mezze/wrangler.jsonc`, the live checks on both hosts, and IndexNow for mezze.sh (it remembers what it
sent in `.wrangler/indexnow-sent-mezze.xml`). Deploy mezze.sh before a potluck.sh deploy that carries the redirect, so
/mezze never points at a site that is not up.

## The founders form

The "Talk to Jackson and Clay" form posts to `/api/contact`. Messages are kept in KV, and each kept message is
emailed to the founders.

- `POST /api/contact`: fields `name`, `email`, `topic` (one of `TOPICS` in `src/contact.js`; `npm test` checks the
  page offers exactly those), `team` (optional), `message`, and `leave_empty`, a field people never see (filled: a bot,
  answered as if it worked and not stored). The page's script sends `accept: application/json` and gets
  `{"ok":true}` or `{"ok":false,"error":<code>}`; a plain form post is sent on to `/thanks` (`public/thanks.html`), or gets a short page
  naming what to fix. One address gets ten posts an hour; a body over 16 KB, of no stated length, or posted from
  another site is refused.
- `GET /api/contact[?limit=N][&cursor=C]` with `Authorization: Bearer <CONTACT_ADMIN_TOKEN>`:
  `{"messages":[...],"next":C}`, newest first, 40 at most a page (each message is one KV read; a Worker gets 50
  subrequests on the free plan); pass `next` back as `cursor` for the page after, until it is null. Refuses everyone if the secret is not set.
- Storage: KV namespace `CONTACT`, `msg:<inverted time>:<random>` →
  `{name,email,topic,team,message,at,country,agent}`, and `rate:<ip>:<hour>` counters that expire.

```sh
curl -s -H "authorization: Bearer $CONTACT_ADMIN_TOKEN" "https://potluck.sh/api/contact?limit=20"
```

- Email (`src/notify.js`): after a message is stored, the Worker sends one plain-text email through Resend's API to
  `NOTIFY_TO` (ops@sengi.run) from `NOTIFY_FROM` (`Founders form <form@sengi.run>`), both `vars` in `wrangler.jsonc`
  and `mezze/wrangler.jsonc`. The key is the secret `RESEND_API_KEY` on each Worker; make it in the team's Resend account
  with sending access only, limited to the sender's domain (sengi.run, verified there). Subject
  `<site>: <name> (<topic>)`; the body has the name and email, topic, team, time and country, and the message;
  Reply-To is the sender, so replying answers them (an address of an unusual shape is left out of Reply-To but stays
  in the body). Visitor text is plain text with control characters removed, never HTML. It is sent in
  `ctx.waitUntil` after the answer: a failed send is logged (read with `npx wrangler tail`) and the visitor still gets
  ok; the message is in KV either way. The bot trap, refused posts and rate-limited posts send nothing, so the only
  brake on how many arrive is the per-address limit above. Without `RESEND_API_KEY` (as under `wrangler dev`)
  nothing is sent and each message logs a warning.
- Changing where it goes: set `NOTIFY_TO` in both configs and deploy. A new sender needs its domain verified in
  Resend and a key that may send for it (`npx wrangler secret put RESEND_API_KEY`, and again with
  `-c mezze/wrangler.jsonc`). To stop the emails, delete that secret; messages are still kept.

Removal: delete a message with `npx wrangler kv key delete --binding CONTACT --remote "<key>"` (list keys with
`npx wrangler kv key list --binding CONTACT --remote --prefix msg:`).

## The closed waitlist

The page had a waitlist for a 30-day demand test; it has no form now. The addresses it collected stay in KV until
they are deleted (the page promised within 12 months of sign-up), and their counts can still be read.

- `POST /api/waitlist` answers 410 `{"ok":false,"error":"closed"}` and stores nothing (an old copy of the page
  may still post).
- `GET /api/waitlist/count` with `Authorization: Bearer <WAITLIST_ADMIN_TOKEN>`: `{"total","hub","capacity","both"}`,
  no emails. Refuses everyone if the secret is not set. Any KV error answers 503 `unavailable`.
- Storage: KV namespace `WAITLIST`, one key per address, `email:<address>` →
  `{"hub":bool,"capacity":bool,"joined","updated"}` (ISO times), with `hub` and `capacity` also in the key's
  metadata. `hub` meant "I'd pay for Sengi to run our pool's hub"; `capacity`, "I'd pay for extra machines when
  ours are busy".

```sh
curl -s -H "authorization: Bearer $WAITLIST_ADMIN_TOKEN" https://potluck.sh/api/waitlist/count
# {"total":42,"hub":8,"capacity":11,"both":3}
```

`total` is every sign-up; `hub` and `capacity` each count everyone who ticked that box, and `both` those who
ticked the two. Entries are unverified: before acting on the counts, read the list's domains
(`npx wrangler kv key list --binding WAITLIST --remote`) and discount throwaway, test and repeated look-alike
addresses.

Removal: people email ops@sengi.run. Delete their key with
`npx wrangler kv key delete --binding WAITLIST --remote "email:<address>"`. To delete the whole list, delete each
`email:` key the same way, then remove the `kv_namespaces` entry from `wrangler.jsonc` and the two routes from
`src/worker.js`.

## Develop

```sh
npm test                        # node --test, no install needed
npm run build                   # rebuild the blog, sitemap, feed, robots.txt and llms files
cp .dev.vars.example .dev.vars  # sets a local WAITLIST_ADMIN_TOKEN
npx wrangler dev --local        # http://localhost:8787 with a local KV store
```

If the local runtime is older than `compatibility_date`, add `--compatibility-date <the date it names>` for the
local run; do not lower it in `wrangler.jsonc`.

Look at the page at desktop (1440 wide) and phone (390 wide) widths before shipping a change, turned all the way
round (the arrows step 30 degrees; the lawn has no end stop): the scene is sized in container units (`cqw`), so
it scales as one picture, and on a phone the labels use short names. A machine is a `.m` with `data-id`, a `.pin`
with `data-pin` where its label points, and a label with `data-for` (`npm test` checks the three match).

`npm test` does not run a browser, so label placement is checked by hand with Playwright before shipping a change to
the scene: at 1440x900, 1280x720, 390x844 and 320x640, turn the lawn from 0 to 359 degrees in 1 degree steps, and at
each step check that:

- no label's box overlaps another label, the card or the turn arrows, and each sits at least 8px inside the scene;
- no label covers any part of a machine: with the labels set to `pointer-events: none`, `elementFromPoint` across each
  label's box finds no machine's face (`.f`, `.lid > *`, or a machine's `.cut`);
- leader lines don't cross each other or another machine: no two stems intersect, and `elementFromPoint` along each
  stem (labels again out of the way) finds no other machine's face or another label;
- no label sits much nearer another machine's pin than its own (it would read as that machine's);
- `elementFromPoint` at each label's centre is that label, and each machine has a point that hits it.

## Deploy

Account `246355770c9ff4630b40496b282a7b46` (set in `wrangler.jsonc`). Deploys are by hand, from this directory,
only: no git-triggered build (Workers Builds, or a Pages project on this repo) deploys this Worker.

`npm run deploy` does all of this: it fails if the build output is out of date or anything under `public/`, `content/`,
`build/` or `src/` is uncommitted, makes sure the notifier has a "before" sitemap, runs `wrangler deploy`, makes the checks below (the two admin reads only when `WAITLIST_ADMIN_TOKEN` and
`CONTACT_ADMIN_TOKEN` are set), then runs the IndexNow notifier. By hand:

```sh
npx wrangler secret put CONTACT_ADMIN_TOKEN   # once
npm run build:check                           # the committed build is current
npx wrangler deploy
for h in potluck.sh www.potluck.sh; do
  curl -s -o /dev/null -w "$h page %{http_code}\n" "https://$h/"
  curl -s -o /dev/null -w "$h waitlist post %{http_code}\n" -X POST "https://$h/api/waitlist"
done
curl -s -o /dev/null -w "favicon %{http_code}\n" https://potluck.sh/favicon.ico
curl -s -o /dev/null -w "install.sh %{http_code} %{redirect_url}\n" https://potluck.sh/install.sh
curl -s -H "authorization: Bearer $WAITLIST_ADMIN_TOKEN" https://potluck.sh/api/waitlist/count; echo
curl -s -o /dev/null -w "contact read %{http_code}\n" -H "authorization: Bearer $CONTACT_ADMIN_TOKEN" https://potluck.sh/api/contact
npm run notify
```

Expect 200 for the page, 410 for the post, 200 for the favicon, 302 to the installer's raw GitHub address for install.sh, the counts, and 200 for the contact read. After a change to the form or the email, send one
test message (with `origin: https://potluck.sh` and a content length), check ops@sengi.run got the notice, then
delete the test message's key.
