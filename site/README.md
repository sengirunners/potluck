# potluck.sh

One static page (`public/index.html`) served by a Cloudflare Worker (`src/worker.js`; the waitlist code is in
`src/waitlist.js`, since a Worker's entry module may export only handlers) with static assets. The Worker handles
the waitlist API and the page's no-JavaScript result line; every other path is a static file. No framework, no
dependencies, no tracking scripts, and no third-party requests: the font (Newsreader, OFL, latin subset,
variable, one file each for roman and italic) is self-hosted in `public/fonts` with its licence. The page's only script is the
inline one that posts the waitlist form.

## Waitlist

For a 30-day demand test. Everyone who signs up hears
when Potluck can be installed. Two optional ticks, counted independently:

| field | on the page |
| --- | --- |
| `hub` | I'd pay for Sengi to run our pool's hub (the machine that hands out work), so none of ours has to stay on |
| `capacity` | I'd pay for extra machines when ours are busy |

- `POST /api/waitlist`: form or JSON body with `email`, `hub`, `capacity` and the honeypot `website`. A tick is
  `on`, `yes`, `1`, `true` or JSON `true`; anything else is unticked. Neither tick is a valid sign-up. Bodies over
  2 KB, bad emails and posts from another site's page are refused. A filled honeypot gets a fake success. At
  most 5 posts a minute per IP (the `WAITLIST_LIMITER` binding; the IP is never stored).
- Answers: when the Accept header names `application/json` (the page's script), `{"ok":true}` or
  `{"ok":false,"error":"..."}`. Any other post (a plain form post, curl's default `*/*`) gets a 303 to
  `/?joined=1#waitlist` or `/?error=<code>#waitlist`, and the Worker fills in the page's result line from fixed
  words (so `/` runs the Worker first, `run_worker_first` in `wrangler.jsonc`). Neither ever echoes what is
  stored, nor whether the address was already on the list.
- Email: trimmed and lower-cased for the key. Printable ASCII only, at most 254 characters; the local part is
  `` [A-Za-z0-9._%+'!#$&*/=?^`{|}~-]{1,64} `` and may not start with `=`, `+` or `-` (spreadsheet formulas); the
  domain is letters, digits, hyphens and at least one dot.
- Storage: KV namespace `WAITLIST`, one key per address, `email:<address>` →
  `{"hub":bool,"capacity":bool,"joined","updated"}` (ISO times), with `hub` and `capacity` also in the key's
  metadata. A repeat sign-up may add a tick but never clears one, so nobody can zero someone else's answers; it
  writes (and moves `updated`) only when it adds one.
- Any KV or rate-limiter error answers 503 `unavailable` (or `/?error=unavailable`), never Cloudflare's error page.
- `GET /api/waitlist/count` with `Authorization: Bearer <WAITLIST_ADMIN_TOKEN>`: `{"total","hub","capacity","both"}`,
  no emails. Refuses everyone if the secret is not set.

Removal: people email ops@sengi.run. Delete their key with
`npx wrangler kv key delete --binding WAITLIST --remote "email:<address>"`. The page promises the list is deleted
within 12 months.

## Develop

```sh
npm test                        # node --test, no install needed
cp .dev.vars.example .dev.vars  # sets a local WAITLIST_ADMIN_TOKEN
npx wrangler dev --local        # http://localhost:8787 with a local KV store
```

If the local runtime is older than `compatibility_date`, add `--compatibility-date <the date it names>` for the
local run; do not lower it in `wrangler.jsonc`.

```sh
curl -H 'accept: application/json' -X POST localhost:8787/api/waitlist -d 'email=you@example.com&hub=yes'
curl -i -X POST localhost:8787/api/waitlist -d 'email=bad'   # 303 to /?error=email#waitlist
curl -H 'authorization: Bearer local-dev-token' localhost:8787/api/waitlist/count
```

## Deploy

Account `246355770c9ff4630b40496b282a7b46` (set in `wrangler.jsonc`). Before the first deploy, confirm:

- The account is on the Workers Paid plan. The free plan allows 1,000 KV writes a day, and one abuser rotating
  addresses can use them up (the page then answers "unavailable" until the next day).
- No git-triggered build (Workers Builds, or a Pages project on this repo) deploys this Worker: deploys are by hand,
  from this directory, only.

One-time setup, from this directory:

1. Create the KV namespace and put the id it prints into `wrangler.jsonc` in place of `REPLACE_WITH_KV_ID`,
   then commit that:
   `npx wrangler kv namespace create WAITLIST`
2. Set the admin token (keep a copy somewhere safe; it is needed to read the counts):
   `openssl rand -hex 32 | npx wrangler secret put WAITLIST_ADMIN_TOKEN`
3. Deploy: `npx wrangler deploy`
4. Smoke test both hostnames, then delete the test keys:

   ```sh
   for h in potluck.sh www.potluck.sh; do
     curl -s -o /dev/null -w "$h page %{http_code}\n" "https://$h/"
     curl -s -H 'accept: application/json' -X POST "https://$h/api/waitlist" -d "email=smoke-test@$h&hub=yes"; echo
     curl -s -o /dev/null -w "$h form %{http_code} %header{location}\n" -X POST "https://$h/api/waitlist" -d 'email=bad'
   done
   curl -s -H "authorization: Bearer $WAITLIST_ADMIN_TOKEN" https://potluck.sh/api/waitlist/count; echo
   for h in potluck.sh www.potluck.sh; do
     npx wrangler kv key delete --binding WAITLIST --remote "email:smoke-test@$h"
   done
   ```

   Expect 200, `{"ok":true}`, and a 303 to `/?error=email#waitlist` for each host.

The rate limit binding needs no setup; it exists once the Worker is deployed. Later deploys are steps 3 and 4.

## Read the counts

```sh
curl -s -H "authorization: Bearer $WAITLIST_ADMIN_TOKEN" https://potluck.sh/api/waitlist/count
# {"total":42,"hub":8,"capacity":11,"both":3}
```

`total` is every sign-up; `hub` and `capacity` each count everyone who ticked that box, and `both` those who
ticked the two. The plan's bar: under about 20 `hub` sign-ups in 30 days, pause the paid-path work. KV lists can
lag writes by up to a minute.

Entries are unverified: nobody confirms they own the address. Before acting on the counts, read the list's
domains (`npx wrangler kv key list --binding WAITLIST --remote`) and discount throwaway, test and repeated
look-alike addresses.
