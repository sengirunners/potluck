# potluck.sh

One static page (`public/index.html`, with its stylesheet `public/site.css` and script `public/site.js`) and a Markdown blog (`content/blog/`, built into `public/blog/`; see "The blog and indexing") served by a Cloudflare Worker (`src/worker.js`) with static assets. The Worker
answers the founders form (`src/contact.js`) and the two routes of the closed waitlist (`src/waitlist.js`), kept
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
`content/`, `build/` or `index.html`'s seo block, and commit the result. There is no post yet, and no placeholder: with
none, the blog page, the feed and the home page's Blog link are not built.

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

`drafted:` and `path:` (a refresh's `/blog/<slug>`) are accepted too. A missing or malformed field, an unknown field, or
a slug that is not the file name fails the build. The body is the Markdown subset `build/markdown.mjs` reads: `##` to
`######` headings (not `#`: the title is the page's h1; a first line that repeats the title is dropped), paragraphs,
bullet and numbered lists, fenced code, blockquotes, pipe tables, and inline code, bold, italic and links. Anything
else (raw HTML, images, nested lists, rules, indented code, strikethrough) fails the build with the line number rather
than rendering wrong. Put the answer first: the first paragraph, or `summary`, is what a search result or an assistant
quotes.

What the build writes:

| file | what |
| --- | --- |
| `public/blog/<slug>.html`, `public/blog.html` | the post and the index, served at `/blog/<slug>` and `/blog` (no trailing slash: Workers assets redirect the `.html` and slash forms, so the canonical address is the 200 one) |
| `public/blog/<slug>.md` | the post as Markdown, for assistants |
| `public/sitemap.xml` | every public page with `<lastmod>`: the home page's comes from `HOME.lastmod` in `build/pages.mjs` (bump it when the home page's content changes), posts' from `updated`; `/blog` only when a post exists |
| `public/feed.xml` | Atom, posts newest updated first |
| `public/robots.txt` | allow all; the search and AI crawlers named (GPTBot, OAI-SearchBot, ChatGPT-User, ClaudeBot, Claude-SearchBot, PerplexityBot, Google-Extended, Bingbot); `/api/` closed; the sitemap |
| `public/llms.txt`, `public/llms-full.txt` | `content/llms-intro.md` plus the page list; and the intro plus every page's Markdown (`content/home.md` for the home page; keep it in step with `index.html`) |
| `public/index.html` (between its markers) | title, description, canonical, Open Graph, Twitter tags and JSON-LD (SoftwareApplication, Organization, WebSite), and the footer's Blog link |

`public/og.png` (1200x630, the share image) and `public/blog.css` are hand-made. `docs/img/readme-hero.webp`, the
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

- Email (`src/notify.js`): after a message is stored, the Worker sends one plain-text email through the `NOTIFY`
  binding (Cloudflare `send_email`) to ops@sengi.run, the binding's `destination_address`, from `NOTIFY_FROM`
  (`vars` in `wrangler.jsonc`). Subject `potluck.sh: <name> (<topic>)`; the body has the name and email, topic,
  team, time and country, and the message; Reply-To is the sender, so replying answers them (an address of an
  unusual shape is left out of Reply-To but stays in the body). Visitor text is plain text with control
  characters removed, never HTML. It is sent in `ctx.waitUntil` after the answer: a failed send is logged (read
  with `npx wrangler tail`) and the visitor still gets ok; the message is in KV either way. The bot trap,
  refused posts and rate-limited posts send nothing, so the only brake on how many arrive is the per-address limit
  above. Under `wrangler dev` the binding is simulated and nothing leaves the machine; with the `send_email` entry
  removed, nothing is sent and each message logs a warning.
- Changing where it goes: set `destination_address` in `wrangler.jsonc` (the address must be a verified
  destination in Cloudflare Email Routing for this account: `npx wrangler email routing addresses list`), and
  `NOTIFY_FROM` to an address on a domain or subdomain with Email Routing (or Email Sending) turned on in this account, then
  deploy. The sender is `form@notify.potluck.sh`: Email Routing is on for the subdomain `notify.potluck.sh`
  only (its own MX and SPF records, added by Cloudflare), so potluck.sh's own mail records are not involved. Check
  it with the Email Routing API (`GET /zones/<potluck.sh zone>/email/routing/dns?subdomain=notify.potluck.sh`);
  `wrangler email routing settings` and `dns get` take only a domain. To stop the emails, remove the `send_email`
  entry and deploy; messages are still kept.

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
curl -s -H "authorization: Bearer $WAITLIST_ADMIN_TOKEN" https://potluck.sh/api/waitlist/count; echo
curl -s -o /dev/null -w "contact read %{http_code}\n" -H "authorization: Bearer $CONTACT_ADMIN_TOKEN" https://potluck.sh/api/contact
npm run notify
```

Expect 200 for the page, 410 for the post, the counts, and 200 for the contact read. After a change to the form or the email, send one
test message (with `origin: https://potluck.sh` and a content length), check ops@sengi.run got the notice, then
delete the test message's key.
