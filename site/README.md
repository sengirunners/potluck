# potluck.sh

One static page (`public/index.html`, with its stylesheet `public/site.css` and script `public/site.js`) served by a Cloudflare Worker (`src/worker.js`) with static assets. The Worker
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
label or leader), the three How it works figures (a blanket that unfolds with scroll, machines that drop into a
row, a MacBook whose menu winds down; their classes start `bk-`, `sk-` and `mb-`), the cost slider, the copy button
and the form. Without JavaScript the page still reads in full; the labels, turn arrows and copy button stay hidden,
the figures show their finished state, and the form posts as a plain form. With `prefers-reduced-motion`, nothing
moves on its own. The card's numbers are half of each machine's cores and memory,
because `potluck host` shares half unless told otherwise (`--share`).

## The founders form

The "Talk to Jackson and Clay" form posts to `/api/contact`. Messages are kept in KV; nothing is emailed on.

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

```sh
npx wrangler secret put CONTACT_ADMIN_TOKEN   # once
npx wrangler deploy
for h in potluck.sh www.potluck.sh; do
  curl -s -o /dev/null -w "$h page %{http_code}\n" "https://$h/"
  curl -s -o /dev/null -w "$h waitlist post %{http_code}\n" -X POST "https://$h/api/waitlist"
done
curl -s -H "authorization: Bearer $WAITLIST_ADMIN_TOKEN" https://potluck.sh/api/waitlist/count; echo
curl -s -o /dev/null -w "contact read %{http_code}\n" -H "authorization: Bearer $CONTACT_ADMIN_TOKEN" https://potluck.sh/api/contact
```

Expect 200 for the page, 410 for the post, the counts, and 200 for the contact read.
