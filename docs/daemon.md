# The daemon around a driver

What Potluck's daemon does with a driver: bindings, which code goes where, roles, leases and budgets, boxes served
from other machines, the GitHub runner broker, and the client commands built on the protocol. None of this is part
of the driver protocol (`docs/protocol.md`); a driver author needs only that. Sharing a machine is in
`docs/hosts.md`, box networking in `docs/network.md`.

The daemon side lives in `runtarget/` (run targets), `runners/` (the GitHub runner broker), `usage/`, `grants/`,
`boxpush/`, `boxcp/` and `internal/e2bgateway/`. `$POTLUCK_HOME` is the daemon's home (zm's was `$ZM_HOME`).

## One daemon per machine

A daemon built on Potluck's kit is the machine's Potluck daemon, whichever program runs it: `potluck daemon`, or a
program built on Potluck that adds its own modules to the same wiring. The `potluck` command and the menu bar item
find it by one rule and never start a second daemon beside it.

**The record.** From the moment its socket listens, a daemon keeps one record for the machine in
`~/.local/state/potluck/daemons/<program>.json` (`$POTLUCK_DAEMONS_DIR` names another directory). The directory
is 0700 and each file 0600, replaced whole; a reader ignores a file that is not this user's alone and refuses a
directory that is not, since a record names a socket to dial and a program to run. It names the program, its binary, its home, its socket, its pid, its
protocol and version, and the modules its routes come from:

```json
{
  "program": "potluck",
  "exe": "/Users/ana/.local/bin/potluck",
  "home": "/Users/ana/.potluck",
  "socket": "/Users/ana/.potluck/potluck.sock",
  "pid": 4242,
  "protocol": "potluck/1",
  "version": "v0.4.0",
  "modules": ["core", "grants", "owners", "runners", "runtarget", "tailnet", "teamlink", "tunnel", "usage"],
  "started": "2026-11-17T09:00:00Z"
}
```

The daemon removes its record when it stops. A record counts only while its process is this user's and alive and
its socket is there and does not answer as another process (a daemon too slow to answer, or on another protocol major, counts as up); a crashed daemon's record is ignored, and the next daemon of that program replaces it. A
second daemon of the same program on another home leaves a live record alone. The code is `kit/daemonrecord`;
`kit/server` writes it.

**Which daemon `potluck` uses**, decided once per command (`cli/attach.go`):

1. `POTLUCK_BOX_CREDENTIAL` set: the authority that credential names, and no daemon on this machine.
2. `POTLUCK_HOME` set: that home. When a daemon of another program that serves compute runs on it, potluck uses
   that daemon; else (also when the records cannot be read) potluck's own.
3. Potluck's own home (`~/.potluck`) exists: potluck's own daemon, started when a command needs it.
4. Exactly one daemon of another program that serves compute runs (its record has every module of
   `wiring.ComputeModules`): potluck uses it. Two or more: potluck refuses and names them; choose one with
   `POTLUCK_HOME=<its home>`.
5. Else potluck's own home, made on first use. Where a daemon built on Potluck has run before (the records
   directory exists), the command that would make that home first waits up to 3 seconds for one restarting.

Using another program's daemon, potluck sends every call over that daemon's socket and reads that home's
`config.json` but writes nothing there. It never starts that program, and never starts `potluck daemon` on that
home. `potluck status` says whose daemon it uses. These refuse, saying which program owns the daemon: `potluck
daemon` (in the foreground or with `--detach`), `daemon stop`, `daemon preflight`, `driver pin`, `isolation test`
and `host shelf`. `potluck upgrade` installs potluck's own programs and leaves the daemon, its home and its
driver pins as they are; it does the same when potluck has no home yet, so installing potluck never makes
`~/.potluck`.

**A home has one program.** The first daemon on a home writes its program's name into `<home>/program`; a daemon
of another program refuses that home before it opens anything there. With `POTLUCK_HOME` naming such a home while
its daemon is not running, potluck starts no daemon there and refuses the commands above.

**A home has a term.** `<home>/term.json` (`kit/hometerm`) holds an epoch and a state: `serving`, `frozen` (stopped
for a move to another home), `fenced` (a named successor serves at a higher epoch) or `standby` (a copy that serves
only once promoted). A home with no file serves at epoch 0. A daemon starts only on a serving home, before it claims
the home or opens its store; a term file it cannot read counts as not serving. The file is not on the list of files an off-site
backup set holds (zm `offsite.HomeFile`, an allow-list), so a home restored on another machine does not take the old machine's
term. Nothing in this build writes a
term other than tests: the commands that move a home and promote a standby are planned on bead zm-d3cnz.

## project.toml and settings this build does not read

`$POTLUCK_HOME/projects/<id>/project.toml` holds a project's own settings (`id`, `slug`, `name`, `paths`, `remotes`,
`[[areas]]`) and the settings of modules, such as `[[run_targets]]` below. Another program built on Potluck can add
its own top-level keys and tables.

A Potluck-only daemon keeps every setting it does not understand and writes it back with the same values, but it
does not check it: the module that owns a setting checks it, in the build that has that module. So a file can load
here and be refused there. Three things tell you a setting is not read:

- at start the daemon logs one warning per project that names them;
- `potluck doctor` lists them per project;
- a name that is one slip from a name this build reads (`path`, `Slug`, `area`) is refused as a typo, with the name
  it meant. Any other unknown name is kept, a misspelt one included.

A `PATCH /v0/projects/<project>` that carries a setting no module in this build takes (`trackers`, `guest_issues`)
is refused as a bad request; nothing is stored.

## Bindings

A project uses a driver through a binding: a `[[run_targets]]` entry in `project.toml`.

- Only the machine's owner sets it, on the socket without a token, like tracker binaries and config. An owner token
  or a tailnet owner gets `forbidden`.
- A binding belongs to exactly one project.
- Its keys:
  - `name`: unique in the project; the `<target>` in handles;
  - `driver`: one driver may serve several bindings and kinds;
  - `kind`;
  - the class: `isolation` (`"none"` or `"ephemeral"`) and `credentials` (bool);
  - `repo` (one of the project's repos; its remote is the one the daemon asks) and `branch` (that repo's protected
    default branch, such as `main`);
  - `allow_any_ref` (default false);
  - `scripts` (run; required when the effective class has `credentials` true or `isolation` `"none"`, checked at
    bind);
  - `budget`, `ttl_max_s`, `handles_max`;
  - for db, `create_in`;
  - anything else is driver config.
- `config` on every call carries the driver config plus `target` (the binding name) and `kind`; `describe` answers
  for that binding.
- The daemon uses the stricter class of the binding and `describe` (`none` over `ephemeral`, `true` over `false`): a
  target never declares itself safer than its owner did.
- One-time work uses the driver's optional `setup`, called only by the owner's bind (a `dry_run` first; what it
  changes outside Potluck is in `describe.setup.touches`), never on `acquire`.

## Which code goes where

Code, not callers, decides where a run goes. Every socket caller is the owner, so who asks proves nothing; the rule
is about the code a target runs.

- A run's caller names a `ref`, never the commit. At acquire time the daemon resolves `ref` by a live lookup on the
  binding's remote (`git ls-remote`, never a local tracking ref) and sends the result as `commit`. A caller that sends
  a commit differing from it gets `forbidden`.
- Every run needs a pushed commit: no `ref`, or one the remote cannot resolve, is `bad_request`.
- The code is trusted only when `commit` is the remote head of the binding's `branch`.
- Any other commit goes only to an untrusted-capable target (class `isolation: "ephemeral"`, `credentials: false`),
  which fetches it from the remote. Elsewhere it is `forbidden`, unless the binding says `allow_any_ref`: the only
  override, owner-only on the socket like every binding key.
- Every run target runs a clean checkout of exactly `commit`, never a caller's working tree, and echoes it in
  `acquire` and `status`.
- A run's `script` is a name the target resolves (an npm script, a make target), never a shell string, and `args` are
  literal arguments. That is hygiene, not protection: the code at `commit` defines what the script does, so the
  protection is the routing above.

### Credentialed or unisolated targets

Effective class `credentials: true` or `isolation: "none"`:

- `acquire` is local-only: the socket owner, as spawning is. A `"none"` target runs as some user on some machine, and
  a UID is no boundary.
- A script not in the binding's `scripts` is `forbidden`.

### Databases

- A db target serves no code, so for a db `ref` is advisory: the daemon routes by it as above (no ref, or an
  unresolvable one, counts as untrusted), but nothing ties the database's later users to it.
- For a db, `credentials: true` means the server holds something beyond the throwaway copies (other databases, a
  role that reaches them).
- Every db `acquire` is local-only, so a `url` never crosses the network.
- A db's `url` is a secret: the daemon returns it only in `acquire` answers (a repeat with the same `request_id` from
  the socket gets it again), never in `status`, `list`, events, logs, mail or stderr.

### Boxes

A box runs whatever its holder sends, so it is untrusted code by construction.

- A `box` binding is refused at bind unless its effective class is `isolation: "ephemeral"`, `credentials: false`,
  and so is a `describe` saying otherwise.
- With `ref`, the daemon resolves it and sends `commit`, as for a run. A binding without `repo` refuses a `ref` with
  `bad_request`; Potluck's clients push such a box the renter's commit instead ("Code from the renter").
- `scripts` and `allow_any_ref` do not apply.
- `size` is the binding's one size (driver config `size`; a host offering two binds twice), and `describe.size`
  states it.
- `image` names one of the binding's owner-set, digest-pinned `images` (default the first); anything else is
  `bad_request`. The pins go to the driver as config `images` (comma-joined), and a booted digest off the pin is
  released at once.

## Roles

- `acquire` needs `operate` on the project (it spawns work and spends).
- `describe`, `status` and `list` need `read`.
- `renew` and `release` are the lease holder's. The owner may force `release` on any handle; the daemon's own lease
  verbs refuse these keys.
- `actor` is the identity the acquire is made for.
- `exec`, background processes and terminals are the holder's, like `renew`.

## Leases

- The daemon holds each handle as a lease keyed by the handle (those keys are reserved: only `acquire` creates them),
  whose deadline is the target's `expires_at`.
- A session-bound lease renews while its session exists, even when idle (a session waiting on a long run goes idle),
  by calling `renew` only once a third of the TTL is left and at most once a minute per handle.
- When the session ends, renewal stops, and the target ends the resource at `expires_at`. Without `renew` a lease
  cannot be extended.
- An acquire with `self_renew` says its client renews the handle itself: the daemon never renews it, so a client that
  dies lets it end at `expires_at` though its session lives on.
- The daemon's leases survive a restart, but the daemon may be down at the deadline: the driver enforces it.
- Expiry publishes one event and nothing else: no notice to the holder, since a run reaching its TTL is normal. It
  is `dev.zm.run_target.expired` (`computeapi.EventRunTargetExpired`, `runtarget/sweep.go`), with a `Lease` keyed
  by the handle, holder the actor.
- A reclaim ends the lease the same way: the daemon learns of it when `status` answers `reclaimed` (it asks when a
  `renew` is refused, and at the deadline), drops the lease and publishes that event.

### Before the handle

- `GET /v0/run_targets/{target}/acquires/{request_id}` answers how far the caller's own acquire with that
  `request_id` got (`computeapi.RunTargetAcquireProgress`, `runtarget/acquires.go`): `ready` with the handle once the
  daemon holds it; `starting` with phase `queued` while it waits behind another acquire of the binding; else the
  driver's `progress` (docs/protocol.md, Acquire progress and cancel). A driver without capability
  `acquire-progress`, or a lending host whose daemon does not relay the verbs, gets `starting` with no phase.
  The driver's `message` is shown only for an acquire this daemon is serving that carried no `jit_config` (a
  driver could quote what it was sent); a lending host drops it before the answer crosses the tunnel.
  Another caller's key, or one never sent, is `not_found`. The machine's owner may name `?actor=` for an acquire it
  made with `actor`, as acquire allows.
- `POST /v0/run_targets/{target}/acquires/{request_id}/cancel` stops it: a queued acquire never reaches the driver;
  the driver's `cancel` stops one in flight and refuses a later acquire with that key (`conflict`); the daemon stops
  a queued one only once the driver has said `cancelled: true`. An acquire that
  already has its handle answers `cancelled: false` with the handle: release it. Once a cancel answered
  `cancelled: true`, a handle the acquire gets anyway (a driver that could not end it, or an acquire cut off at its
  time limit after it made the resource, whose retry was refused) is released by the daemon; one it cannot release
  stays held, and the cancel or acquire answers it. Such a handle, once released, makes `progress` and a second
  `cancel` answer `cancelled`. The daemon looks in the driver's `list` for what the acquire made whenever its
  driver call failed after the cancel was sent (a crash too), and when no acquire it serves is with the driver (it
  restarted); an acquire whose `list` failed too is left to the sweep, which ends what it finds once no acquire
  could still be with the driver. A driver without the capability is `not_implemented`.
  The daemon answers a repeat of a cancel that stood itself, without the driver, for an acquire it serves or has a
  row for (a repeat of a key it never received goes to the driver again, and counts again). The driver keeps a cancel of a
  key it never received for at least 24 hours, so a caller may send at most 64 cancels of keys no driver was sent
  (no acquire the daemon serves has asked its driver, and no row has the key) in 24 hours (then `rate_limited`; the count is kept
  in memory, so a restart clears it); a lending host, which cannot tell a known key from a new one, relays at most 256 cancels per actor and 1024 per project in 24 hours (then `rate_limited`, saying when to retry). A driver's refusal of `progress` or `cancel` is passed on without its words
  (it could quote what the acquire was sent); the daemon's own errors keep theirs.
  A lending host passes on a progress or cancel answer's state and phase only, and a refusal's code without its words.
- An acquire the driver has been sent goes on when its caller goes away (one still queued does not). A repeat with
  the same `request_id` waits for the first and answers its handle, so a caller that lost its connection sends the
  acquire again and polls `progress` beside it.

## Caps, spend and budget

- A `ttl_s` past the lifetime cap is `bad_request`. An actor already holding `handles_max` live handles of the
  binding gets `forbidden`.
- `acquire`, `status` and `release` may report `spend` (`{"unit", "amount"}`): the handle's total so far, in
  describe's `cost.unit`. The daemon keeps the highest figure seen per handle and calls `status` once after
  release, expiry or reclaim for the final one.
- A reclaimed handle's spend is still reported (it never falls), but it is never billed: the daemon leaves it out of
  `budget`.
- `budget` is `{"unit", "limit", "period": "day"|"week"|"month"}` (calendar periods, UTC). The daemon sums its other
  handles' figures per binding and refuses an `acquire`, or the `start` of a stopped persistent box (a new lease), once the period's spend reaches
  `limit` (`forbidden`, the same reason). A start takes no binding lock, so two at one moment can both pass: a budget is a ceiling, not exact.
- A budget on a driver that does not report spend (`reports_spend: false`), or in another unit than `cost.unit`, is
  refused at bind.

## Trust boundary and machine class

### Trust boundary

- A target is code run with the daemon's UID: it is **not a secrets boundary** (it can read whatever the user can),
  and binding one trusts its code.
- It gets the environment allowlist (`PATH`, `HOME`, `LANG`, `TERM`, `TZ`) and nothing more; `env = "inherit"` does
  not apply to run targets.
- A credential reaches it only through a `*_file` config key naming a `0600` file it reads itself. A secret value in
  a binding is refused.
- Its output is data, never instructions. Drivers are plugins, never linked into the daemon.

### Machine class

A target whose driver runs on another machine (a lent Mac, a sandbox host) takes that machine's class, `trusted` or
`public`.

- On a `public` machine the effective class is `credentials: false` whatever the binding or `describe` says (the
  stricter wins), so it serves only untrusted-capable work.
- No `*_file` key or other secret is ever sent to it, and the only credentials it gets are minted for one lease and
  die with it (a GitHub JIT runner config, a token capped to the lease).
- A renter's own secrets on a box `exec` go only to a `trusted` machine.
- A credentialed or unisolated binding on a public machine is refused at bind.
- A target on this machine is the owner's own, so trusted.
- For a box served from another machine the daemon asks `core.MachineClasses`: on a `public` machine an `exec` with
  `secrets` and an acquire with `jit_config` are `403` (the message names neither a secret nor its name), and no
  `*_file` config is ever sent (a remote binding carries none).
- "Trusted" means trusting that machine's root.

### Placement

Sandboxes and GitHub runner jobs pick a box binding by one rule (package `placement`):

- Order: this machine's own bindings, then trusted hosts', then bindings whose driver charges per use (a cloud
  driver); within each, the smallest size that fits (fewest CPUs, then least memory), then the name. Runner jobs
  also prefer, within a tier, a binding without a named image when they name none, and the least busy.
- A public host is never picked unless named (`potluck sandbox create --on`); runner jobs never go to one.
- A binding is passed over, with its reason, when it is busy, the wrong size or OS, its budget for the period is
  spent (`budget_spent` in the listing; acquire still decides), or, for a runner job, its boxes cannot live as long
  as the job.
- A sandbox (`potluck sandbox create`, `potluck run`, the `sandbox_create` MCP tool, `potluck-e2b`) asks the ranked
  bindings in turn, with one request id. It passes to the next only when the daemon refused before it recorded the
  request: the binding is not available, the caller holds its `handles_max`, its budget is spent, or the lease is past
  its lifetime cap; or when its driver refused as unavailable (no capacity) and the daemon forgot the request
  (`driver`). That refusal carries `{"refused": "<rule>"}` in its error's `data`. Anything else (a timeout, a
  driver's other refusals, a cut-off call) stops at that binding, since its box may still come up there; a retry with
  the same request id answers it. A request id names one box acquire across the project's bindings: asked of another
  binding, the daemon refuses with `conflict` and `{"used_on": "<binding>"}` in `data`, and the walk asks that one,
  whatever `--on`, size or OS the retry names. Only an ended box of a binding since removed frees the id.
- No room now is a wait, not a failure. A binding that is not available only for now (every slot in use, its `why`
  the driver's `no free <os> slot`, or its host's reclaim policy, host mode `auto`, holding new work:
  `RunTargetDescribe.FullNow`), or whose driver answered
  `unavailable` with `no_room` data (a free slot taken first, the host's admission controller's no), is refused as
  `{"refused": "no_room", "on": "<binding>", "why": "<the host's words>", "retry_s": N}` (`retry_s` the
  controller's, when it gave one). Placement passes over such a binding and takes another that has room. When none
  has room and one that is only full would fit, `potluck sandbox create`, `potluck run`, `potluck agent-task` and `potluck host`'s
  check boxes wait (package `roomwait`). They ask again at the controller's retry time (5 s to 10 min), or backing off from
  10 s to 2 min. Each ask is a real acquire, so the controller still counts the demand that waits. For each change
  they print one line: `<binding> has no room: <why>; asking again at 05:41:00 (waited 4m of 30m)`. They wait up to
  `--wait` (default 30m; agent-task's default is 0, so it waits only when asked; `--wait 0` fails at once). The bound or Ctrl-C ends the wait with the last
  answer and nothing held. The daemon that marks a no room says it from these fields, never in the driver's or
  the host's own words: `<binding> has no room now: <why>; it says to ask again in 600 s`. A hard refusal (`bad_request`, `forbidden`, an isolation refusal, a size no machine
  offers, a spent budget) never waits, nor does a binding its owner paused, drained or closed (`potluck host set`).

## Limited tokens

A token may carry a route limit: a name, such as `answer`. A limited token reaches only the routes that admit its
limit by name, and nothing else.

- **Admitting.** A module admits a limit on its routes with `Router.Admit(limit, patterns...)`. `GET /v0/whoami`
  admits every limit, and its answer names the limit (`limit`). A GET pattern also answers HEAD. At start the daemon
  refuses an admitted pattern that is not a registered authed route, or a malformed limit name.
- **The choke point.** Right after authentication, before the request gates, the team proxy and the router, the
  daemon refuses a limited token on any route that does not admit its limit: 403 `forbidden` with data
  `{"reason":"token-limit"}`. This covers routes, unmatched paths and the fallback. A public route runs with no
  subject and no `Authorization` header. The router's guard checks the limit again. A sealed body is never opened
  for a limited token: a sealed call with one is refused with the same 403.
- **One device.** A limited token works only over the tailnet, from the one device named at mint. The device is
  matched by its stable node ID and its node key. Any other caller gets the same 401 as an unknown token: the socket,
  the tunnel, a listener without WhoIs, another node, a re-keyed node, and this host. So does a revoked or expired
  token, and one whose identity no longer holds a whole-project grant of at least the token's role.
- **Minting.** `POST /v0/tokens` with `limit`, `project`, `node` and `ttl_seconds`, on the socket only, and not by a
  session acting through `Zm-As`. The token is for the caller's own identity (`as` must be empty). That identity must already own the whole project by its own
  grant. The token is stored with role owner, and no grant or identity is added. The limit must be one some route
  admits. The TTL is required and at most `limited_token_max_ttl_seconds` in config.json (default 30 days).
  The daemon resolves `node` among its tailnet peers by the name the tailnet gave it (the full MagicDNS name or its
  first label), never by the host name a device reports for itself. It refuses
  this daemon's own node, a tagged node, a node named as a daemon's (`zm-*`, `potluck-*`), a node that serves subnet
  routes or is an exit node, a node shared in from another tailnet, and a name two peers answer to. The answer
  shows the device's name, login and OS for the minter to check. These are checked at mint only; a device that
  changes later keeps its token until it is revoked or expires.
- **Older code.** The secret is `potluck_<limit>_` plus 52 base32 characters, which the older 52-character check
  refuses. Its stored hash is domain-separated (`authz.HashLimited`), so no lookup by the plain hash finds the row.
  A build without limits, or a backup it restores, never treats a limited token as a full one. For the same
  reason a limited token is revoked by its id (`DELETE /v0/tokens/<id>`, listed by `GET /v0/tokens`), not by a
  hash of its secret.
- **Other readers.** Every other reader of the tokens table refuses a limited row (subscriptions, token scope,
  liveness and expiry) or only ever reads node-bound rows, which a limited row never is. `TestTokenReaders` lists
  them, so a new reader must decide.

## Secrets (the daemon's side)

The driver's side is in `docs/protocol.md`, "Secrets".

- The daemon sends `secrets` only to a target on a `trusted` machine. Every binding's driver runs on its own machine
  today, the owner's, so trusted; once a binding is served from another machine, secrets go to it only when that
  machine is `trusted`, else `403`.
- Neither `secrets` nor a `jit_config` goes to a machine that can seal in the clear, on any leg or transport. They
  go sealed to the receiving daemon's certified key (`kit/sealbox`, "Sealing between daemons" below): a member's
  machine seals them to its pool's machine, and the pool's machine seals an op holding them to the host that runs
  the box. The tunnel and the tailnet use the same code.
- A machine too old to seal (it names no seal key) is treated by the pool's sealing mode ("Sealing modes" below):
  over the tunnel it is refused in every mode, as before; over the tailnet `preferred` (a pool started before new pools began at `required` has it, until its admin sets `required`) sends to it as
  before, in the clear, and `required` (a new pool starts with it) refuses (`403` naming that machine as too old; the op never runs). Calls without
  secrets work in every mode.
- The CLI's call to its own daemon (the socket) is not sealed: it never leaves the machine, and the daemon is the
  caller's own. Sealing starts at the first leg that does.
- Before anything is sent, the pool's machine checks them, and a `jit_config`, against its secret grants (below).
- The daemon never stores, logs, meters or echoes a value. The ledger (below) records names and sizes, never a value.
  A refusal names neither a value nor the names given; a malformed `secrets` field is refused without quoting it.
- A driver's own refusal reaches the caller with every name and value of 4 bytes or more replaced by `[secret]` (and
  a `jit_config` by `[jit_config]`), scrubbed on the machine that runs the driver, before a relayed op's report leaves
  it.
- Values come from the renter's own client, which reads its own secret references: `potluck sandbox exec --secret
  NAME=@file` or `--secret NAME` for `$NAME`, never a value on the command line. Potluck keeps no secret store.

### Sealing between daemons

- The primitive: HPKE (RFC 9180, Go's `crypto/hpke`) in base mode, X25519, HKDF-SHA256 and ChaCha20-Poly1305, one
  suite. Each daemon's seal key (`$POTLUCK_HOME/tunnel/seal`, `0600`; a key file group or others may read is refused,
  naming the `chmod 600` that fixes it) is certified by its ed25519 identity key (`tunnel/key`). The info binds both
  ends' key ids, an expiry and the purpose.
- A daemon that cannot read its own seal key sends and passes on no secrets (`503`, logged as an error): a fault to
  fix, never treated as an older build.
- A member's call: the whole body, sealed to its pool's machine for that one request (method, path with the box and
  verb, a date and a nonce; over the tailnet also the sending node and the project it names), opens for 5 minutes
  past its date (plus 5 of clock skew), and once (the tunnel's replay journal; over the tailnet a nonce set in memory,
  keyed by the sender's seal key, at most 4096 per node). Over the tailnet the home checks the caller's token before it
  reads, opens or records anything sealed, and the sender signs the request's purpose and the sealed body with the
  identity key it names (`Zm-Seal-Sig`, ed25519), so a seal key id is taken only from whoever holds that key. A caller
  that names a key but is not a member's token (a tailnet login the route takes) has the key dropped, so the pool's
  sealing mode applies to its clear secrets. Which machine a seal identity belongs to is not recorded yet (binding it
  at join is a TODO).
- **A restart of the pool's machine reopens the replay window** on the tailnet: its nonce set is in memory, so a call
  captured in the 10 minutes before the restart could be taken once more, by someone who can send as that member's
  node with its token.
- A relayed op: its arguments, sealed to the host for that op, project, binding, verb and box, open for 10 minutes;
  the host runs an op id once (its own record, kept 30 days), so a captured op cannot run again or elsewhere.
- How each end learns the other's key: over the tunnel, from a request or answer the same identity signed (the
  member pinned its home's key in the invite; the home knows a member's key by its key-bound token). Over the tailnet,
  each request and answer between daemons names the identity key and the seal key it certified (`Zm-Seal-Id`,
  `Zm-Seal-Key`), and the tailnet vouches for them: a member checks every connection's node against the one it pinned,
  the home takes a key only from a node WhoIs names. So over the tailnet a seal key is only as authentic as the
  tailnet's node keys, which Tailscale's control plane hands out. Every answer of the tailnet listener names the key,
  a `503` while the daemon starts included.
- A member (or host) takes "this pool's machine is older" only from a `2xx` answer to `/v0/health` that names no key.
  A pool's machine reports beside its key the mode it applies (`Zm-Sealing`: `required` when any pool it serves
  requires sealing). A member or host records the key id and the last mode reported (`tunnel/seal_peers.json`, `0600`,
  written whole and synced). If that mode was `required`, it never sends that machine secrets, or takes an op holding
  them, in the clear again until its owner runs `potluck sealing forget -P <pool>` (logged; it forgets that pool's
  machine for every pool it serves). If it was `preferred` (an auto-updater's rollback), it goes clear after such a
  `2xx` health answer, with a Warn every call naming the machine and "it stopped sealing". A key-less answer to a
  request it sent sealed drops the kept key, so a running member notices a rollback at its next call.
- That record fails closed: a file group or others may read is refused (naming the `chmod 600`); a corrupt or empty
  one is moved aside as `seal_peers.json.corrupt` (an Error in the log) and, while it stays, no secret goes in the clear
  to any pool's machine; `potluck sealing forget` then starts a new, empty record.
- Fail closed: a body or op that does not open is refused with one message (sealed for another key, request or op,
  expired, or changed on the way). A host that cannot open an op refuses it. Secrets in the clear from a daemon that
  names a seal key are refused and never used: it can seal, so a clear copy is a bug or a stripped envelope.
- How "this peer can seal" is learned: from the same message that names its key (above). Over the tunnel that is a
  signed header; over the tailnet it is a header on a WireGuard connection to (or from) the pinned node. Someone on
  the path who holds neither peer's keys can change neither, so cannot make a pair that can seal send in the clear.
  What remains: Tailscale's control plane can hand out a false node key, and so could strip the advertisement on a
  tailnet leg; under `preferred` that makes the leg clear, which is what every tailnet leg was before this, and
  `required` refuses it.
- In every mode, secrets in the clear from a daemon that names a key are refused, and a host refuses an op holding
  them in the clear that its pool's machine marked `sealing` (a current pool's machine marks every op, except one it
  sends in the clear to an older host under `preferred`), that came over the tunnel, or that came from a pool's
  machine that named a key to it before.
- A machine the pool's machine heard sealing since it started that names no key now is a downgrade signal, not
  version skew: warned every time secrets would go to or come from it in the clear (under `required`, refused).

### Sealing modes

A pool's mode is kept at its machine (a row of `run_settings` in the pool's `project.db`), read on every call, so a
change takes effect at once, with no restart. `potluck sealing` shows it and the machines its machine heard from since
it started (each one's heartbeat names a seal key or not): that list is the pre-check. `potluck sealing set required`
(an admin of the whole pool, never of one area; `PUT /v0/sealing`) is refused (`409`, naming them) while one cannot
seal, unless `--force`. Every change by `potluck sealing set` is logged and published as the event `dev.zm.sealing.changed` (who, from, to); a new pool's starting mode is not a change and has no event.
`potluck status` shows each pool's mode; the machine list is for the pool's project-wide operators (an area's members
see the mode only).

| | `preferred` (a pool with no row, which is every pool started before the change below) | `required` (what a new pool starts with) |
| --- | --- | --- |
| Between machines that can seal | sealed | sealed |
| Pool's machine to a host that cannot seal, tailnet | as before: in the clear; one Warn per host per start, naming it and "update it" | `403`, the op never runs (runner `jit_config` too) |
| An older member's call to the pool's machine, tailnet | taken as before; one Warn | refused (`403`); its bytes crossed the tailnet in the clear already, and are never used |
| A member's call to an older pool's machine, tailnet | sent as before, in the clear; one Warn on the member's machine (an older pool's machine has no mode to require more) | the same. But a pool's machine that ever named a key to this member is never sent secrets in the clear again (in either mode) until the member's owner forgets it |
| Any leg over the tunnel to or from a machine that cannot seal | refused, as before | refused |

A pool that this build starts (`potluck create`, or any turning of a solo project into a team one) starts `required`:
the row is written before the project turns team, and `potluck create` prints the mode. A machine from before
sealing gets no secrets in such a pool; `potluck sealing set preferred` lets it, while it is updated. A pool that was
already running keeps what it had: no row stays `preferred`, and a mode its admin set stays. Running `potluck create`
again on an existing pool changes nothing, and no migration touches an existing pool. (A running pool that goes solo
and back to team with no row counts as started again and gets `required`; a row it has is kept.)

A machine reports `required` to its members and hosts as soon as any pool it serves is `required`, so starting a new
pool turns that report on for the pool's older siblings on the same machine too: if that machine is later put back on
an older build, their hosts refuse its clear ops instead of warning.

Rollout (a pool that is `preferred`): update every machine of the pool (hosts, members, its machine, in any order: `preferred` keeps the old ones
working), run `potluck sealing` until it lists none that cannot seal, then `potluck sealing set required`. A machine
that has not beaten since the pool's machine started is not listed: check that every host shows.

### Who can read a secret in transit

| Who | `required`, or two machines that can seal | `preferred`, a tailnet leg to or from a machine that cannot seal |
| --- | --- | --- |
| Cloudflare (tunnel) | No: sealed | No: refused over the tunnel in every mode |
| Anything between two tailnet nodes | No: sealed | No: WireGuard, as before |
| Tailscale's control plane | Not by reading traffic. It could hand out a false node key, and so a false seal key | The same; and, holding a node key, it could read that clear leg |
| The pool's machine (its operator, its memory) | Yes. It opens a member's call, checks it and seals it again for the host: hop by hop, not end to end. A `jit_config` is minted there anyway | Yes |
| The host that runs the box, and the box | Yes: the recipient ("trusted" means trusting its root) | Yes |
| The older machine's own process | - | Yes, as before: it is the sender or the recipient |

Sealing from the member's client to the host, so that the pool's machine relays what it cannot read, is not built yet.
Commands, input, output and files (`cp`, `push`) are not secrets: they cross readable by the pool's machine and, over
the tunnel, by Cloudflare. **Whatever a box prints is output**: a command that echoes a secret sends it back unsealed,
readable by the pool's machine and, over the tunnel, by the tunnel's provider.

## Secret grants

`grants/`. A secret grant is a row on the pool's machine (the authority) saying who may put which kind of secret, up
to what ceiling, into which of the pool's boxes. No grant, no secret, except the built-in defaults.

What it governs: secrets landing in a box through the secrets door, that is `exec`'s and `spawn`'s `secrets`, the
box-scope `secrets` verb (`POST .../handles/{id}/secrets`), and a box acquire's `jit_config`. Nothing else is a secret
channel and nothing else is governed: a command's `stdin` and `argv`, files copied in (`sandbox cp`, `push`), a
terminal's `input`, and a daemon's own secrets do not go through the check. `potluck-e2b`'s environment values
are `exec` secrets, so they do.

A grant has:

- `area`: the pool area whose boxes it covers (none: every area);
- `to`, exactly one of: `identity` (one identity in the pool, kept by its id), `role` (`member` or `owner`: anyone
  holding at least that role on the box's area), or `job` (a GitHub job the runner broker serves: `repo`, `workflow`
  (a file directly under `.github/workflows/`), `ref` (a branch's full ref, `refs/heads/<name>`, written exactly),
  `events` (push and schedule by default; workflow_dispatch and repository_dispatch only when listed; never
  `pull_request*`, `workflow_run`, `merge_group`, `release` or the like); matched on GitHub's answers about the run,
  never on what the box or the workflow says);
- `kind`: `value` (a secret the sender holds), `github-jit` (a runner registration the broker holds), or a kind an
  issuer on the pool's machine mints per box (`github-token`, `potluck-token`). An issuer's kind needs its issuer
  there; until then no grant of it can be added. A job grant is for an issuer's kind only;
- `name`: the file name in the box, `[A-Z][A-Z0-9_]{0,63}`, or `*` for any name of a `value` grant (`JIT_CONFIG`
  for `github-jit`);
- `ceiling`: the most one request may ask. For `value`, `{"max_bytes": n}` (1 to 65536, the default 65536): it caps
  each value on its own; a call's names and values together stay capped at 64 KiB separately. For `github-jit`,
  `{}`. For an issuer's kind, what its issuer reads (repositories and permissions, a role);
- `host_class`: `trusted`, the only class a grant may name;
- `scopes`: `exec` (one command: `exec` and `spawn`, the default) and `box` (the box's life: a `jit_config`, and the
  box-scope `secrets` verb);
- `targets`: optionally, only these run targets as listed (`<binding>` or `<binding>@<machine>`).

The check:

- Every `exec` and `spawn` with `secrets`, and every acquire with a `jit_config` (the runner broker's own included,
  as its linked area's member), is checked at the pool's machine before anything is sent to a driver or a host. Each
  name must be covered by a grant whose `to`, kind, scope, area, targets and ceiling all fit; one name uncovered
  refuses the whole call (`403`, naming no secret).
- A public host gets nothing, whatever a grant says: the check asks the host's class as the pool's machine sees it,
  and anything but `trusted` is refused (the refusal is the public host's, as before). The class is asked again when
  a relayed op carrying secrets is handed to its host: a host demoted in between gets nothing and the call is
  refused.
- `role` is judged as admin rights are: a member machine's link, capped at member, by its identity's own grants
  here; a minted token keeps its cap. An `identity` grant also needs the caller's own scope, cap included, to operate
  in the box's area, so a narrowed token or session never inherits its identity's grant. It goes stale, matching
  nothing, once its identity leaves the pool or the grant's area: coming back does not revive it (an admin adds it
  again).
- A row that does not decode lists as `corrupt` and matches nothing.
- The box-scope `secrets` verb: what it sets needs a grant at scope `box` (no default covers it, so a pool without
  one refuses every call); unsetting one's own names needs none. A name the pool's machine mints into boxes itself
  (any name a grant of an issuer's kind names, and `JIT_CONFIG`) is neither set nor unset by a box's holder
  (`403`, the same answer as a name no grant covers), so a minted credential cannot be overwritten or removed from
  inside the box's door. Only grants of the box's own area, or of every area, reserve a name for it. Adding a grant
  of an issuer's kind is refused (`409`) while a live box it would cover holds a value its holder set under that
  name: the value would stay in the box, out of its holder's reach. The authority keeps, per live box, the names its
  holder set (`run_box_secret_names`, names only, never a value); the holder unsets the name first, or the box ends.
  Names set before this record existed (a box live across the upgrade that added it) are not in it.
- An `exec` or `spawn` with secrets of its own runs with its own directory in `POTLUCK_SECRETS` for that one command,
  so a name it sends shadows the box's value of that name there (the box's directory stays as it is).
- The defaults, one switch for both: `default`, anyone who may use the box's area (a `member`) may send `value`
  secrets of any name on `exec` and `spawn` to a box on a trusted host; `default-jit`, a member acquiring a box on a
  trusted host may hand it a `jit_config`. `exec` and `spawn` already require the box's holder and acquire operate
  on the area, so a pool with no grants behaves exactly as before grants existed. The pool's owner turns them off
  once the pool's own grants are in place (a `jit_config` then needs a `github-jit` grant, for the broker's
  identity), and on again.
- The check reads the table itself, so a daemon that wires run targets without the grants module still enforces the
  table and the defaults; it then has no routes to change them.
- Rolling the pool's machine back to a build from before grants silently stops enforcement: that build sends secrets
  as it always did, whatever the table says (the table stays, unread, until a build with grants returns).

Who edits them:

- An admin of a grant's area adds or removes it (a grant for every area: an admin of the whole pool); only an admin of
  the whole pool turns the defaults off or on. Admin is judged as above, so an area's owner manages its grants from
  their own machine. A grant in an area the caller cannot operate in answers as missing. An edit and its event commit
  together.
- `GET /v0/secret_grants` lists the defaults and the rows in the areas the caller may operate in (a guest gets
  `403`); `POST /v0/secret_grants`, `DELETE /v0/secret_grants/{id}`, `POST /v0/secret_grants/default {"on"}`. A
  member's machine forwards them to the pool's machine.
- CLI: `potluck secrets grant add --name NAME (--identity NAME | --role member|owner) [--area] [--target]
  [--max-bytes] [--scope]`, `potluck secrets grant ls`, `potluck secrets grant rm <id>`, `potluck secrets grant
  default on|off`.

The ledger and the grant table live on the pool's machine and stay there. `secret.*` and `secret_grant.*` events
are never copied to a member's replica nor put in a project's export, whatever an older row's stored audience says
(enforced by type), and they neither wake a replica's sync nor count toward the log's pruning. Secret names never
cross a tunnel in the clear (its provider reads answers): over a tunnel (anything that is neither this machine's
socket nor a tailnet node counts as one) `GET /v0/events` delivers these events with a `count` in place of `names`,
`sizes` and `grants` (a grant event without its `name`), `GET /v0/secret_grants` lists rows without their names and
says so in `note`, and an add or remove answers without the name. A body that names a secret crosses sealed on every
leg, or is refused: `POST /v0/secret_grants`, and a box's `secrets` call that only unsets names (so such a call to a
host, or from a member, too old to seal is now refused as too old). An export keeps the grant rows with their names
blanked. The ledger has its own retention on the pool's machine: at most 50,000 rows per kind of ledger event and
none older than 90 days. Names are shown on the pool's machine and over the tailnet.

The ledger is the pool's event log (`GET /v0/events`):

- `dev.zm.secret.granted` when a call's caller-held secrets passed the check and are handed to the transport (if the
  log cannot take it, nothing is sent), `dev.zm.secret.delivered` when that call answered (`failed` when it returned
  an error: they may or may not have reached the box), `dev.zm.secret.refused` when the check said no (with why).
  For an issuer's kind the issuer writes granted, delivered, renewed and revoked itself; the check writes refusals.
- Each carries who asked (address, redacted for a reader who may not see it, and identity id), the kind, scope and
  verb, the names, each value's size, the grant that covered each name, the target, box, host and its class. Never a
  value: the check itself is given names and sizes only. This is a change from recording nothing: a name is policy
  data. They are read by those who may operate in the box's area, never a guest.
- `dev.zm.secret_grant.added`, `.removed` and `.default` record edits of the table, read by the area's admins.

## Kept disks and volumes (the daemon's side)

### Kept disks

From the command line: `potluck sandbox create --persist`, then `potluck sandbox stop <sandbox>` and
`potluck sandbox start <sandbox> [--ttl 1h]`; `potluck sandbox rm` deletes the disk.

- `persist` is for trusted machines only: a public machine's box is refused `403`, since its host can read a kept
  disk. Never with a `jit_config`.
- `stop` is the holder's, or the machine owner's. `start` is the holder's alone: not even the machine owner starts
  another's disk.
- A stopped box is `stopped` in `status` and `list`. The daemon keeps it live: it counts against `handles_max`, it is
  not renewed, and `exec`, `spawn`, `renew` and the rest are `conflict`.
- Its spend does not grow, so the usage row written when it finally ends bills only its time up.
- The daemon asks `trusted` again at `start`: a machine made `public` since keeps the disk but starts nothing on it
  (`403`; release deletes it).
- The daemon never writes secrets to a kept disk: on a persistent box `exec`'s `secrets` go only to a memory
  filesystem (`/dev/shm`), and a guest without one refuses them (a macOS Tart guest today). The daemon sends nothing
  else long-lived into a box, and a runner box is never persistent.
- What is kept is the renter's: anything the renter wrote stays on the lender's disk until the box is released or
  expires.
- A box held by a session can be started only by that session.
- `start` asks the driver's `status` first, so a row that still says `ready` after the lease's end stopped the box
  starts it rather than answering the stale lease.
- For owners: a stopped box is a live handle, so rebinding a project binding (`replace`) or removing it is refused
  (`409`) while stopped boxes keep disks there, up to their 30 days. `potluck sandbox ls` lists them (`stopped`), and
  the machine owner ends each with `potluck sandbox rm`, which deletes its disk.
- A home that outlives a box is a named volume mounted at it.

### Named volumes

- Trusted machines only, as kept disks.
- `volume_owner` is set by the daemon to the holder's identity (never the renter's word; a renter cannot name
  another's), else `actor`.
- `volume_project` is set by the daemon to the project.
- `potluck sandbox create --volume name:path[:rw|ro|cow]` names them.
- The quotas and how `potluck-driver-tart` mounts them are in `docs/hosts.md`.
- A caller never names a volume starting `deps-` or `base-`, and never asks `sealed`: the daemon names
  dependency layers and push bases itself.

### Push bases

A box acquire may ask for `base: {"repo":"<64 lowercase SHA256 hex>"}`. The repository key hashes the sorted
root commit IDs of the commit being pushed. A shallow checkout must omit it. The daemon mounts the holder's
sealed Git object store readonly at `.potluck-base`, and answers `base: {"path":".potluck-base", "hit":true}`.
The push base and dependency layers can be mounted together; their paths cannot overlap, and together with
ordinary volumes they count toward the eight-volume limit.

The daemon names it `base-git-<40 hex>-<ISO year and week>` from the project ID, repository key and current
UTC week. The driver isolates volumes by holder and binding. Different projects never share a name, even for
the same identity. A new week uses a new volume; an older one remains immutable until the existing volume
keep time or capacity policy evicts it. Forks with the same roots share a base within one holder and project.

A box can read the base commit that its holder filled this week on this binding, including files deleted
since that commit. This is more than the pushed bundle carries. A caller that does not want that exposure
omits `base`; it then transfers its checkout without a host base. Only trusted hosts keep bases. A persistent
box, GitHub runner, or binding without `volumes`, `volumes-seal`, `volumes-keep` and `net-off` gets an ordinary
box with `base.hit:false` and `base.why` explaining why.

A capable binding with no sealed base refuses the acquire with `not_found` and `data.reason: "base_missing"`;
no box was made. This requires the driver's optional structured missing-volume detail. An older host or
driver that omits it leaves its original error unchanged; clients must fall back to an acquire without a
base rather than infer what was missing. A host that does not confirm the exact mount is refused and its box
released.

`base: {"repo":"...", "fill":true}` asks for a builder. It retains only the request ID, holder and image or
template from the caller's settings: no ref, commit, dependencies, ordinary volumes, JIT config or kept disk.
Its network is off, its lease is at most 900 seconds (or the shorter binding cap or requested TTL), and neither
the holder nor the daemon renews it. It takes no command or box secrets. The builder answers the base path,
with `hit:false`, and mounts exactly that volume in `build` mode. An unsupported fill is refused; it never
answers an ordinary box. An already sealed base answers `conflict`; another builder answers `unavailable`
with `data: {"reason":"building", "retry_s":...}`.

The holder fills `objects/` and writes `refs/heads/base` last, then releases that builder with `{"seal":true}`. The daemon
supplies `keep: ["objects", "refs"]`, so nothing else the builder wrote reaches readers. Only that builder's
holder can seal it; an ordinary box cannot be sealed, and a caller cannot choose `keep`. A release without
seal, expiry, reclaim or failure discards the unfinished volume. Clients should bound waiting and fall back
to a full push when filling fails.

### Dependency layers

An acquire's `deps` (`[{"kind", "path", "scripts"?, "files": [{"path", "data"}]}]`, `data` base64) asks for a
holder's dependency layer: a sealed volume (`docs/protocol.md`, Sealed volumes) holding what the kind's recipe
built from exactly those files, mounted `cow` at `path`, so nothing a box writes reaches the layer or another box.

- Kinds are one table (`runtarget/deps.go`). `npm`: the files are `package-lock.json` (lockfile version 2 or 3),
  `package.json`, and the `package.json` of every package the lockfile keeps in the project (its workspaces and
  local links), no other and none missing, and the project's `.npmrc` when it has one. An `.npmrc` is read as
  `key=value` lines, comments (`#`, `;`) and blank lines, and takes only keys that change resolution and hold no
  secret: `legacy-peer-deps`, `strict-peer-deps`, `engine-strict`, `install-links`, `save-exact` (`true` or
  `false`), `include` and `omit` (`dev`, `optional`, `peer`; also as `key[]=`) and `install-strategy`
  (`hoisted`, `nested`, `shallow`, `linked`). Any other key (a registry, a credential such as `_auth` or
  `//host/:_authToken`, a script setting) or value is refused by name, its value never shown. The builder gets,
  and the key holds, only those `key=value` lines; one with none is as no `.npmrc`. The recipe runs `potluck-deps link .` when the shelf is mounted
  (`docs/shelf.md`), then `npm ci --ignore-scripts` and `npm rebuild`, which runs install scripts.
  `scripts: "off"` leaves out `npm rebuild`. The root package's own lifecycle scripts never run in a build; a
  workspace package's own install script may, and fails when it needs the project's sources (only `package.json`
  files are there): use `scripts: "off"` then.
- The volume is named `deps-<kind>-<52 hex>`, from the key: the sha256 over the kind, the recipe's text, the image
  digest, the path, the scripts setting and each file's path and bytes (format 4: a layer built before layers took
  the `.npmrc` is never found). The answer's `deps`
  (`[{"kind", "key", "hit", "built"?}]`) carry the whole key.
- Limits: at most 4 layers, 2,000 files and 16 MiB of files in all; layers and volumes together at most 8, their
  paths not overlapping. Trusted hosts only; not with `persist` or `jit_config`; only on a binding that names its
  pinned images (the digest is in the key, so it must be known before the box boots), on a driver listing
  `volumes`, `volumes-seal`, `volumes-keep` and `base64`. A binding another machine serves names the pins its host advertises for
  it (a host from before this advertises none, and is refused); the box and its builder must boot exactly the
  digest the key holds, or they are released and the acquire refused, so an advert gone stale never keys a layer.
- With a `ref`, a box takes one layer, and the driver checks the commit out into that layer's directory (the
  volume's `checkout`, `docs/protocol.md`), so code and `node_modules` sit at the paths the layer was built at.
  The layer's files must then be the commit's root ones (a lockfile in a subdirectory would put the whole
  repository at the layer's path). Such a box always waits for its layer: `deps_wait_s` 0 is refused, since a box
  without the layer would hold the checkout elsewhere. Without a `ref` the caller puts code there itself (a push
  into that directory). On a binding another machine serves, deps take a template, not an image name.
- When the holder has no such layer, the daemon builds it before the box: a fresh box of the holder's on the same
  binding (the same host, where the volume lives), the same image, with the volume in mode `build`, a lease of up
  to 900 s, and nothing else of the caller's: no ref or commit, no volumes, no network setting, no secrets. The
  files are written into the layer directory over exec, the recipe runs there (at most exec's 600 s), and only
  when it exits 0 is the builder released with `seal` and `keep`: the kind's trees (npm: the root's `node_modules`
  and each local package's `<dir>/node_modules`). Once the builder's processes have ended, the driver deletes
  everything else in the layer before it seals it, so nothing the recipe or an install script wrote elsewhere (a
  `.git`, a config file, the files sent) reaches a later box, and clears every setuid and setgid bit and deletes
  every device node, FIFO and socket in what is kept, so an install script run with `sudo` (on a binding with `root`)
  leaves no setuid-root file for later boxes. That cleanup is what keeps a layer safe on every runtime; it does not
  remove file capabilities (`security.capability`). On the OCI layer each box's `ro` and `cow` view of a volume is
  a mount of its own, `nosuid,nodev` on the host, but in the box gVisor (the one runtime that mounts volumes there)
  shows it as a plain 9p mount, `rw` for `cow`, with neither option: Docker's `--mount` cannot carry them. A layer
  that holds the box's checkout is that same mount. A setuid bit or file capability there is inert because of the
  box: a box without `root` runs with `no-new-privileges`, and gVisor ignores setuid bits except under root's
  `runsc-root`, whose boxes have `sudo` already. Each `cow` box writes to an overlay upper of its own, which no other
  box mounts, so nothing it makes there (a device node included) reaches another box. Any other end deletes the volume, so a failed or partial
  build is never there for a reader; the acquire is refused with the recipe's last lines. The builder counts
  against the holder's handles and budget like any box, and is listed, but is the daemon's alone: exec, release,
  acquire progress and cancel, and every other call on it answer as for a missing handle, and a caller's `request_id` may not start
  `deps-build-`.
- Another acquire needing a layer while a box builds it waits (asking again after the driver's `retry_s`) up to
  `deps_wait_s` (default 600, at most 3600), then is refused `unavailable`. With `deps_wait_s` 0 it takes the
  box at once without the layer: `hit` and `built` are both false, and the caller installs for itself.
- After a build the box is asked for again; if the layer was evicted in between, it is built once more.
- Each binding keeps its own layers: a layer built on one host is not found on another, which builds its own.
- A caller has at most 8 acquires with deps in a daemon at once. A joined project's member forwards an acquire
  with deps to its authority with a body of up to 16 MiB (any other call carries at most 1 MiB), the most the
  tunnel and the tailnet take: about 12 MiB of files once base64. Its proxy waits for the answer as long as the
  acquire may take: `deps_wait_s` plus up to two builds of each layer.

#### Asking from the command line

`potluck sandbox create --deps npm[:path] [--deps-wait S] [--deps-scripts off]`, and the same flags on
`potluck run`, send the `deps` field (and `deps_wait_s`) for you.

- `--deps` is repeatable (at most 4). `npm` reads `package-lock.json` and `package.json` in the working directory,
  then the `package.json` of each workspace or local package the lockfile names. The command compiles in the same
  kinds table as the daemon (`runtarget/deps.go`), so a CLI and a daemon of different versions can disagree on which
  files a kind takes: the daemon then refuses the acquire with its own reason. `path` is where the layer mounts in
  the sandbox, relative to its HOME (a `--volume` path); without it, the checkout (`--dir`, default `checkout`)
  plus the working directory's place in its repository, where a push puts that project's files (run from `app/`,
  the layer is at `checkout/app`).
- The files come from the working directory, not from `--ref`. When one is not what the commit holds (its content
  differs, or the commit lacks it, as for an untracked file), the command warns on stderr, since the layer would not
  be the checkout's: commit them, or send them with `--dirty` (an untracked file also needs `--include-untracked`).
- With `--ref` on a machine that checks out refs itself, `sandbox create --deps` is refused: that checkout is not in
  a directory a layer is mounted in. Create without `--ref` and push into the layer's directory.
- The command reads the project through a root it cannot leave: a symlink that points outside the working
  directory, or a file that is not a regular file, is refused, never read. Nothing but the named files is sent.
  Together the layers' files are at most 2,000 files and 16 MiB, as the daemon holds them.
- `--deps-wait S` is `deps_wait_s` (0 to 3600, default 600; 0 takes the sandbox at once without the layer).
  `--deps-scripts off` sends `scripts: "off"`: dependencies' install scripts are not run in the build, and the
  setting is part of the layer's key. Both are refused without `--deps`.
- On stderr, one line says what each layer came to: `potluck: deps npm at checkout: hit (key 3f2a9c1d4e5b)` (already
  built), `built` (this acquire built it), or `not mounted (install for yourself)` (`--deps-wait 0` while another
  sandbox was building it). A layer the daemon's answer does not report is said so, never shown as a hit.
  `sandbox create --json` carries `deps` in the handle as the daemon answered; `potluck run --json` does not.

Trusted hosts only. A public host is refused (`403`, "is a public host: it is given no dependency layer"): a layer
is built from the project's files on that host, outlives the box, and is mounted into the owner's later boxes, so
whoever runs the host could read the files and change `node_modules` the owner's next box runs. It is the reason
`persist` and named volumes are refused there too.

Install scripts: unless `--deps-scripts off`, the build runs dependencies' install scripts (`npm rebuild`) in a box
with network and no secrets, and the result is reused by that owner's later sandboxes until evicted. The root
package's own lifecycle scripts never run in the build (its sources are not there). A workspace package's own install
script may run, and fails when it needs that workspace's sources, since only `package.json` files are sent; the
whole build then fails. Use `--deps-scripts off` for such a project and run the scripts in the sandbox.

### Templates

Acquire's `template` (`potluck sandbox create --template agent`) picks the binding's image of the template's name,
any digest (a binding lent by another machine pins another build).

- Its booted digest is checked against that pin, like any pin.
- A template and an image together, an unknown template, or a binding that lends none of it are refused by acquire
  (400; the last names `potluck host`).
- On a binding served from another machine, the pool's machine holds no images: the image name goes to the host's
  driver, which picks and checks the pin itself, by name, and refuses one its binding lacks.
- Placement with a template takes only a binding that lends it, those known to (on this machine) ranked before ones
  on another machine (whose images it cannot see). None lending it is `unavailable`, naming each and `potluck host`.
- No template: the binding's first image.

## Boxes on other machines

A host's box bindings are machine-level: `$POTLUCK_HOME/run_targets.toml`.

- Owner-only writes, mode `0600`, through `POST /v0/run_targets` with `host` (`potluck host`).
- Kind `box` only, no repo, the same validation as a project binding, plus `format = 2` and
  `share = ["<team project id>", ...]`. A bind may name a project by the slug this machine joined it under (looked
  up first, since a slug can look like an id); it is stored as the project's id, so a project joined later under the
  same slug gets nothing.

### Moving from slug-keyed files

Builds before this one keyed `run_targets.toml`'s shares and `host_handles.json`'s owners by slug. Old files are
told from new ones by their format, never by the shape of a value: a bindings file with no `format` line and a
ledger with no `version` are old. When the daemon starts, once it has loaded its joins, it migrates them: each slug
becomes the id of the project recorded under it (joined, or revoked for now); a slug no project is recorded under,
or that two recorded projects share, is dropped from the shares (with a binding left shared with nothing), and a
ledger entry left with it matches no project. A migrated project keeps the request key tag of its slug, so its boxes
and volumes keep answering; only slugs the old files name are frozen, so a project they no longer named (its shares
removed and its ledger entries expired before the upgrade) starts with a new tag, and volumes left under the old one
are no longer its. An older build refuses files this one wrote (the `format` line, the ledger's `version`).

The mapping is frozen first in `$POTLUCK_HOME/host_keys.json` (`0600`, written once, at the first start of this
build on every host, empty when there was nothing to move; a file that cannot be read, or gives one slug to two
projects, is refused), then
marked done in `registry.db`. A slug found later, as in a restored backup of either old file, resolves only through
`host_keys.json`, never to a project joined since. The mark sits in `registry.db` because that is where the joins
are: a restore that takes the mark away (the whole home, or `registry.db` alone) takes the joins back with it, so a
second migration then means the projects the old files meant; a restore of the files alone leaves the mark.

| `host_keys.json` | mark | files | the daemon |
| --- | --- | --- | --- |
| missing | no | none, or old only | migrates (the first start of this build) |
| missing | no | any new | refuses: restore `host_keys.json`, or, with no copy, remove both files and share again |
| missing | yes | none | starts again with an empty `host_keys.json` (volumes left under an old tag are no longer any project's) |
| missing | yes | any | refuses: restore `host_keys.json`, or, with no copy, remove both files and share again |
| there | no | any | sets the mark again, then as below |
| there | yes | any | old files resolve only through `host_keys.json` |

While it refuses, or until the migration has finished, binds and relayed calls answer `503` and nothing is changed;
boxes already made end at their `expires_at`, since no renew reaches them (drivers end a box at its deadline on
their own; conformance: "a short ttl ends the resource with no release and no call to the driver").

One limit: a host that left a project and joined another under its slug before the migration finished (before
upgrading, or while the daemon could not write the files and was restarted) gives the second what the first left;
the old files do not say which project a slug meant.

### Adverts

- The host replica's heartbeat for project P carries `run_targets`: each binding shared with P and its `describe`
  (effective class, size, `available`; refreshed by a background ask at most every 20 s).
- The pool's machine keeps the latest set per machine in memory, dropping a machine's after 60 s without a beat.
- It keeps them only from an enrolled host: a `trusted` machine (a member's by default) or one whose class the owner
  set (`public`: a lender the owner named). A guest's or area member's machine by default lends nothing, and only
  status, release and list are relayed to one. A host that is no longer enrolled gets no box, command or renewal; its
  boxes can still end.
- Placement (`potluck sandbox create`) never picks a box on a `public` machine unless it is named (`--on`).
- `GET /v0/run_targets` lists them as `<binding>@<machine>` (path-safe; handles `box:<binding>@<machine>/<id>`), and
  every view carries `host` (the serving machine; this one for a local binding) and `host_class` (`trusted` or
  `public`, from the pool's machine; local is `trusted`).
- `describe` answers from the advert. The effective class is the stricter of the advert and `describe`.

### Relayed calls

- acquire, progress, cancel, status, release, renew, exec, spawn, output, signal and input on such a name behave as for a local box
  (rows, areas, holder checks, the sweep, the end hook, all at the pool's machine), except that the driver call is a
  machine op `run_target` (`{binding, verb, id, args}`): recorded before it runs, run at most once; one not taken
  within a minute never runs.
- A host that lends boxes to the project holds `GET /v0/team/ops` open at the pool's machine (a long poll, at most 25
  s per request, outbound only, so over the tailnet and the tunnel alike), answered as soon as an op is queued for
  it, and posts each result at once (`POST /v0/team/ops/report`). Heartbeat answers and beats carry both too, the
  fallback while the poll is down.
- The host's run-target module checks the binding exists and is shared with that project (else `not_found`) and
  calls the driver exactly as a local call (environment allowlist, limits, error mapping). `list` is answered only for
  the `request_id` the pool's machine names.
- The pool's machine waits the verb's limit (exec: `timeout_s` + 30 s; output: `wait_s` + 10 s, at most 30 s; input:
  15 s) plus 20 s. No answer is a driver timeout (acquire retries with the same key, then reconciles; never acquires
  afresh). A host not heard from in 60 s (no beat) is `unavailable` at once, not waited on.
- Arguments (an exec's argv, stdin, secrets) and answers are held in memory for the one call, never written to the
  database. A relayed call is not end to end: the pool machine's operator can see an exec's argv, stdin and output
  (and its secrets as they pass through; on the wire to the host they are sealed). The host blanks its report once an
  answer took it.
- The host keeps each answer within 3 MiB as encoded: an exec's output is cut to fit and `truncated` (a base64 one
  in whole quanta); a relayed `output` page is cut to 256 KiB at the host, and its `*_next` says where to go on;
  any other answer over it is an error. A heartbeat carries at most 4 MiB of reports; the rest go with the next
  beats.
- The host's node-bound token is the only credential; exec is proxied through the pool's machine, never direct.
- A relayed call adds tens of milliseconds on a warm or a cold host (measured on one machine: about 15 ms over the
  driver's own exec); through the heartbeat fallback it waits for a beat.

### The host's ledger

- The host keeps a handle ledger (`$POTLUCK_HOME/host_handles.json`, `0600`). Each handle its drivers issued through
  a relay is recorded, on the acquire answer, against the id of the project whose pool machine acquired it (kept
  until untouched for 30 days, far past the 24 h an ended handle is kept). A ledger from a build that recorded slugs
  is migrated with the bindings file, through `host_keys.json`; an entry whose slug it does not hold is recorded as
  `slug:<slug>` and matches no project.
- status, release, renew, exec, spawn, output, signal, input and list from another project's pool machine answer
  `not_found`, as for a missing handle.
- The host sends its driver `<project tag>-<key>` (the pool machine's key behind a tag of the project's id; for a
  project migrated from slug-keyed files, of that slug as `host_keys.json` holds it, so its boxes and volumes keep
  answering), so two
  projects sharing a binding never meet on one key. An acquire answered with a handle the ledger has for another
  project is `409`, and that box is left alone.
- A ledger that cannot be read fails closed (`503`, never rewritten).

### From a member's machine

- A member's own machine reaches all of this through its socket: the team proxy forwards `/v0/run_targets` and below
  for a joined project (by `-P` or the working directory) to its pool machine, waiting the verb's limit plus the
  relay's slack (exec: `timeout_s` + 60 s; acquire: two tries and a reconcile, 340 s), not its 30 s call timeout.
- Bind and unbind are refused there (`400`, "bound on the machine that serves them"), and `?host=1` and a bind with
  `host` stay local.

Not yet: image choice on a remote box (a `ref` there is the renter's push), placement across hosts, runs across
machines, the pool machine's own pin of a remote box's image (the host reports its digest and is believed), a relayed
call past Cloudflare's 100 s limit over the tunnel (the answer is one signed body: a long command there is a
background process, whose calls each answer within 50 s).

## Pools

A pool is a team project whose members lend boxes and run sandboxes on each other's. A host is a guest of the `hosts`
area, never a project member (a project relation covers every area); a renter is a member of a team's area. The pool
commands wrap the steps above and keep no state of their own.

- `potluck create <pool> [--via tunnel]` creates the project if missing, with the areas `team` and `hosts`, makes
  this machine the one that runs it, and brings the tailnet node up unless `--via tunnel` (printing the login URL
  once, waiting up to 10 minutes). Running it again on an existing pool is safe.
- `potluck invite NAME` prints a one-time invite for a renter (a member of the team area): first a link to
  potluck.sh's join page carrying the code in its fragment (`https://potluck.sh/join#v1.<base64url of the code>`),
  which shows the one line that installs Potluck and joins, then `potluck join '<code>'` for a friend who has Potluck.
  `--json` adds `link`. Without `--via` the code is the tunnel's (the friend needs no Tailscale) while this machine's
  tunnel URL answers from outside, signed by this home's key; otherwise the tailnet's, with a line saying why the tunnel
  was not used. `--via tunnel` or `--via tailnet` asks for one route and is refused while it does not work. `potluck invite NAME --host [--trust public]` prints one for a host (a guest of `hosts`; they run
  `potluck host '<code>'`).
- Each host invite carries its machine's class: `trusted` by default, `public` with `--trust public`. The machine has
  it the moment it joins.
- A friend who comes later gets an invite from `potluck invite` run again.
- The class rules then apply as for any host: a class the owner chose enrolls it (a `trusted` host's boxes are placed
  and take secrets; a `public` host's only when named, never with secrets), and an area guest's machine with a chosen
  class is listed (it sends no machine numbers).
- `potluck join '<link or code>'` joins; a link is read as the code inside it (`kit/project/joinlink.go`).
- `potluck invite ls` lists the invites this machine made (live, redeemed, expired; a redeemed one's machine, and
  whether it left); `potluck invite rm <id>` removes one (a machine that redeemed it keeps its token).
- `potluck host` is described in `docs/hosts.md`.

The same commands work on any team project, not only a pool (`cli/team.go`): `potluck invite <project>[/<area>]
--as <name> [--role member|guest] [--trust trusted|public] [--allow-tagged]` admits a machine there as that
identity; `potluck join <project>` joins one with no code where the program that runs it grants GitHub accounts and
yours has a grant (Potluck alone makes none; the machine that runs it asks GitHub to admit you; `<project>/<area> --from https://<host>#<fingerprint>` joins one area over https), and
`--path` records your checkouts of it. A pool's invite (no `--as`) into a team project that is not a pool is
refused, since it would add a pool's area there. `potluck remotes` shows a remote's local copy when its program
keeps one.

### Inviters

Only an owner (or an admin of the whole project) makes invites, unless an owner makes a member an inviter. An
inviter makes guest invites, nothing else. The rule is in `kit/project/inviter.go`; every program built on Potluck
gets it from there.

- An owner of the whole project grants a member's grant the role `inviter` (`POST /v0/grants` with role `inviter`;
  in zm, `zm grant <name> inviter <project>[/<area>]`). The grant stays `member`, with `inviter` set in the grants
  list. Only someone who is already a member there can be made an inviter. Nobody is an inviter by default, and an
  area's owner cannot make anyone an inviter.
- To take it back, an owner grants the plain role again (`member`, or any other role), or revokes the grant.
- An inviter's invite must have role `guest`. It goes into the object of the inviter's own grant: an area member's
  invite goes into that area, and a project-wide inviter's into the project or any of its areas. So the guest never
  sees more than the inviter does. It carries no machine class and no `--allow-tagged`, since those are an
  owner's to give. It names someone new: never an existing identity, never a name another live invite holds, and
  never a name the daemon keeps for its own identities (`zm`, `potluck`, or one starting `zm-` or `potluck-`).
  An inviter changes no grant and no role.
- A project-wide inviter may invite a guest into any area, a pool's `hosts` included. With no class, such a guest's
  machine is not a host (see Machine class).
- A member or owner invite from an inviter is refused with `an inviter makes guest invites only`. A plain member is
  refused as before.
- An inviter's code joins only while its inviter still holds the permission and its name is still free. When the
  permission goes (another role, a revoke, a lowering, a raise to owner), that inviter's unredeemed codes are
  deleted. Giving the permission back does not restore them.
- The identity an inviter's guest invite creates stays a guest. An inviter picks the name, and it may be one an owner
  or the daemon meant for someone else (a future member, the runner broker's `zm-runners`). So no invite, join or
  grant by the daemon makes it more than a guest; those are refused. Only an owner's own grant can raise it. After
  that, it is an identity like any other.
- Minting a member token for an inviter keeps them an inviter.
- An inviter's live invites count toward the project's cap on live invites, like an owner's. An owner sees them in
  the invite list, made by the inviter, and removes them there.
- In a pool, `potluck invite NAME` makes a member invite, and `--host` gives the machine a class. Both are refused
  for an inviter. A pool inviter's guest invites go into the area they joined, with `potluck invite <pool>/team --as NAME
  --role guest`.

## Clients

A program built on Potluck carries these commands in its own command tree, in its own words:
[mounting.md](mounting.md).

### Sandbox commands

`potluck sandbox create|ls|exec|extend|rm|push|cp|shell|logs|signal`, `potluck usage` and `potluck run`.

- `potluck sandbox ls` lists your own sandboxes in the pool (made from this machine's human, machine or any of its
  sessions), newest first: each with who made it (`by=`), and an ended one's end time (`ended=`) in place of
  `expires=`. `--all` lists every sandbox for the pool's machine owner or an owner or admin of the whole pool; a friend in a pool sees only their own either way (the server keeps the rest out).
- `potluck sandbox logs -f` pages until the process exits and exits with its code, or 1 for a process that ended with
  no exit.
- `create` places the box. `--on` takes a binding as named: its full name (`c2m4-box@example-host`, as the target list prints it), its own name (`c2m4-box`), a machine that shares one box, or a machine with `--size`; more than one match is refused with the names, and a miss names the closest. Otherwise every box binding describes itself, and the
  smallest available one matching `--size` and `--os` wins: fewest CPUs, then least memory, then name. With `--net
  off`, only a binding listing `net-off`.
- A refusal names each binding and why, and, when `--size` names a size no binding placement may pick states, the
  sizes they do (Potluck's own: `c1m2`, `c2m4`, `c2m8`, `c4m16`, `c8m32`).
- Both use the run-target routes; there are no sandbox routes.

### Terminals

`potluck sandbox shell <sandbox> [-- argv]` (default a login shell: bash, else sh):

- It puts the local terminal in raw mode and sends its size and each resize.
- It keeps one `input` call in flight: keys typed meanwhile go in the next, at most 32 KiB a call, base64 so any byte
  crosses. The part a terminal did not take goes again. A lost call loses its keys, never repeats them. A refusal that
  would recur on every key, such as an older host that relays no `input`, ends the session with its message.
- It keeps one `output` call waiting, exits with the command's code, kills it when Potluck itself is signalled, and
  ends it on Enter `~` `.`.
- Piped input works too: an end of input sends Ctrl-D.
- A key's echo measured on one machine: about 30 ms (p50) through the local daemon and relayed to a host over the
  tailnet or the tunnel stand-in alike; a real network adds about two round trips.
- Keys cross a relay as `exec`'s stdin does: in the clear to the pool machine's operator, and over a Cloudflare Tunnel
  where Cloudflare can read them (only `secrets` are sealed). So a password typed into a box's terminal crosses as
  typed.

### Code from the renter

`boxpush/`. A box whose binding has no `repo` (every box served from another machine) cannot fetch a ref, and needs
no credential to get one: the renter's client sends it.

- It bundles one commit of its own checkout (default `HEAD`): a git v2 bundle (v3 for SHA-256 repositories), holding
  that commit and its tree only, the box marking it shallow as `clone --depth 1` would.
- `--full` sends the whole history. `--dirty` adds the working tree's changes to tracked files as a second commit on
  top, and `--include-untracked` with it the files git neither tracks nor ignores, each named on stderr (an untracked
  `.env` never goes unasked). It is written through a temporary index, so the renter's refs, index and files are
  untouched.
- It refuses a bundle over the limit (default 128 MiB) before anything is sent.
- It ships it through `exec` alone, no new verb: one short `sh` script per call with literal arguments (no value
  spliced into a script), stdin base64 (capability `base64`).
- A probe checks the box has `git` and a SHA-256 tool (`sha256sum`, `shasum` or `openssl`) and lists the chunks
  already there.
- Each 512 KiB chunk (its request about 700 KB of JSON, inside every door's and the team proxy's 1 MiB body, each call
  answering in well under a second, so inside a relayed call's limits and the tunnel's 100 s) goes to
  `$HOME/.potluck/push/<key>/c.<i>`. It is kept only when its SHA-256 matches the one sent beside it (written to a
  temporary name, then renamed). Up to four are in flight.
- A call that fails without a refusal, or a chunk that arrived damaged, is sent again (at most four tries). A push
  that still fails ends with the chunks kept, so the same push again sends only what is missing.
- The last call joins the chunks, checks the whole bundle's SHA-256 (a mismatch drops them and checks nothing out),
  fetches the bundle into the push's own git directory (`$HOME/.potluck/push-git/<key of the checkout path>`, or the one a box already has under `$HOME/.zm/push-git`; the ref
  `refs/zm/push`, no remote), checks the commit out detached in `~/checkout` (or `--dir`), verifies `HEAD` and
  removes the chunks, in 90 s. Sent again after its answer was lost, it finds the chunks gone and `HEAD` at the
  commit, and answers the same.
- The checkout's `.git` is always a file naming that directory, written by the push. A `.git` that was there before
  (a dependency layer's, the image's, an older push's) is moved aside unread (`<git directory>.replaced.<n>`), so
  its config, hooks, alternates or replace refs never change what is checked out. The first push into a directory
  then removes every untracked file and nested repository there except ignored ones and `node_modules`, so the
  checkout holds the commit's tree (in a box pushed into by a potluck from before this, untracked files in the
  checkout go on the next push). A later push to the same directory (by any spelling of it) fetches on top of the
  push's own history and leaves untracked files as they are. A checkout that is `/`, HOME or a directory holding it,
  or under `HOME/.potluck` or `HOME/.zm`, is refused.
- Nothing but the bundle's bytes enters the box.
- Measured on the reference driver (one machine): zm's own `HEAD` is a 7.0 MiB bundle in 15 chunks, made in 0.3 s and
  pushed in 0.5 s; 3 MiB in 7 chunks through a relay across two daemons, 0.4 s.
- `potluck sandbox create --ref <ref>` on such a box acquires it, then pushes (printing the handle first). A failed
  push keeps the box and names `potluck sandbox push <sandbox>`, which pushes into a box held already, to resume.
- On a binding with a `repo`, `--ref` stays the pushed ref the daemon resolves, as above.

### Files in and out

`boxcp/`. `potluck sandbox cp <sandbox> <local path> :<path>` and `potluck sandbox cp <sandbox> :<path> <local
path>` copy a file or a directory with everything beneath it, through `exec` alone, as a push does: no new verb or
route, so across machines and the tunnel within the same caps, and the holder's only, by exec's checks.

- A sandbox path is under the box's `HOME` unless absolute (`:` alone is `HOME`). The sandbox's side is the argument
  that starts with `:` (a local path that starts with one is written `./:name`).
- As `cp`: an existing directory takes the copy by its name, a path ending in `/` is made a directory first, else the
  path is the copy's own name.
- The limit is one copy's bytes (a file, or a directory as a tar): default 128 MiB, `--max-mib` up to 1024. Past it
  the copy is refused with `too_large` before a byte is sent up, or by the box before a byte comes down.

Up:

- A copy is not a secret channel: it lands on the box's disk. A copied path whose name looks like a credentials
  file (`.env`, `.netrc`, an SSH key, a `.pem`, `.aws/credentials`, `.docker/config.json` and the like:
  `boxcp.CredentialLike`) is still copied, and named: `credential_like` in the result (also `sandbox_write_file`'s),
  and a warning on stderr from `sandbox cp` pointing at `exec --secret`.
- A file goes as its bytes, a directory as a tar the client writes (directories, regular files, symlinks as links
  never followed; devices, sockets and FIFOs left out and listed; no owner names or ids). A directory holding this
  machine's temporary directory, where the tar is written, is refused.
- A first exec resolves the destination once, as `cp` has it, to one absolute path, refusing a directory onto a file
  or a file onto a directory before a byte is sent. Every later exec writes exactly that path, so one sent again
  after a lost answer does the same thing, never a second copy inside the first.
- The bytes go in 512 KiB chunks by the push's own chunk sender (`boxpush.Send`: probe, checked chunks under
  `$HOME/.potluck/push/<key>`, four in flight, retries, resume). The key names the bytes and that destination, so two
  copies of the same bytes to two places never share or clean up each other's chunks.
- One more exec (90 s) joins them, checks the whole SHA-256 (a mismatch writes nothing), writes the file (its mode
  kept) or untars it with the box's `tar -x -o` (merging into a directory there), and answers the path as the box's
  shell has it.
- The chunks go after it answers, written or refused. With no answer (or a chunk missing) they stay, and the same copy
  again resumes.

Down:

- One exec (90 s) snapshots the path in the box's `TMPDIR` (a file's bytes, or the box's `tar -cf - .` of the
  directory, cut at the limit + 1 to see it passed) and answers kind, size, SHA-256 and a file's executable bit. A
  directory holding that `TMPDIR` (`/`, say) is refused, never a tar of its own snapshot.
- 512 KiB pages come back by `dd` through exec's base64 output (each checked for its length, `truncated` refused),
  four in flight, into a temporary file beside the destination. The snapshot is removed whatever happened (else it
  ends with the box).
- The client checks the SHA-256 against the box's, then writes the file (mode 0644, or 0755 when executable there;
  replacing a file at the destination), or unpacks the tar itself with Go's `archive/tar` into a fresh directory
  beside the destination under `os.Root`, renamed into place at the end. The box's bytes and names are data, never
  run, never given to a local `tar`.
- An entry whose name is absolute or holds a `..` component refuses the whole copy (`forbidden`, nothing written), and
  so does a sparse entry (an old GNU `S` entry or PAX `GNU.sparse.*` records, which a tar reader expands to any size),
  and writing past the copy's limit in all (a running total).
- Only directories and regular files are written (modes at most 0755, no set-id bits, no owners). Symlinks, hard
  links, devices and FIFOs are left out and listed (names quoted, at most 50). A directory already at the destination
  is `conflict`, never merged.

Measured on the reference driver through a relay across two daemons: a 2 MiB file and a 2 MiB directory up and the
whole tree down in 0.7 s.

### Jobs

`potluck run [--on|--size|--os] [--ref] [--dirty] [--timeout 30m] [--keep] [--net off] [-- argv]`:

- It places and acquires a box as `potluck sandbox create` does. The lease is the timeout plus 10 minutes, cut to the
  binding's lifetime cap, and refused when the timeout does not fit.
- It pushes the commit, `spawn`s the command in the checkout with `timeout_s` the timeout, follows `output` (20 s
  waits, 256 KiB pages) to the terminal as it comes, and exits with its exit code.
- The box is released at the end unless `--keep`: also on Ctrl-C or a closed terminal (SIGHUP, unless Potluck was
  started with it ignored; the process is sent TERM first; exit 130; a Ctrl-C during the acquire asks again with the
  same `request_id`, which answers the box the daemon went on making, and releases it), an error, or the timeout
  (exit 124; KILL is sent 90 s past it).
- A process that ends with no exit code, or whose output stays unreachable for 2 minutes, is `lost` and fails (exit
  1), never a pass.
- No argv: the commit's `.potluck/job`, run directly when committed executable, else with `sh`.
  The job lives with the code, so a commit names one job. No argv and no job file is refused before a box is made.
- `--json` prints `{"handle", "target", "commit", "pid", "state": "exited"|"timeout"|"lost"|"interrupted"|"failed",
  "exit"?, "error"?, "error_code"?, "kept", "released", "seconds", "setup_s"?, "phases"}` on stdout alone
  (the command's output then goes to stderr). `error_code` is the typed API error code, or `no_target` when
  placement found no fitting shared binding; it is absent for errors without a structured code. A command's
  nonzero exit remains in `exit`, and a missing box remains `state: "lost"`.
- `setup_s` is wall time from command entry (local preparation included) until spawn answers; absent if no
  process started. `phases` contains seconds for `prepare`, `acquire`, `push`, `start`, `run`, `collect`, and
  `cleanup`. They do not overlap and sum to approximately `seconds`; zero means that phase did no work.
  Acquire includes placement, base filling, dependency waiting/building and box startup. Optional
  `acquire_details.base_fill` measures the base-fill attempt, including waiting and cleanup.
  `acquire_details.deps_acquire`, when dependencies were requested, is acquire minus base fill: it includes
  placement and box startup, not only dependency work. These details overlap acquire; do not add them to
  the phase sum. The daemon does not expose separate dependency-build and wait timings.
  One stderr line reports setup, phases, and total time.
- Agents use the sandbox tools in the same order. There is no job tool, since a job outlives a tool call.

### Agent tasks

`potluck agent-task [claude] --prompt-file <file|->` runs a coding agent CLI once in a fresh box on the renter's
pushed commit, with the renter's credential as the agent's spawn secret, and fetches the commits it made into
`refs/potluck/agent-task/<sandbox>` in the renter's checkout. It is `potluck run`'s box job with one step before the
release (the box's bundle copied back), with no routes or state of its own. See `docs/agent-tasks.md`.

### Agent runners

`potluck agent-runner <adapter> --secret-file <file>` keeps a coding agent vendor's self-hosted runners going, one
fresh box per runner, with the renter's vendor key as each runner's spawn secret (the secrets door above). It is a
client over the box verbs, like `potluck run`, with no routes or state of its own. See `docs/agent-runners.md`.

### Usage

When a box ends (the daemon's final status recorded), the usage module (`usage/`) writes one immutable row per
handle (`GET /v0/usage`, rows in the areas the caller reads; a member's machine forwards the call to the pool's
machine; `potluck usage`):

- area, actor, target, size, image digest, end state;
- lease seconds from `acquire`'s answer to the recorded end (never past `expires_at`);
- the driver's spend;
- `billable_minutes` = ceil(min(lease minutes, the driver's minutes where its unit is time));
- `billed`: false for `reclaimed`, `lost`, or a box that never answered `acquire`.

A repeated end report writes nothing. A failed write is retried, and a row still missing comes back when the module
reconciles from run targets' ended handles (the whole record at start, then every 10 minutes from a day before its
newest row). Area guests read their area's rows. Nothing a box was sent is recorded. Rates come later; the team pool
is free.

An area can have a sandbox minute limit per UTC day, ISO week or month: `[[box_minutes]]` in project.toml (`area`,
`minutes`, `period`), set by an owner or admin of the whole project with `potluck usage limit <area> <minutes> --per
<period>` (`PUT /v0/usage/limits/{area}`; 0 removes it). `potluck usage limit` (`GET /v0/usage/limits`) lists the
limits on areas the caller reads. Before a new box in that area, or the start of a stopped one, run targets ask the
usage module, which adds the area's billed minutes for boxes that ended this period, boxes of the area that ended
this period whose row is not written yet (read from run targets' record, however long ago they ended), and each live
box's minutes so far (its lease, by the same rule that bills an ended box; for a box stopped or started again, the
driver's time figure for what it ran before). More than 50,000 of the area's boxes ended in one period read as
unreadable, so refuse. At the limit the box is refused, with the command that raises it; a
running box is not stopped. Minutes that cannot be read refuse too. A box counts in the period it ends in, so one that
runs across midnight counts wholly in the new day. The limit is a fair-share ceiling: two boxes asked for at one
moment can both pass. `potluck usage limit <area> <minutes>` without `--per` keeps the area's period (a month for a
new limit). Renewing a running box's lease is allowed at the limit: it adds no box, and what it runs is counted as it
runs. A limit on an area the project no longer defines is dropped at the next change of limits or reconcile pass.

### E2B gateway

`cmd/potluck-e2b` (`internal/e2bgateway`; old name `cmd/zm-e2b`): a socket client like `potluck sandbox` (no routes of
its own) that lets the unmodified E2B SDKs run on boxes.

- Control plane: create (placement as `potluck sandbox create`), get, list, kill, timeout and connect (a renew within
  `ttl_max_s`). A sandbox id is a hash of the handle.
- Data plane: it answers envd's `process.Process/Start` (Connect, JSON) and `GET`/`POST /files` itself with `exec`
  (no envd in the box), routed by `E2b-Sandbox-Id` and authenticated by `X-Access-Token` (an HMAC per key and
  sandbox).
- Output past exec's 1 MiB is `out_of_range`, and a command past its deadline or 600 s `deadline_exceeded`, never a
  cut copy. Network rules, pause, volumes, PTY and stdin streaming are refused, not dropped.
- A sandbox's `envVars` and a command's `envs` go to the box as that command's `exec` secrets (the command's own
  win), never in argv: a fixed `sh` wrapper in the guest reads each file under `POTLUCK_SECRETS` into the
  command's environment. So the secrets rules apply: a trusted host whose driver lists `secrets`, under a grant;
  a public host answers `permission_denied`, and so does a host whose swap is not safe for secrets (`docs/hosts.md`); a guest with no memory filesystem, or with swap on disk, fails such a
  command (exit 125). Like any exec with secrets, a command with envs gets its own secrets directory in
  `POTLUCK_SECRETS`, not the box's (`secrets` verb). A name is a secret's name, `[A-Z][A-Z0-9_]{0,63}` (an E2B lower-case
  name is refused), and all of one command's envs are at most 64 KiB.
- API keys come from a `0600` keys file, each acting as the gateway's own identity or a token.
- Clients set `E2B_DOMAIN=<domain>:<port>`. `*.localhost` is loopback (RFC 6761), and the gateway signs a certificate
  per name from its own CA, name-constrained to the domain. One port serves TLS and plain HTTP (plain only on
  loopback: a non-loopback `--addr` needs `--tls-only`).
- A scanned box belongs to the key whose tag its request id carries.

## GitHub runner broker

`runners/`. On a pool machine whose `config.json` has `runners`, the daemon serves GitHub Actions jobs from a pool
project's box bindings through one GitHub App. Nothing about a host or a team is built in: the App, the pool and the
sizes are config.

### Config (`runners`)

- `github`: `app_id`, `client_id`, `slug`, `private_key`, `client_secret` and `webhook_secret` (secret references,
  never values), `api_url` and `web_url` (default github.com).
- `pool`: `project` (a slug); `sizes` (optional: a size name to the one box binding that serves it, the owner's
  override of placement below); `default_size` (the size a plain `label` job asks; unset: any, the smallest box that
  fits).
- `label` (default: the pool project's slug, so a pool `friends` serves `runs-on: friends` or `friends:<size>`, with
  or without `self-hosted`; an explicit label works the same).
- `public_url`: an https URL that forwards only the broker's public paths (`/v0/runners/hook`,
  `/v0/runners/oauth`, `/v0/runners/app/{new,created,installed}` and `/v0/runners/capacity/<host>`) to the daemon unsigned, for example Tailscale
  Funnel on just those paths or a reverse proxy, never the rest of the API. The tunnel's door takes only
  member-signed requests, which GitHub and a browser cannot make.
- `listen`: a loopback `host:port` the broker opens serving those paths and nothing else; HEAD is refused where a
  state is spent. With `listen` set, `public_url` can be `tunnel.url`:
  `potluck runners setup` points cloudflared's ingress for exactly those paths at `listen` (see Setup).
- `allow_fork_prs` (default false): serves a private repository's runs from forks and of every event. A public or
  internal repository's never are. Which repositories are served is not config: see Connected repositories. It is set by
  editing `config.json`; the broker serves the edit from its next start or its next settings save (`potluck runners
  set`, which reads `config.json` as it is on disk). Switching it off then retires the runners that served forks (see
  Serving one).
- `accounts` (default none): the GitHub users and organizations whose installations of the App the broker hears,
  compared without case. GitHub offers an App to other accounts only once it is public, and then anyone can install
  it and GitHub sends that installation's deliveries here signed with the App's own `webhook_secret`. A delivery from
  an account not listed is answered 202 and dropped before it is stored (no inbox row, no job row, so no doctor
  probe or owner notice), logged at most once an hour per reason; linking such an installation is refused. A linked
  installation's own `installation` events are still heard (they can only unlink it). With none listed, only the
  installations linked here are heard. Read from `config.json` at start and at each settings save.
- `job_ttl_s` (default 3600; never under three sweeps, 900 s): a job's time limit. Its box is leased this long and the
  lease is renewed by that much while the job runs (the sweep), up to the lending machine's own `ttl_max_s` and
  never past it; a box whose limit is below `job_ttl_s` is not given the job. A job keeps the limit it was served
  with (migration `runners/0006_ttl`, two columns, one-way): a later `job_ttl_s` is the next job's. A job whose box
  expired under it says in its row that it was stopped at its time limit.
- `potluck runners set <setting> <value>` (`POST /v0/runners/settings {"key", "value"}`, local, the pool project's
  owner) changes `label` (a name, or `default` for the pool's slug) or `job_ttl_s`
  (seconds, 900 to 432000): written into `config.json` as it is on disk and served from the next job on, with no
  restart. After a `label` change a recorded job that asks for the old label is not served (its row says so) and is
  never placed again without its size. `potluck runners ls` prints the settings.
- `allow_public_repos` (the old switch that served every public repository of a linked installation) does nothing
  any more: a public repository is served only after its owner says yes for it (see Connected repositories). A
  `config.json` that still sets it to `true` is told so in plain words, naming `potluck runners allow <owner/repo>
  --public`: once in the daemon log at start, in the `notices` of `GET /v0/runners/setup` (printed by `potluck
  runners ls` as `notice`), and in the `warnings` of the CI doctor. Public repositories that were served under it
  are not served until their owner says yes. The key stays in `config.json` until it is deleted there.
- The commands the broker's hints, warnings and job reasons tell a person to run are spelled from one setting on
  the broker, `Service.Command` (default `potluck`): a program that embeds the broker under another name sets it,
  and none of those texts (the JSON of the routes below included) names `potluck`.
- Without `runners.github.app_id` every route below but `app` answers `unavailable`.

### Links

An installation of the App serves a pool area only once linked.

- `POST /v0/runners/links {"installation_id", "area"?}` (admin of that area in the pool project) answers a GitHub
  authorize URL.
- The person signs in there, and GitHub returns to `GET /v0/runners/oauth` (public, a single-use `state` bound to that
  request, 10 minutes). The pool machine exchanges the code for a user token and goes on only if the person
  administers the installation's account (for an org: an active admin, `GET /user/memberships/orgs/{org}`, which the
  user token may call only if the App has `members` read; for a user account, that account itself), then drops the
  token. GitHub's 404 there (not a member) is a no; its 403 "Resource not accessible by integration" is an error page
  saying the App lacks Members: read and how to add it, never "not an admin".
- The page then names the area and who asked, and the link is made only on that person's yes (`POST
  /v0/runners/oauth`, a second single-use state). So a link URL handed to an org's admin cannot bind their org to an
  area they did not choose.
- An installation linked to another area moves only for someone who also administers that area.
- The link mints identity `zm-runners` a `member` token capped to the pool project and that area (only its id is
  kept, never its secret). Boxes are acquired as that token's subject through the run-target routes' own checks
  (`core.Deps.RunTargets`), so they are that area's, and its budget and ledger apply.
- `GET /v0/runners/links` lists links in the caller's areas. `DELETE /v0/runners/links/{installation}` (admin)
  unlinks and revokes the token. An installation deleted on GitHub unlinks too.
- CLI: `potluck runners link [<installation-id>] [--area]`, `potluck runners unlink <installation-id>`,
  `potluck runners ls`.

### Connected repositories

A linked installation's repository is served only once the pool's owner connects it (`runners_repos`, migration
`runners/0008_repos`). No config is edited for it. A connection is of one repository: its name (any case) and
GitHub's repository id. A repository under a connected name with another id (the connected one was renamed or
deleted, and another took its name) is not connected, and neither is a connected one under a new name, until the
owner connects it (which drops the record under its old name).

- `POST /v0/runners/repos {"repo", "public"?}` (local, the pool project's owner; `potluck runners allow <owner/repo>
  [--public]`) finds the App's installation on the repository (`GET /repos/{o}/{r}/installation`), which must be
  linked to the pool, and reads the repository. A public one is refused without `public`: the owner's yes to serve
  its own branches (pushes, manual runs, schedules, pull requests from its own branches). A private one is refused
  with `public`: the yes is given while the repository is public, never ahead of it. Once given it stays on the
  record, also when the repository is made private and public again (its jobs are served again without a new yes);
  `allow` while the repository is private records no yes, and `disallow` removes the record. It answers the record.
- `DELETE /v0/runners/repos/{owner}/{name}` (`potluck runners disallow`) disconnects it: its waiting jobs are refused
  at their next try; one that runs keeps its box. Unlinking an installation disconnects its repositories.
- `GET /v0/runners/repos` (`potluck runners repos`) lists the connected repositories of links in the caller's
  readable areas: who connected each and when, the yes, and what GitHub last said: its visibility and, for a public
  one, its approval rule for outside contributors' runs (`approval`: `all_external_contributors`,
  `first_time_contributors` or `first_time_contributors_new_to_github`).
- That rule is read at the yes and again by the sweep once `RepoCheckEvery` (an hour) has passed
  (`GET /repos/{o}/{r}/actions/permissions/fork-pr-contributor-approval`, Administration: read). A rule weaker than
  all outside contributors, or one GitHub does not answer, is the record's `warning` in plain words and is logged. A
  read that fails (GitHub does not find the repository, or answers another repository under its name) is logged and
  tried again after `RepoCheckEvery`, doubled for each failure in a row up to 16 times; an installation whose API
  budget is nearly spent is not read until the next sweep. These reads only inform: what is served is decided on
  GitHub's answer before each runner (below).
  It changes nothing that is served: a fork's run is never served for a public repository anyway, but it can take a
  runner meant for the repository's own job.
- A repository with jobs the broker took before this table existed (an upgrade) is connected, by `upgrade`, without
  the yes; a public one is refused until its owner gives it. Such a record has no repository id until GitHub's API
  names it (the check before a runner, or the sweep's read); a delivery's id never fills it.
- A lost job (below) is re-run only while its repository is connected as it would be served.

### Jobs

Deliveries:

- `POST /v0/runners/hook` (public) takes the App's deliveries. `X-Hub-Signature-256` is checked against
  `webhook_secret` before anything else (401); at most 5 MiB; de-duplicated by `X-GitHub-Delivery`; answered 202 once
  durable, or once dropped when `accounts` does not hear it (the check's deliveries are dropped the same way).
- Workers apply them in two lanes, oldest first: `queued` (a serve waits for a box to boot) on 3 workers, never two on
  one job; and `in_progress`, `completed` and `installation`, which end boxes and so never wait behind a serve, on
  one, in arrival order.
- The broker also asks GitHub (`runners/check.go`), so it serves jobs with no public address and catches up on jobs
  queued while it was stopped or asleep. For each linked installation it reads its connected repositories
  (`runners_repos`, afresh every round, so one connected a moment ago is asked about in the next round and one
  disconnected is not asked about again), and lists each one's runs that are `queued`, `in_progress` or `waiting`,
  and the jobs of the runs that are new or changed (or not listed for 2 minutes). It never asks about a repository
  that is not connected, and every call it makes uses a token for that one repository (`actions` and `metadata`
  read, cached until 5 minutes before expiry and revoked when the broker stops): it holds no token for a whole
  installation. Minting them is App calls, not counted in the installation's budget: about one an hour per
  connected repository, all of them in the first round after a start. A disconnected repository's token stays in
  the cache, unused by the checker, until it expires or the broker stops. What it finds becomes the same `workflow_job` deliveries: `queued`
  for a job the broker has no record of, `in_progress` for one on a runner of its own, `completed` for the jobs of a
  run it holds a job of. The workers apply them through the checks below, as a webhook's; a webhook's copy of one
  is harmless.
- With no `public_url` it asks every 15 s; with one, GitHub's webhooks serve and it catches up every 5 minutes. Both
  ask at the first tick after start, and after a wake from sleep: the schedule is by the wall clock, which runs on
  while a Mac sleeps. A clock stepped back counts as due too, and costs the budget nothing. Many repositories
  stretch the round so it stays under 300 requests a minute.
- Every list is read page by page, every page a conditional request (`If-None-Match` with that page's last ETag):
  an idle repository costs only 304s, which GitHub does not count against the installation's 5000 calls an hour.
  It reads the newest 300 runs of each status, and 1,000 jobs of a run; a list longer
  than that is named in `cut`. A page over the 1 MiB read limit fails as such.
- What it spends is capped at 1500 counted calls an hour per installation (every page counts; five minutes' worth
  saved up at most). The bucket is kept in `runners_checks`, so a daemon that restarts again and again does not
  start with a full one. A round stops for an installation (between the pages of a list too) while GitHub says its budget is low, or after GitHub
  refuses for a rate limit (403 or 429) until its `Retry-After` or reset (a minute when it names neither), as
  waiting serves do. A round the cap cuts short is recorded and the next goes on from there (from that repository by name, though
  others were connected or disconnected meanwhile).
- A list that fails (one repository's runs, one run's jobs, or storing what a run's jobs say) is recorded and
  skipped; the round goes on with the rest. It is asked again after 30 s, then twice as
  long each time it fails again, up to 5 minutes, and stays in the record while it fails. A repository the App can
  no longer act for (taken out of the installation) backs off the same way; an installation GitHub no longer has
  (uninstalled: its tokens refused with a 404) backs off once for all its repositories, and the round stops there. A run's jobs list keeps its ETags only once every delivery made from
  it is stored, so one that failed to store is listed and stored again.
- `GET /v0/runners/setup` shows it as `check`: `mode` (`check` with no `public_url`, else `webhook`), `every_s`,
  `error`/`error_at` when a round could not begin (no App client, the links unreadable), and per linked
  installation `repos` (its connected repositories, the ones checked), `last_ok` (the last round in which every list asked for answered and none was skipped; at
  most a minute old), `error`/`error_at` (which lists failed in a round since: the repository and which list, never
  a token) and `cut`/`cut_at` (why the last round read less than everything) (migration `runners/0007_checks`).
  `failing_since`, at the top for rounds that could not begin and per installation for failed lists, is when the
  failing rounds in a row began: an installation's failing round that started within two catch-up intervals (or two
  of `every_s`, if longer), a minute and the last whole round's length of its previous one continues the streak (so a
  round that outlasts the rest on a dead network does not split it), a round with no failed list (complete, or only
  cut short) ends it, and a clock moved back past its start starts it again (migration
  `runners/0010_checks_failing`). `awake_since` is when the daemon last started or woke, from a heartbeat that beats
  apart from the rounds. `potluck ci doctor` check 15 fails on a streak of 30 minutes or three check intervals,
  whichever is longer.
- Creating the App and linking an installation still need `public_url` (GitHub returns the browser there: The App,
  Links); serving jobs does not.

Which jobs:

- A `workflow_job` `queued` whose labels name the pool's label (plain or `<label>:<size>`) is the pool's. With
  anything but that, `self-hosted` and GitHub's OS (`linux`, `macos`, `windows`) and arch labels, it is ignored with
  why. A job without the pool's label is another runner's, not recorded.
- It is served when its installation is linked, its repository is connected through that installation (a public one
  with its owner's yes), GitHub has not already finished it, its run is of that repository, and the run is not from a
  fork (the run's head repository is the repository) and was started by an event only someone who can push causes
  (`push`, `pull_request`, `workflow_dispatch`, `schedule`, `merge_group`, `release`, `repository_dispatch`; never
  `pull_request_target`, `workflow_run`, `issue_comment` and the like, which act on outsiders' input), unless
  `allow_fork_prs` and the repository is private. An internal repository (GitHub says `private` for it, but its
  whole enterprise can see it and propose code to it) counts as public everywhere here, as in `potluck ci doctor
  --probe`: it is connected only with its owner's yes (one connected without it, including one connected while
  internal repositories counted as private, has its jobs refused until `potluck runners allow <repo> --public`;
  a job refused before that yes is not served again, so re-run it), and forks' runs of it are never served. A
  visibility this version does not know counts as public too. The rule never looks at who started the run. A
  Dependabot (or any
  other bot's) branch in the repository itself has the repository as its head repository, so its `pull_request` and
  `push` runs are served like a person's; a bot's `pull_request_target` or `workflow_run` run is refused like anyone's,
  and so is a run of a bot working from a fork. A repository that is itself a fork is trusted like any other
  (GitHub's fork flag is not read): what its writers merge from its upstream runs on the pool as their own code does.
- The queued delivery's `repository` (its `private` and `id`) is checked first, to refuse early. A job may wait up to
  a day, and its repository may be made public, renamed or disconnected meanwhile, so just before each runner is
  minted (a first serve, a retry, a re-serve after a taken runner or a lost box) the broker reads the repository from
  GitHub (`GET /repos/{o}/{r}`) and checks the connection again on that answer. The run's checks are kept per job
  with the rule they were decided under: checks decided while `allow_fork_prs` served the private repository's forks
  and events are decided again, in full, once it no longer does (the repository went public, or the switch went off).
  A run that passes the full rules (its own branches, a trusted event) is recorded as passing them whatever
  `allow_fork_prs` says. The box's `jit_guard` is made from the same answer.
- A check GitHub does not answer fails closed (retried, then the job fails). A nearly spent API budget makes the job
  wait for its reset. Else the job is ignored and recorded with why; a why never names `allow_fork_prs`.

Serving one:

- The broker mints a JIT config on the job's repository (repo-level for an organization installation too: an org
  runner sits in a runner group every repo of the org may use, so a refused job of another repo with the same labels
  could take it), with the job's labels.
- The runner is named `potluck-<job id>` (runners named `zm-<job id>` before the rename are still cleaned up and
  adopted).
- It acquires a box placed as below, with request id `gh-<job id>`, `ttl_s` `job_ttl_s` (default 3600), that
  `jit_config`, and a `jit_guard` of the job's repository with the trusted events above. A runner minted while
  `allow_fork_prs` serves its repository (GitHub says it is private, just before the mint) on a box whose driver
  lists `github-jit-guard-private` gets an open guard (`open_if_private`), whatever run it was minted for: it also
  serves a job the trusted events refuse (a fork's, another event's), but only while that job's own event payload
  says the repository is private, checked by the box before the job's first step. GitHub gives a runner to any
  queued job of the repository with its labels, so a fork's job queued beside an own branch's may take the own
  branch's runner, and runs there while the repository is private. On a box whose driver does not list it the
  runner gets the trusted events' guard: a fork's job GitHub hands that runner is stopped by the box and fails on
  GitHub. A job whose run passes only under fork serving goes only to a box that lists it (it waits for one; with
  none in the pool it waits until its job ends). Other jobs are placed in placement's usual cost order, so in a
  pool that mixes such boxes with older ones a fork's job can still meet a strict runner. On an open runner the broker decides a job that
  starts from GitHub's answer then, the box from the run's payload: a run created while the repository was public
  is stopped by the box even if the repository is private again by its start. So a redelivery makes nothing new. The sweep renews it while the job runs.
- Once `allow_fork_prs` no longer serves a repository, its runners with an open guard are retired as
  soon as the broker learns of it: the switch off (at its next start or settings save, and again at every sweep, every
  5 minutes, while it is off and such an idle runner is left), or GitHub saying the
  repository is public or internal (every read of it: before any mint for it, when its owner switches it on, by the hourly
  repository check, before a re-run, or when a job's start on one of its runners is decided again; the App does not
  receive GitHub's `repository` event). Each one no job
  has started on, and that GitHub does not list busy, has its runner deleted and its box released, and its job is
  served again after its run is decided again, as for a box taken back before its job started (it uses up none of the
  job's serves; the end of the serve and its re-serve are one step, so a box lost at the same time does not serve the
  job twice): an own branch's job gets
  a runner with the trusted events' guard, a fork's is `ignored`. A runner a job has started on is left: that job was
  decided when it started, runs to its end, and a GitHub runner takes one job only, so the runner and its box go when
  the job ends. A runner GitHub lists busy is not asked about again until the next sweep (GitHub can list a
  runner busy whose job was cancelled before it started). A runner list GitHub refuses the App (404, or 422 for the
  repository's token: the repository gone, out of the installation, or no permission for its runners) counts as
  no runner busy: the box is released, as at any job's end, and the runner is left registered with nothing behind
  it. One job GitHub fails for does not hold up the others. A retire GitHub does not answer is tried again from the
  start, up to 6 times, and a new one is not queued for the same
  repository while it waits; once dropped, the next read that learns the change (for the switch, the next sweep)
  queues it again. The retire is applied in order with `in_progress`; a job GitHub gives the runner between the
  retire's read of the runner (not busy) and its deletion keeps it (GitHub refuses to delete a busy runner): the
  retire is tried again, finds it busy, and leaves it to the decision at the job's start. A runner
  minted before this was recorded counts as open.
- A job's k-th runner (a refused box, a takeover, a lost box) is `potluck-<job id>-<k>` with request id
  `gh-<job id>-<k>`.

Placement:

- A size in `sizes` goes to its binding alone.
- Else the job may go to any box binding of the pool project, its own or a host's (`<binding>@<machine>`), whose
  host is `trusted` (a `jit_config` never goes to another host: placement skips one and the acquire refuses it),
  whose `describe` lists `github-jit` and `github-jit-guard`, says it is available, and matches the job's size and OS
  (`linux` when it names none, as GitHub's own runners; `macos` is a `darwin` box; arch labels are not checked: no
  describe states one), whose lifetime cap (`ttl_max_s`) holds `job_ttl_s`, and that did not refuse a box in the last
  2 minutes. So a binding that says available but refuses costs at most one runner minted and deleted per 2 minutes,
  however many jobs wait.
- They are ranked like sandbox placement, the smallest first (fewest CPUs, then least memory), then the one serving
  fewest broker jobs, then by name. Up to 3 are tried in turn, the runner deleted after each refusal.

Waiting and failing:

- When none fits or takes it, the job is `waiting` with why (each box and why not, drivers quoted; first, when no
  runner box of the pool has the size it asks for, that and the sizes there are), and tried again at
  most 5 minutes apart (sooner once a served job completes, except a serve backing off after an error) for as long as
  GitHub has it queued (asked at most every 15 minutes; a `completed` delivery ends it at once; a day at most). It is
  never dropped after a number of tries.
- A delivery fails out after 6 failures in a row of anything else (a check GitHub does not answer), its job `failed`
  with why.
- While an installation's API budget, from GitHub's `X-RateLimit-*` headers, is at or below 10% of its hourly limit
  (at most 100 calls), a waiting serve makes no call until the reset.

A box lost before its job started:

- Every 30 s the broker asks each served job's box (run-target status, no GitHub call while the box is there).
- One that ended (`reclaimed` by its Mac's owner, `expired`, `lost`), or whose host has not listed it for 5 minutes,
  is, once GitHub says the job is still queued (one call) and its runner is deleted (GitHub refuses a busy one: a job
  that did start there lost its box while it ran, below), released in the background, and the job served again. A box
  its host took back (`reclaimed`) or stopped serving (a drain, a close, its owner back) does not count against the
  job's serves: the job is served again each time, until a box keeps it or GitHub cancels it. A box that failed on its
  own (`expired`, `lost`, gone, ended by itself, or its runner did not start within 300 s) counts like a takeover:
  three serves in all.

A box lost while its job ran:

- The same check covers boxes a job runs on. GitHub cannot move a job off a runner that vanished: it keeps the job
  `in_progress` until its lost-communication timeout (about 10 minutes) fails it, has no call that cancels one job
  (only a whole run), refuses to delete a busy runner, and re-runs a job only once its whole run has completed.
- So when the host itself says it took such a box back or lost it (`reclaimed`, `expired`, `lost`) or that it has no
  such handle, the broker asks GitHub about the job that runs on it (the job that took the runner, not always the one
  it was minted for) and records the loss only if GitHub still has it `in_progress` on that runner. A job that
  reported a conclusion first ended for its own reasons and is never re-run. The box is released in the background;
  the job's row is `lost` with why. If GitHub does not answer 10 checks in a row, or has no such job, the box is let
  go without a re-run, with why.
- The rule for what counts, by the box's state as its host reports it:
  - `reclaimed` (the machine's owner or its admission took it back, or it went over a volume cap), `expired` (its
    lease ran out) and `lost` (the VM or its supervisor died under it; or the host's driver or handle ledger has no
    such handle, which the pool's machine records as `lost`): the host took the box or lost it. These count.
  - `expired` under a running job means the host's own lifetime cap ended the box, or the host refused the broker's
    renewals. It is counted as taken and re-run like the others, twice at most. A job that outlives the cap will
    likely hit it again on each re-run, on that host or another with the same cap.
  - `ready`: the box is there. A runner that finishes its job and exits leaves its box `ready` until the broker
    releases it at `completed`, so a job's normal end never shows as an ended box ([protocol.md](protocol.md): a box
    is never `running` or `ended`).
  - `ended`: the box's own process finished by itself. No conforming box reports it; from a driver that does, it is a
    normal end and never a loss. A job that ran on it is closed by its `completed` delivery (or the sweep), its row
    never says the machine took its box back, and it is not re-run whatever its conclusion. A job that had not
    started on it is served again.
  - `released`: only the broker or the box's holder does that; nothing to do.
- "No such handle" is only the host's answer about the handle itself: its driver's `not_found` to `status`, or its
  ledger having no such handle for the pool (over the relay: still `not_found`, with a mark beside it in the report's
  payload), or the pool's machine having no record of it. Any other `not_found` on the way (the host says "this
  machine is not in that project" after it dropped out of the pool or while it restarts, the binding is no longer
  shared, the relay does not know the machine) says nothing about the box: the box's record stays as it was, its
  status answers `unavailable` with the host's words, and the job is treated as on a host that does not answer.
- Mixed builds: a pool's machine on a build without the mark reads the same `not_found` it always did. A host on a
  build without it sends no mark, so its `not_found` for a box that is gone is not taken as gone (it is what a host
  out of the project answers too). The backstop is the run-target sweep: 15 minutes past the lease it records the box
  `lost`, an ended box's status answers its record without asking the host again, and the broker then acts on `lost`
  as above (a job not yet started is served again that late, not within a check).
- A host that is only unlisted or does not answer (its daemon restarts, the network to it drops) has said nothing:
  a job running there keeps its box, renewed, and its box is never released under it. If GitHub then fails the job,
  it is not re-run (the broker cannot tell a dead host from a job that failed on its own). Once GitHub has ended the
  job, its row ends when the host releases the box, or, if the host never answers, once the box has gone a whole lease
  without a renewal (the host ends such a box itself; a daemon restart counts the lease again from the restart).
- What the states cannot tell apart: a job that ends its own VM (powers the guest off) or kills its box's container
  leaves a `lost` box while GitHub still has the job `in_progress`, and is re-run like a taken box (twice at most).
- When GitHub ends the job (`completed`, or the sweep if that delivery never comes) with any conclusion but
  `failure` (a person cancelled it), it is not re-run. Else, once the run has completed and GitHub has ended every
  lost job of it, the broker asks GitHub to re-run it, unless the run ended any other way than `failure` (cancelled
  by a person, or by a newer run under `concurrency: cancel-in-progress`, which a re-run could cancel in turn): the job
  alone (`POST .../actions/jobs/{id}/rerun`) when it is the run's only lost job, else the run's failed jobs (`POST
  .../actions/runs/{id}/rerun-failed-jobs`, which also re-runs jobs of that run that failed on their own). The job comes
  back `queued` under a new id and is served like any other.
- A run is re-run this way while its attempt (GitHub's `run_attempt`, so a person's re-runs count) is below 3: two
  re-runs at most, however often a host takes boxes back.
- The job's row and the daemon's log say what happened: "the lending machine took its box back while the job ran
  (its box on c2m8@mac ended (reclaimed)); re-queued: attempt 2 of 3", or why it was not re-run.
- Re-running needs the App's repository permission Actions: write. Without it the loss is recorded and logged with
  what to add, the job stays failed, and nothing retries.
- This came with migration `runners/0005_lost` (a column and a table), which is one-way: there is no down
  migration, so a machine that has started a build with it must not be put back on an older build with the same
  home. Fix forward, never roll back.

Other runs taking the runner:

- Every job's state, why and box are listed by `GET /v0/runners/jobs` (newest first, the caller's readable areas;
  `potluck runners jobs`).
- GitHub still gives a repo's runner to any job of that repo with its labels, so a job the broker refuses there (a
  fork's run, a `pull_request_target`) can take it: the box's hook stops it before its first step. The repository's
  visibility and link are the served job's, so the hook checks the rest. It cannot see labels, so a job of a trusted
  run without the pool's labels passes it and runs until its box is released. A job the broker would serve, of any
  run, passes.
- `in_progress` is checked against the same rules: a runner of a serving job taken by a job they refuse (or cannot
  check) has its box released and the runner deleted at once, and its job is served again in the serve lane while
  GitHub still has it waiting (runner `potluck-<job id>-<n>`, request id `gh-<job id>-<n>`, at most 3 serves per
  job; then `failed`). A job they would serve taking it is a swap, left alone. The refused job, if it is the pool's,
  is recorded `ignored` with why ("refused when it started on runner ...").
- On a runner with an open guard, every job that starts, the runner's own job too, is decided again
  at `in_progress` (unless its run already passed the full rules, its own branches and a trusted event, which hold
  whatever changed) from GitHub's answer then (`GET /repos/{o}/{r}` and the run, never the delivery's word) and
  `allow_fork_prs` as it is then: once the repository went public or the switch went off, a fork's job there has its
  box released and the runner deleted, and the runner's own job refused this way is `ignored` with why ("refused when
  it started on its runner ..."), not served again. GitHub is asked up to 3 times within about a second and a half;
  a check it still does not answer refuses (the why says so when the installation's API budget is nearly spent).
  This is the backstop for what the retire missed, and it is after GitHub started the job, so a step may already
  have begun. The box's guard reads the job's event payload, which is its run's, fixed when GitHub created the run:
  a fork's run created after the repository was made public or internal is stopped before its first step. The window
  left: a job of a run created while the repository was still private (a later job of it, or a re-run) that GitHub
  gives an open runner the broker has not retired yet, until the broker next reads the repository (above); and a
  fork's job that GitHub gives an open runner after `allow_fork_prs` went off and the broker began serving that, but
  before the broker retired the runner, that is a runner GitHub had already given a job before a retire read it (the
  broker serves forks until it reads the edit, by design), and, while GitHub does not answer the retire's reads, an
  idle one until a later try or sweep retires it.
- On a runner with the trusted events' guard (minted while `allow_fork_prs` did not serve the repository, or on a box
  that cannot check private), a job whose run passes only under fork serving (switched on since, or a fork's job
  GitHub handed that box's runner) is stopped by the hook before its first step
  whatever the broker says: it fails on GitHub, and the runner's own job is served again, as for a refused one.
- A box whose job ended or was taken while it started is released as `acquire` answers, and its runner deleted.
- `completed` releases the box the job ran on (the one whose runner it took, recorded at `in_progress`) and deletes
  that runner; the job that runner was minted for is served again if GitHub still has it queued. The completed job's
  own box goes too unless another job runs on it: then it stays, renewed, until that job ends (the sweep follows the
  job on the box the same way). For a pool job not yet served it records the job done, so a serve still checking it
  mints nothing.

Tokens and sweeps:

- Every installation token the broker mints names one repository, the job's, and only what its call needs: `actions`
  and `metadata` read for the checks and the lists, `administration` write for its runner (repo-level registration:
  GitHub has nothing narrower), `actions` write for a re-run. There is no exception: the broker never mints a token
  for a whole installation (the App's client refuses one with no repository). `members` read is used only by a linking person's user token, never in an installation token.
- Every 5 minutes a sweep releases boxes whose job ended without a delivery and deletes idle runners named
  `potluck-<digits>[-<n>]` or `zm-<digits>[-<n>]` that no live job holds. While `allow_fork_prs` is off it also
  queues the retire of idle runners that served forks, if any are left (see Serving one).
- Registration, deregistration and the sweep never run on a host.
- When the daemon stops, the broker revokes its cached installation tokens (`DELETE /installation/token`) instead
  of leaving them to lapse within the hour. A crash leaves them to lapse.

### The App

- `POST /v0/runners/app` (owner of the pool project; `{"org"?}`) answers a one-time page URL (24 h).
- The page posts GitHub's App manifest to github.com: `administration` write (for per-repository runners); `actions`
  write (read for the checks, write to re-run a job whose box was taken back) and `metadata` read; `members` read, for
  the link's org-admin check (an organization permission: an installation on
  a user account gets nothing from it); never `organization_self_hosted_runners`; event `workflow_job`; webhook
  `public_url` + `/v0/runners/hook`; callback `public_url` + `/v0/runners/oauth`; private.
- GitHub returns to `GET /v0/runners/app/created` (public, single-use `state`), which converts the code and writes the
  key, client secret and webhook secret under `$POTLUCK_HOME/secrets/runners/` (`0600`) and the ids into
  `config.json` (re-read first: edits made there since the daemon started are kept). No person copies a secret.
- The manifest's `setup_url` is `public_url` + `/v0/runners/app/installed`. The created page links to the App's
  install URL with a new single-use install `state` (1 h) bound to the owner who asked and the request's `area`.
  `POST /v0/runners/links` without `installation_id` mints one too (`install_url`).
- After an install GitHub returns there with `installation_id`, a claim anyone could type. The route spends the state
  only if the id is a number and the broker is set up, checks it is this App's installation and not linked to another
  area (the page does not name it), then answers a 303 to GitHub's authorize URL with a link state for that
  installation, area and requester. From there it is the link above: the sign-in, the admin check of the
  installation's account and the person's yes decide. No id is copied.
- An App whose "Request user authorization (OAuth) during installation" is on (GitHub's button reads "Install &
  Authorize") has no setup URL: GitHub returns to the first callback URL, `GET /v0/runners/oauth`, with
  `installation_id`, `setup_action`, its own `code` and the install URL's `state`. Without a link state such a return
  is handled exactly as `setup_url`'s (the same checks, the same 303 to a sign-in the daemon starts; the person has
  authorized already, so GitHub comes straight back with a code for that link state).
- GitHub's install-time code is never exchanged, and a return with no live install state links nothing and mints
  nothing: it names `potluck runners link <installation-id>`, whose sign-in and yes decide.

### Setup

`potluck runners setup <pool> [--url https://host] [--org] [--name] [--area] [--repo owner/repo]...`, the pool project's owner on the
machine's socket (`POST /v0/runners/setup`, local):

- It writes `runners` `pool.project`, `public_url` (the flag, else the one set, else `tunnel.url`; neither is an
  error; once the App exists a different `--url` is refused, since GitHub calls the App's URLs) and `listen` (the
  one set, else a free `127.0.0.1` port) into `config.json` as it is on disk.
- It opens that listener live (no restart; a changed address replaces the old one).
- When `public_url` is the hostname of this home's named tunnel, it rewrites cloudflared's ingress for the broker's
  paths and restarts it only if it changed (a quick tunnel cannot route paths: refused).
- It prints one URL: the App's creation page, or with an App the install URL.
- With `--repo` (`repos` in the request; owner/repo, all of one account, at most 20) the install URL is GitHub's
  `/apps/<slug>/installations/new/permissions?target_id=<owner id>&repository_ids[]=<id>...&state=<state>`: the
  install page opens on that account with those repositories ticked, and the state is the same single-use state as
  the plain URL's. Ids are read from GitHub as the machine's owner does (`gh`, as the doctor does); a repository
  that cannot be read, or a second account, is refused before anything changes. When the App is created by the
  same setup, the choice waits with the creation state and is on the install link GitHub's return shows. GitHub
  does not document these two parameters; that has not been checked live yet.
- `potluck runners link [<installation-id>] [--area]` prints the install URL, or for an existing installation GitHub's
  sign-in URL (an App made before `setup_url` needs it set on GitHub; the CLI says so). `potluck runners unlink <id>`.
  `potluck runners ls` shows the pool, `public_url`, the listener, the App, the settings (the label, the job limit),
  how the broker checks GitHub (webhooks or asking, and each installation's last complete round, errors and cuts),
  sizes, the pool's machines, links and the connected repositories; away from the daemon's socket (a remote client,
  a sandbox) or with a token, only the links and repositories, since the setup is the machine owner's. `potluck runners repos` lists the
  connected repositories with what keeps one from being served (a public or internal one without `--public`). Sizes are not
  set by setup.
- Everything is checked before anything changes; a new pool drops every pending state. A `public_url` is https with
  a host and no path.
- A taken or bad `listen` at daemon start leaves the broker running without its listener (logged), so `potluck
  runners setup` can pick a free port.

### Host capacity reports

A lending host that runs its own admission controller (for example one that also decides between the host's own CI
runners and pool boxes) can publish what it has room for, so routers on cloud CI runners, which cannot reach the host, pick
the host or another runner from one fresh answer. The broker keeps the report as an opaque JSON object; its shape
is the host's (a `schema` field naming it is the reader's best check).

- `PUT /v0/runners/capacity/<host>?ttl_s=N` (authed), the report as the body: a JSON object of at most 64 KiB,
  kept compacted for `ttl_s` (10 to 600, default 60) and then dropped. Only `<host>`'s own machine publishes it:
  its replica's node-bound token, whose machine is `<host>`, seeing the pool project; or the authority's owner on
  its socket. Any other caller is refused (`403`). On a member machine the team proxy sends it to the pool's
  authority once (no write id: the next report replaces it). `<host>` is a machine name: lowercase letters, digits
  and `-`. Reports are held in memory, at most 256 hosts: a restart loses at most one report, which the host
  publishes again within seconds.
- `GET /v0/runners/capacity/<host>` (public, on `listen` too): the live report exactly as kept,
  `content-type: application/json`, `cache-control: no-store`; `404` when the host has none or its ttl passed. A
  reader treats `404`, no answer and a stale report the same way: not this host.
- `potluck runners capacity <host> [--ttl-s N] < report.json` publishes stdin as the report (on the host, as the
  user its daemon runs as; under zm, `zm runners capacity <host> -P <pool>`).

## Secrets issuers

`issuers/`. An issuer mints a credential on the authority for one box, cut down to what the request names and
inside the issuer's config ceiling, renews it while the box lives and revokes it at the issuer when the box ends. It
hands the value to the box through a delivery interface (the sealed transport and the landing in the guest are
separate parts) and never writes it anywhere itself. Two kinds exist: `github-token` (a `github-app` issuer) and
`potluck-token` (a `potluck` issuer). The daemon (`wiring.Compute`) runs at most one issuer of each: two of one kind,
or one it cannot build (a key it cannot read), leave that kind unminted, and the daemon logs why. Both kinds share
one lifecycle (`issuers.Minted`: admission, renewal, revocation, ledger, restarts); a kind's own part is a backend. Each
issuer starts on its own, so one whose source is down holds up neither the other's issues nor its renewals; the
runner broker passes over a job grant of a kind no issuer here mints.
Their callers are the runner broker's job grants (below) and, later, identities asking through the door.

- It writes into a box as the authority (`rt.Door`: the box secrets verb without the holder check and reserved names,
  every other gate kept: a ready box on a pinned image before its expiry, a trusted host, a driver listing
  `box-secrets`). It asks the box's machine class and whether the box lives from the run targets' own record.
- It starts after run targets and before the broker, and stops after the broker. Nothing is issued until its start
  has settled what an earlier run left; it ticks again after every issue, sweeps its boxes against the run targets'
  record every 5 minutes (a box the record says ended or never had is ended; one the record cannot be read for is
  kept), and every box end the run targets report revokes that box's tokens.

### Config (`secrets.issuers`)

```json
"secrets": {"issuers": {
  "gh": {"kind": "github-app", "app_id": 123, "private_key": "file:/path/to/key.pem",
         "ceiling": [
           {"repo": "acme/app", "permissions": {"contents": "write", "pull_requests": "write"}},
           {"repo": "acme/*", "permissions": {"contents": "read"},
            "workflows": [".github/workflows/release.yml"], "events": ["push", "workflow_dispatch"]},
           {"repo": "acme/infra", "permissions": {"contents": "write", "workflows": "write"},
            "elevated": ["workflows"], "reason": "tasks the dispatcher cleared for workflow files"}]},
  "broker-app": {"kind": "github-app", "use": "runners.github", "ceiling": []}}}
```

- The section is read strictly: a key it does not know, at any level (`"workflow"` for `"workflows"`), refuses the
  whole section, naming the key, and no issuer runs. The daemon still starts, and saving `config.json` keeps the
  section as written.
- An issuer is named by lower-case letters, digits and `-`. `kind` is `github-app`, the only kind.
- The App is `app_id`, `private_key` and `api_url` (default `https://api.github.com`; https, or http only to a
  loopback address), or `"use": "runners.github"` for the runner broker's App, with those three left unset.
- `private_key` is a secret reference (`file:`, `keychain:`, `env:`), never the key: an inline key, or anything
  that is not a reference, is refused. The key is read when the issuer starts: rotating it takes a daemon restart.
- `ceiling` is the most the issuer ever mints (the operator's bound); with no rule it mints nothing. Who may ask
  for what is a grant's ceiling, checked by the grants check; both must allow a token.
- A rule is a repository (`owner/name`, or `owner/*` for every repository of the owner) and the most each
  permission may be (`read` or `write`). A request must fit inside one rule: permissions are never added up across
  rules.
- `workflows` (files under `.github/workflows/`) limits a rule to GitHub jobs of those workflow files; such a rule
  never serves a person or a controller.
- `events` are the job events a rule serves: `push` and `schedule` by default. `workflow_dispatch` and
  `repository_dispatch` only when listed: their inputs are chosen by whoever dispatches, which is anyone with write
  access to the repository or any token that can dispatch, so a workflow that uses them must treat them as
  untrusted. No other event ever gets a token (`pull_request*`, `workflow_run`, `merge_group`, `release`, ...).

Permissions are an allowlist in three tiers (names and levels as GitHub's "Create an installation access token for
an app" and "Permissions required for GitHub Apps" give them):

- Plain, `read` or `write` under any rule: `contents`, `pull_requests`, `issues`, `discussions`, `metadata`.
- Elevated, `read` like plain, `write` only when the rule lists the permission under `elevated` and gives a
  `reason`, and the request gives a reason too: `workflows` (GitHub takes only `write`), `actions`, `checks`,
  `statuses`, `deployments`, `packages`, `attestations`, `pages`, `security_events`, `secret_scanning_alerts`,
  `vulnerability_alerts`, `merge_queues`.
- Everything else is refused, among them `administration`, `secrets`, `environments`, `repository_hooks`,
  `dependabot_secrets`, `codespaces`, `repository_custom_properties`, `members`, every `organization_*` and user
  permission, and any name GitHub adds later.

What the plain and elevated permissions amount to, so a ceiling is written knowing it:

- `workflows: write` lets the token change workflow files, and a workflow can read every secret of the repository:
  it amounts to reading them all.
- Pushes made with an App token do trigger workflows (unlike the job's own `GITHUB_TOKEN`).
- `contents: write` can push to the default branch when the branch is unprotected or the App is on a ruleset's
  bypass list.
- `pull_requests: write` lets the token submit reviews, approvals included, as the App.

### What a box can get (`github-token`)

- An installation token of the issuer's App for exactly one repository with only the permissions asked.
  `metadata` read is always added: GitHub puts it on every repository token.
- The request to GitHub names that repository and those permissions (`repositories` and `permissions` on
  `POST /app/installations/{id}/access_tokens`). Each box gets its own token, never one from the broker's cache, so
  revoking one ends one box. Token calls never follow a redirect.
- GitHub's answer is checked: `repository_selection` must be `selected`, the repositories exactly the one asked, the
  permissions exactly those asked, and `expires_at` within the next hour plus 5 minutes (a clock further off is
  refused). Anything else is revoked at once (retried until GitHub confirms) and refused.
- Refused before any call to GitHub: a repository or permission outside the config ceiling, a permission off the
  allowlist, an elevated write without its reasons, a box on a machine that is not `trusted` (the class is asked
  at every issue and every renewal, never taken from the request), a request the grant check refuses, and for a
  GitHub job: a run whose head repository is not the repository (a fork's) or is unknown, no run id, an event the
  rule does not serve, a ref other than `refs/heads/<default branch>`, a job of another repository, a workflow the
  rule does not name. The job rules are package `jobrule`, shared with the grants check.
- The grant check is required (`issuers.GrantCheck`, over the grants check). It judges the caller as authenticated,
  its token's cap included, never the identity's whole rights, and names the grant the token is issued under; a
  caller's own word for the grant is ignored, so removing that grant revokes its tokens. An identity's assignment
  re-issued after a restart has no caller, so the check refuses it (fail closed); a job's is checked again in full.
- A box asking again for the same name and request gets what it holds (waiting for one being issued); a different
  request under the same name is a conflict.
- A 5xx, a 429 or a rate-limit 403 from GitHub is tried 3 times in all, waiting at most 30 s; a limit that resets
  later fails at once. A failed mint, a failed delivery or a ledger that refuses the row fails closed, and whatever
  was minted is revoked.

### What a box can get (`potluck-token`)

A token of the pool's own (the same tokens table `potluck token mint` writes and every caller authenticates
against), for a box to call its authority back: to comment on a work item, say, with no long-lived login anywhere.

```json
"secrets": {"issuers": {
  "pool": {"kind": "potluck", "as": "ci",
           "ceiling": [{"project": "oncall", "role": "member"},
                       {"project": "oncall", "area": "ci", "role": "member",
                        "repo": "acme/infra", "workflows": [".github/workflows/nightly.yml"], "events": ["schedule"]}]}}}
```

The first rule serves boxes of project `oncall` (a person's, say). The second serves the `nightly.yml` job of
`acme/infra`, a repository the runner broker serves: that job's box is in the pool project (`runners.pool.project`), not in `oncall`,
and its token acts on `oncall` area `ci`, as `ci`, which must hold `member` there.

- A rule is a project (slug or ULID; a slug is looked up at each issue, so a rule follows the slug), optionally one
  area of it (none: any area), and the most a token's role may be:
  `guest` or `member`. `owner` is never minted for a box. `workflows` and `events` limit a rule to GitHub jobs as for
  `github-token`. With no rule it mints nothing.
- A grant's ceiling is `{"role": "member"}`; a request's params `{"role": "member"}`. Both the grant and a config rule
  must allow the role.
- Which project a token acts in. Under a rule of the box's own project: the box's project, the box's area (none: the
  whole project). A GitHub job the runner broker serves runs in a box of the pool project, in the area of its
  installation's link, so a job's token for the project whose work it does comes from a job-only rule (one naming
  `workflows` and the `repo` it serves, as `owner/name`) of that project: the token acts in the rule's project and the rule's area (none: the whole project),
  never in the pool project. Such a rule serves only a job of its `repo` (a rule of another project without one
  serves nothing: the pool's admin of any area may grant a job, so the rule names the repository the project's
  owner means) that passes the job rules (as for `github-token`: its own repository, `refs/heads/<default branch>` with the broker's reviewed and pinned checks, an event and
  workflow file the rule lists), and only in the broker's own box for that job: in the pool project, in the area
  linked to the job's installation, recorded as guarded to the job's run (`runners_job_secrets`). It is asked at every
  issue and renewal. A rule of another project without `workflows` serves nothing, so a person's box never gets
  another project's token. The rule's project must exist and its area be an area of it. Rules that would put one
  token in two projects or areas refuse it, so adding a job-only rule of another project beside a pool-project rule
  without `workflows` (which also serves jobs) makes that job's token refuse until one of them is narrowed.
- The token is capped to exactly that project and area and the asked role. A person's token acts as that person; a
  GitHub job's acts as the issuer's `as` identity (no `as`: jobs get none). That identity must already hold the role
  there, on the token's project and area (`potluck grant`): the token only narrows it, and an identity that does not
  hold it is refused before anything is minted.
- It lives at most an hour, never past the box's lease, and for a person who authenticated with a token never past
  that token (so one box's token cannot be traded for a longer-lived one); with less than a minute left it is not
  issued. It is renewed at two thirds of its life, like `github-token`; each renewal re-reads the lease and asks the
  kind's whole check again (the rule, the token's project included, and for a job under another project's rule the
  broker's box; the identity still exists and holds the role; the caller's token still works): a renewal
  it refuses ends the token; one the registry cannot answer is tried again each minute, the old token staying. A mint
  whose registry stamp lands past the cap (the mint stalled) is revoked and refused.
- A token minted for a caller who used another token (one box's, say) is not revoked when that token is: it lives
  at most until that token would have expired, and is not renewed once it is revoked. A caller on the local socket
  or the tailnet without a token is bounded by the lease and the hour only.
- Revoking is `token revoke` on the token's row, at every way a box ends (below). The tokens table outlives the
  authority, so after a crash the next start revokes every token an earlier run held (by its hash, recorded
  `revoked`), rather than leaving it to lapse.
- The ledger names it by `hashed_token`: the SHA-256 (hex) the tokens table looks a secret up by. The value is
  never stored or logged.
- The box gets one file under the grant's name: a box credential (below), holding the token and a key made for that
  token alone (the per-box key), never written anywhere by the authority. The token is bound to the key's id
  (`tokens.box_key`) and works only on a request that key signed over the tunnel: not as a bearer token on the
  socket or the tailnet, not with any other key. Each renewal mints a new token with a new key and replaces the file
  whole. A box token is never a team replica's (`tokens.node` stays empty): it cannot link or unlink a GitHub login
  or leave a team as the member, and it holds no subscription (pushes, runner links, previews read later).
- With no tunnel URL (`tunnel.url`) a box could not reach the authority, so nothing is minted: the request is
  refused naming it.

### Calling the authority from a box

A program in a box reaches the pool's machine with the credential file alone: no daemon in the box, no other file,
nothing long-lived. The file (`kit/tunnel` `BoxCredential`) is one JSON object:

```json
{"kind": "potluck-box/1", "token": "potluck_…", "key": "<hex ed25519 seed>",
 "url": "https://pool.example", "home": "<the authority's key fingerprint>"}
```

- `potluck` reads it when `POTLUCK_BOX_CREDENTIAL` names it, `$POTLUCK_SECRETS/<the grant's name>` in a box: every
  command then talks to that authority, as the token's identity, capped to the token's project, area and role (the
  box's, or a job-only rule's: above). It
  starts no daemon.
- Each request is signed with the box's key for the pinned authority, as a member's daemon signs its own (the
  tunnel's signed exchange); only answers the authority's key signed are read. A box has no seal key, so a call
  carrying a secret is refused before it leaves.
- Code that is not `potluck` uses `tunnel.ReadBoxCredential` and `tunnel.BoxClient`, which answers a
  `kit/client` client.
- The conformance suite's group `box-authority` checks it from inside a real box (`docs/conformance.md`).

### Renewal and revocation

- The lifecycle owner calls the issuer's tick when it last said to, never later than a minute. At two thirds of a
  token's life (40 minutes) it mints a new one with the same repository and permissions, asks the class and the
  grant again (and, for a job's token, whether the job still holds its box: below), delivers it in place of the
  old, and revokes the old one 5 minutes later. Renewals run several at
  once (8 by default). A renewal that fails is tried again each minute; the old token stays until it expires, and
  then the box has none (closed): the issuer still revokes it, retrying until 10 minutes past its expiry.
- Every way a box ends revokes every token issued for it (`DELETE /installation/token`, authenticated by the token
  itself): released, expired, reclaimed, lost, its machine no longer trusted, its job completed or taken over, its
  grant deleted, and a sweep that finds the box gone. A revocation GitHub does not confirm is retried until it does
  or the token is 10 minutes past its expiry.
- A machine's class changing (an owner setting it, `POST /v0/team/machines/{machine}/class`) has every box holding
  a token, or being issued one, asked its class again at once: a box whose machine is no longer trusted has its
  tokens revoked within seconds, and one being issued revokes what it minted. A class can also fall without being
  set (the machine's join ends); every box is asked again every 5 minutes as well, and at every renewal.
- A job's token is renewed, or issued again after a restart, only while the job still holds its box: the broker's
  record of the box says issued (a completion records it ended, in the registry, so a restart never undoes it) and
  GitHub says the job holding it is in progress on the box's runner. GitHub saying it finished records it ended.
  So a job that completed while the daemon was down, or whose `completed` was lost, never gets a token again; GitHub
  not answering is retried, never taken as a yes.
- Tokens are held in memory only: nothing stores them. What is stored is the assignment: which box holds which
  request (never a token), removed when the box ends.
- When the authority stops cleanly it revokes every token it holds and keeps the assignments. At the next start,
  every assigned box its owner says still lives is issued a new token through the whole check again (config
  ceiling, class, grant), delivered in place of the revoked one; an assignment whose box is gone, or that no longer
  passes, is dropped. So a restart cuts a box off for as long as the restart takes.
- After a crash the tokens cannot be revoked: each one an earlier run minted and never revoked is recorded
  `expired_unrevoked` and lapses at its expiry, within the hour; the boxes are issued new tokens as above.

### The record

- Renewals, revocations and the issuer's refusals are published to the pool's event log (`secret.renewed`,
  `secret.revoked`, `secret.refused`) for readers who may operate in the box's area. Each names the issuer, the
  subject (`identity:<id>` or the job), the grant, the box, its host and the host's class, the repository and
  permissions, the elevated ones, the reasons, the expiry and the token's `hashed_token`. The event's project is the
  box's; a potluck-token's names where it acts (`token_project`, a ULID, and `token_area`), which for a job under
  another project's rule is not the box's. Never a token.
- `hashed_token` is the base64 of the token's SHA-256, the form GitHub's audit log searches by
  (`hashed_token:"..."`, URI-escaped in the REST API). Installation tokens are among the tokens the audit log
  records; Git events are found only in an exported audit log, not by search.
- `issuers_ledger` in the registry is the issuer's own recovery record (issued, renewed, revoked, failed, expired,
  expired_unrevoked, maybe_delivered, without values): what lets a start settle a crashed run. Its `project` and
  `area` are the box's; `token_project` and `token_area` where a potluck-token acts. `maybe_delivered`
  is a delivery that failed after it was sent (the driver timed out or its host went away), so the value may be in
  the box; the token is revoked after it, and the issue answers an error of its own (wrapping
  `rt.ErrMaybeDelivered`) so no caller delivers it again as if nothing arrived: the broker's job gets nothing, and
  a re-issue after a restart is not retried. A renewal is retried all the same: the box may now hold the revoked
  new token, and only a renewal puts a live one back. `issuers_assignments` holds the
  assignments.
- A deliverer's error or panic is passed on with the value replaced by `[secret]`.

### What the GitHub App needs

- The App must itself hold every permission a ceiling names (for example `contents` and `pull_requests` write): a
  token cannot carry more than its App. The broker's manifest asks only for what runners need, so an issuer that
  uses `runners.github` needs the App's permissions raised in its settings on GitHub.
- An organisation owner must approve new permissions on each installation (org settings, GitHub Apps) before
  GitHub mints tokens with them; until then the mint fails and nothing is issued.
- The App must be installed on the repository's account, with access to the repository.

### Not covered

- How the value reaches the box and where it lands: the delivery interface's implementers.
- Code in the box can use or copy the token for its life. It is bounded to one repository, the permissions asked,
  and an hour, and revoked when the box ends.
- A token GitHub minted on a retried POST whose first answer was lost is held by nobody; it lapses at its expiry.
- Tokens of a crashed authority, as above.

## Job grants

`runners/jobsecrets.go`. A job grant (`"to": {"job": {"repo", "workflow", "ref", "events"}}`, kind `github-token`,
scope `box`) gives a GitHub job the broker serves a token in its runner box. No other job ever runs a step in that
box, and nothing is minted before GitHub says the right job holds it.

- **At serve**, before the runner is minted: if a job grant of the link's area names the job's repository, the
  broker asks GitHub about the run and the repository. The job is GitHub's answers only (never the box's or the
  workflow file's word): the run's repository and head repository with their ids (a fork is a head repository id
  other than the repository's, or none), its workflow file, its event and run id, and its ref. The ref is
  `refs/heads/<default branch>` only when the run's head branch is the default branch and its head commit is that
  branch's tip or an ancestor (`GET /repos/{o}/{r}/compare/{branch}...{sha}`) and no tag carries the branch's name
  (`GET /repos/{o}/{r}/git/ref/tags/{branch}` answers 404). These two are asked only when a grant would name the run
  on the default branch, so the repository's other jobs never wait on them. A re-run (`run_attempt` above 1)
  gets the ref only while its head commit is still the branch's tip: anyone who can re-run could otherwise run an old
  commit's workflow file, one since fixed, with a fresh credential. A run that called a reusable workflow from anywhere but its own repository at its own commit gets nothing.
  Two more checks, asked only then, from GitHub with read-only tokens: the default branch counts as reviewed only
  when an active ruleset requires a pull request on it with at least one approval of its last push
  by someone other than its pusher (`require_last_push_approval`; dismissing stale reviews alone lets a pusher
  approve their own push) and GitHub says this App can never bypass
  that ruleset
  (`GET /repos/{o}/{r}/rules/branches/{branch}`, then `GET /repos/{o}/{r}/rulesets/{id}`:
  `current_user_can_bypass` is `never`; metadata read). And every `uses:` in the run's workflow file, and in every
  local workflow (`./.github/workflows/...`) or local action (`./path`, its `action.yml`) it pulls in, read at the
  run's own commit (contents read), must name a full commit id (`owner/repo@<40 hex>`), an image by digest
  (`docker://...@sha256:<64 hex>`), or a local path, and every image the job or an action runs (`container`,
  `services.*.image`, an action's `runs.image`) must be named by digest. A tag, a branch, an expression, another
  repository's reusable workflow (its own `uses:` are not read), a Dockerfile action, a file of several YAML
  documents or a file that is not there gets no ref. A re-run (`run_attempt` above 1, the broker's own re-runs of
  jobs that lost their box included) gets a ref only while its commit is still the branch's tip: once the branch moved
  on, it runs without its grants' tokens. The broker logs which check a run failed.
  If a grant names the run (`jobrule`: own repository, default branch, an event the grant lists, its workflow file),
  the box's `jit_guard` gains `run` (`docs/protocol.md`): the hook stops any job of another run, workflow or ref,
  and holds this one until the ready marker (300 s). Nothing is minted. A target whose driver cannot hold a job refuses the
  acquire, so such a job is never served on one. A job no grant names is served as before, its box holds nothing.
- **At `in_progress`** naming a job of that run on the box's runner (its own job, or another job of the run that
  took the runner), the broker asks GitHub again (GitHub must say that job is in progress on the box's runner, and
  the same answers must name
  the same run, workflow and ref), issues each matching grant's token through the issuer (its whole check again:
  config ceiling, trusted machine, the grant check with the job as subject), then writes the ready marker
  (`POTLUCK_READY`, the run id). It gives this 240 s from the job's start (GitHub's
  `started_at`, or the box's acquire if that is later), inside the hold: a webhook or lane running late leaves less,
  and none left mints nothing. A job of another run gets nothing: its guard
  stops it.
- **Fails closed**: GitHub not answering, a grant gone, an issue refused (two grants giving one name different
  permissions among them), the issuer not started within the 240 s, or the marker not written revokes what the box
  got and writes no marker; the hook stops the job before its first step when the hold ends. The broker records why (`runners_job_secrets`).
- **Revocation**: the `completed` of the job that held the box revokes its tokens at once; every end of the box (released, expired,
  lost, reclaimed) revokes them too, through the run targets' end hook. A job served again (a new box) gets a new
  record and the record of a box another job of its run still holds stays, so that job's `completed` still revokes it
  (its token is not renewed: it ends at its expiry). Records are forgotten 30 days after they last changed (the sweep).
- **Permissions**: the grant's ceiling permissions for the job's own repository, and the ceiling's `reason` as the
  request's reason (an elevated write needs one).
- **Preconditions the door cannot check**: classic branch protection is not read (it needs Administration read,
  which the App is not given): a branch guarded only that way counts as not reviewed, so protect it with a ruleset.
  GitHub tells the App only whether the App itself can bypass a ruleset, not who else is on its bypass list
  (repository admins, teams, deploy keys): an admin who bypasses, or turns the ruleset off for a moment, can still
  put unreviewed code on the branch's tip, so keep the bypass list empty. A local action is read at the run's
  commit, but the runner runs the copy in its workspace: a step that checks out another ref or repository before
  `uses: ./...` runs code this check never read. A full commit id may name a commit that exists only in a fork of
  the action's repository; the id alone does not say whose it is. What a pinned action pulls in when it runs (its own `uses:` if it is a composite action from another
  repository, anything it downloads) is not read: pin only actions whose own dependencies are pinned. A job of
  `workflow_dispatch` or `repository_dispatch` runs with inputs chosen by whoever dispatches.
- The broker's App needs `contents: read` for the compare, the tag lookup and the workflow files, and `metadata:
  read` (every App has it) for the rulesets.

## How runtarget implements this

`runtarget/`:

- Owner-only bindings, describe, acquire with the routing, class, local-only, allowlist, cap and budget rules above,
  status, release (with the final status), renew within the lifetime cap.
- Idempotent `request_id`, per actor: the daemon sends the driver `<actor tag>-<request_id>`; a repeat sends the
  commit the first resolved.
- Handle records in `run_handles`: spend never falls; reclaimed is never billed; a budget first refreshes the
  binding's live handles, so it may overshoot by what runs spend after acquire.
- A member reads a binding without its driver config or `create_in` (the owner's).
- A `scripts` list is enforced on every run binding that has one, not only on guarded ones.
- Bindings live in a project this machine holds (a solo project or the pool's).
- A deadline sweep (at start and every 30 s) asks status once a live handle is 5 s past `expires_at` (its final state
  and spend), and records it `lost` when its binding is gone or the driver cannot answer 15 minutes past the deadline.
- The sweep also renews a handle held by a live session (a third of its TTL left, at most once a minute, idle or not;
  never past the lifetime cap; not once the session ended; never one acquired with `self_renew`).
- An expired or reclaimed handle publishes the expiry event (above) and nothing else. The lease verbs refuse `db:`,
  `run:` and `box:` keys.
- The owner's bind with `setup` runs the driver's `setup` (a dry run, then the real one) before binding.
- Boxes: the bind and acquire rules above, the `exec` route for the holder, `images` pins sent to the driver as config
  `images` (comma-joined), a booted digest off the pin released at once.
- `core.Deps.OnRunTargetEnd` is called once per handle (the one write that ends its row, after its final status; each
  subscriber on its own goroutine) for the usage ledger, which reconciles a failed write from
  `core.Deps.RunTargetsEndedSince(project, since, limit)` (ended handles, oldest end first). A handle's `acquired` is
  when acquire answered.
- A handle records the area its acquire resolved (`area`; a pool team is an area): lists, status, release, renew and
  exec answer it only to callers who may act in that area, others as missing.
- Forwarding to a box host's replica, with the machine-class refusal: "Boxes on other machines" above.

### Tests of the daemon's side

The daemon's side is held by daemon tests, not by the conformance suite: routing (a non-head commit refused on a
trusted target, allowed with `allow_any_ref`; a caller-sent commit that differs `forbidden`; no ref or an unpushed one
`bad_request`; a local `main` ahead of the remote and a stale tracking ref both resolved from the remote), the
stricter class, roles, owner-only bindings, local-only acquire for db, credentialed and unisolated targets, the
`scripts` allowlist, caps, budget refusal and refused binds, reserved lease keys, the lease deadline equal to
`expires_at` with throttled renewal that goes on while the session is idle and stops when it ends, expiry with no
mail, a reclaim ending the lease and left out of budget, the final `status` after the end, the environment allowlist
with no inherit, secret values refused in a binding, a timed-out acquire never acquired afresh, and `url` kept out of
every place but the acquire answer.
