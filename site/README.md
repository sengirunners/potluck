# potluck.sh

One static page (`public/index.html`) served by a Cloudflare Worker (`src/worker.js`) with static assets. The Worker
answers the two routes of the closed waitlist (`src/waitlist.js`, kept apart because a Worker's entry module may
export only handlers); every other path is a static file. No framework, no dependencies, no tracking scripts, and no
third-party requests: the font (Newsreader, OFL, latin subset, variable, one file each for roman and italic) is
self-hosted in `public/fonts` with its licence. The picnic scene at the top is built in CSS 3D: the machines are boxes
whose sides darken as they turn away from a fixed sun (CSS `cos()`, with fixed shading where that is missing), and the
people, food and cloud are inline SVG cut-outs that keep facing the reader. The page's only script is inline: it runs
the scene (pick a machine, drag the lawn or press the arrows to turn it), places the machines' labels in a flat layer
from where each machine lands on screen (front to back, each at the cheapest spot: never over another label, the card
or the arrows, and off every machine's body where there is room), the cost slider and the copy button. Without
JavaScript the page still reads in full; the labels, turn arrows and copy button stay hidden. With
`prefers-reduced-motion`, nothing moves on its own. The card's numbers are half of each machine's cores and memory,
because `potluck host` shares half unless told otherwise (`--share`).

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

Look at the page at desktop (1440 wide) and phone (390 wide) widths before shipping a change, turned to both ends
(the arrows step 15 degrees; the lawn turns at most 45 either way): the scene is sized in container units (`cqw`), so
it scales as one picture, and on a phone the labels use short names. A machine is a `.m` with `data-id`, a `.pin`
with `data-pin` where its label points, and a label with `data-for` (`npm test` checks the three match).

`npm test` does not run a browser, so label overlap is checked by hand with Playwright before shipping a change to the
scene: at 1440x900, 1280x720, 390x844 and 320x640, turn the lawn from -45 to 45 degrees in 5 degree steps (drag it),
with Leo away and back, and at each step check that no label's box overlaps another label, the card, or the box of
any machine's face (`.f`, `.lid > *`, or a machine's `.cut`), and that `elementFromPoint` at each label's centre is
that label.

## Deploy

Account `246355770c9ff4630b40496b282a7b46` (set in `wrangler.jsonc`). Deploys are by hand, from this directory,
only: no git-triggered build (Workers Builds, or a Pages project on this repo) deploys this Worker.

```sh
npx wrangler deploy
for h in potluck.sh www.potluck.sh; do
  curl -s -o /dev/null -w "$h page %{http_code}\n" "https://$h/"
  curl -s -o /dev/null -w "$h waitlist post %{http_code}\n" -X POST "https://$h/api/waitlist"
done
curl -s -H "authorization: Bearer $WAITLIST_ADMIN_TOKEN" https://potluck.sh/api/waitlist/count; echo
```

Expect 200 for the page, 410 for the post, and the counts.
