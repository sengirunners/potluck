# The secrets door: one way to move a secret into a box or a runner (design, 2026-11-10)

Bead zm-eakh.12.48. Status: accepted 2026-11-10 (owner rulings recorded in section 9); implemented since (grants,
issuers, job grants, sealing on legs whose machine can seal (every leg in `required` mode), box-scope secrets, and
zm's `task_start` taking `project`, section 6); `docs/daemon.md` is the current reference and this file is the
historical design, written against potluck e4a79f9.

Where this file and `docs/daemon.md` disagree, `docs/daemon.md` wins. The sections below describe the world as it
was at e4a79f9; the inventory in section 2 in particular is a dated record, and some of its paths have since been
moved through the door or deleted (see `docs/daemon.md`).

Written against potluck `main` e4a79f9 and the product spec. The `SPEC.md` and `API.md` citations refer to zm's
spec, a separate project's documents that are not in this repository; their line numbers are those of the spec at
the time of writing and have drifted.

Tags: **[V]** checked against the named source (file:line, a command, or a config read). **[A]** assumed; the
implementation bead named beside it must check it. The sections under "Design" were proposals when written; for what
shipped, see `docs/daemon.md`.

## Owner rulings this design carries out

The owner, 2026-11-10, ask 01M737KZ3VSCN12ADAJSPZJVFZ, asked for a design pass: build this into the product as a
general-purpose tool, with a standard way for people to securely transfer secrets into ephemeral VMs or runners,
instead of a dozen parallel systems and guards doing the same thing.

Rulings the owner made the same day, all kept:

| Ruling | Ask | Where it lands here |
|---|---|---|
| The per-job GitHub token is built into Potluck: at job start the broker mints a 1-hour, one-repo, permission-limited token and revokes it at the end (zm-eakh.12.7.3.4) | 01M737YHPHWPCNKEZ6V8A80KCS | The `github-token` kind (section 4.3) |
| A main-only Actions environment may replace a host job gate | 01M737KZ2VP1Q4EDKNAZB1E6Z4 | Section 4.9 and row 7 of the migration table |
| An agent login may enter boxes, in memory only, on trusted hosts only | 01M737KYZQR9TC30K1TANMCE0R | The `value` kind |
| A task's GitHub token may carry `workflows: write` only for tasks cleared for high-risk files | 01M737KZ0T24A7ZWSX7WGSTQM1 | A second grant (section 4.2) |

## 1. Summary

Today a secret reaches a box or a runner by one of seventeen paths (section 2). Four are sound and already share
code: renter secrets on a box `exec`, the sealed tunnel body, the sealed relayed op, and the broker's `jit_config`.
The rest are patterns a deployment grows around the product: client-side minters, stored long-lived values on hosts,
per-host hook allowlists, and repository secrets every runner can read. The door replaces them.

The design keeps the sound four and makes them one door with five parts:

1. **A grant** (policy, on the authority): who may put which kind of secret, with what ceiling, into which boxes.
2. **A kind** (an issuer): `value` (the caller holds it), or a credential the authority mints per box and
   downscopes (`github-token`, `potluck-token`, and the existing `github-jit`).
3. **One transport**: the run-target op, its secret-bearing arguments always sealed to the host that runs the box.
4. **One landing**: `0600` files in a `0700` directory on a memory filesystem, named by `POTLUCK_SECRETS`.
   Never an environment variable, an argument, a VM config or a disk.
5. **One lifecycle**: renewed by the authority before expiry, revoked at the issuer on every way a box can end,
   and written to the ledger without values.

A GitHub runner is a box [V: SPEC.md:3747], so a job's token is one grant whose subject is a job instead of a
person. Nothing in the door names a host, a team, a repo or an App: issuers and grants are config and data.

## 2. Inventory

Each entry: what moves, from where to where, who can mint it, lifetime and scope, how it is revoked, the guard.
"Path n" numbers follow the bead; 13 to 17 are paths this pass found. Systems outside the product appear only as patterns
(a client-side minter, a credentialed runner), never as a report on any deployment.

### Path 1. Renter secrets on a box `exec` or `spawn` [V]

- **What and where:** name/value pairs the caller holds, from its own machine to a file in the guest.
  `exec`'s `secrets`, names `[A-Z][A-Z0-9_]{0,63}`, 64 KiB in all (SPEC.md:3778-3806; potluck
  `runtarget/secrets.go:17,24-45,69`). The CLI reads them from `NAME=@file` or `$NAME`, never a value on the
  command line (`cli/sandbox.go:483`).
- **Landing:** the driver frames them ahead of the command's stdin; a Perl wrapper in the guest writes each to a
  `0600` file in a fresh `0700` directory under `/dev/shm`, sets `ZM_SECRETS` and `POTLUCK_SECRETS` to the path,
  and removes the directory when the command ends (`vmhost/exec.go:56-76`).
- **Who mints:** nobody; the value is whatever the caller has. zm keeps no store (SPEC.md:3804-3805).
- **Lifetime:** one command. **Revocation:** none (the door never knew what the value was).
- **Guard:** the host must be `trusted` and the driver must list `secrets` (`runtarget/secrets.go:35-43`,
  `runtarget/remote.go:129-133`; call sites `runtarget/runtarget.go:1131`, `runtarget/procs.go:179`). Refusals and
  driver errors are scrubbed of names and values (`runtarget/secrets.go:50-66`). The conformance suite checks the
  file modes, env, argv and removal (`conformance/box.go:310-391`).
- **Gaps:** any member who may `exec` may send any value (no policy beyond host class); nothing is recorded,
  by design (SPEC.md:3797); when the box keeps its disk the driver refuses, but a guest with no `/dev/shm` that
  does not keep its disk falls back to `TMPDIR`, on disk (`vmhost/exec.go:62-65` at e4a79f9); the secret lives for
  one command, so a long job needs a second copy somewhere (see path 4).

### Path 2. Sealed run-target bodies, member to home [V]

- A member joined over the Cloudflare Tunnel seals the whole body of a run-target call holding `secrets` or a
  `jit_config` to its home's certified X25519 key: HPKE base mode, `zs1.<expiry>.<blob>`, the info binding sender,
  recipient, expiry and the request (SPEC.md:2909-2934; `kit/tunnel/seal.go:45-59,113-160,279-312`). Both ends
  refuse such a body unsealed (`kit/tunnel/client.go:119`, `kit/tunnel/door.go:204`).
- **Key:** `$ZM_HOME/tunnel/seal`, `0600`, made on first use, certified by the daemon's ed25519 key, not escrowed.
- **Gap:** sealing happens only on tunnel legs. On the tailnet and the socket the same body goes as it is, so
  there are two code paths and the tested one is not the common one. It is hop by hop: the authority sees the
  plaintext (SPEC.md:2930-2931 says so).

### Path 2b. Sealed relayed ops, authority to host [V]

- The authority seals a relayed op's arguments to the key the host's poll or heartbeat certified, bound to the
  project, op id, binding, verb and box, for 10 minutes (`computeapi/runtarget.go:76-80`,
  `runtarget/relay.go:253-342`). Only the host opens them and hands them to its driver.

### Path 3. The runner broker's `jit_config` [V]

- **What:** GitHub's `encoded_jit_config`, a single-use runner registration, minted by the authority and sent as
  an `acquire` argument; the driver gives it to the guest's runner on stdin and stores nothing
  (`vmhost/driver.go:495-507`, `vmhost/store.go:37`, `vmhost/supervise.go:37,325`; SPEC.md:3747-3777).
- **Who mints:** the broker, with an installation token of the pool's App that names one repository and
  `administration: write`, `metadata: read` (`ghapp/ghapp.go:494-498`; tokens are cached per installation, repo
  and permission set and refreshed with under 5 minutes left, `ghapp/ghapp.go:57,303-339`).
- **Lifetime:** one job. **Revocation:** the broker deletes the runner on completion, takeover, a lost box and in
  a 5-minute sweep (SPEC.md:4056-4100; `ghapp/ghapp.go:565-571`).
- **Guards:** the webhook signature; a private repo; not a fork; a trusted event only
  (`runners/jobs.go:38`: push, pull_request, workflow_dispatch, schedule, merge_group, release,
  repository_dispatch; any other, `workflow_run` and `issues` included, is refused at `runners/jobs.go:548-558`,
  with `allow_fork_prs` off, the default);
  a `trusted` host only (`runtarget/runtarget.go:564`); and `jit_guard`, a job-started hook in the box that kills a
  job of another repo, event or fork before its first step (SPEC.md:3757-3777).
- **Gaps:** the guard checks repo and event, not the workflow file or the ref, so any trusted job of the repo with
  the pool's labels may take the box (SPEC.md:4084-4088). That is fine while the box holds no secret, and is the
  thing to fix before it holds one (section 4.5). The broker's installation tokens are never revoked: `ghapp` has
  no call to `DELETE /installation/token` at that commit; they lapse in an hour.

### Path 4. A client-side minter for sandbox tasks (pattern)

- **What:** a client that puts an agent login and a GitHub token into a leased box through path 1, minting the
  token itself from an App key it holds, re-minting on a timer and revoking on exit, is a client-side minter.
- **Guard:** such a client should pick only `host_class == "trusted"` targets that list `secrets`, on top of zm's
  own check.
- **Rule:** a client that mints its own token must hold an App key on every machine that mints, check GitHub's
  answer itself, run its own renewal loop (a path 1 secret dies with its command) and revoke by hand. The door
  replaces that: the key stays on the authority (threat 10) and the landing rule (section 4.6) applies to every
  secret a client sends.

### Path 5. Secret references in config [V]

- `potluck/kit/secrets` is the one reader: `file:PATH` (absolute, owner-only, at most 64 KiB, read on every use),
  `keychain:SERVICE[:ACCOUNT]`, `env:NAME`; values print as `[secret]` (SPEC.md:254-264; `kit/secrets/secrets.go`,
  `drivers.go`, `files.go`). Config holds references, never values, and adapters refuse inline secrets
  (SPEC.md:1447-1482).
- On the authority, the pool's GitHub App is configured under `runners.github` with `private_key` and
  `webhook_secret` as `file:` references into the authority's secrets directory. The App manifest flow wrote them;
  no person copied one (SPEC.md:4101-4108).
- **Verdict in advance:** this is how a daemon reads its *own* secrets. It is not a transport and stays as it is.
  The door's issuers read their signing keys through it.

### Path 6. A pool host's admission token [V]

- A bearer for the host's loopback-only admission endpoint; the driver reads its own `0600` copy named by
  `admission_token_file` (`vmhost/admission.go:21,86-98`; `hostprobe/admission.go:35,80`).
- Host-local, loopback only, never enters a box. Minted by the host's owner by hand; rotated by rewriting the file.

### Path 7. GitHub Actions secrets and credentialed runners (guidance)

- **Repository secrets:** GitHub puts a repository secret in the job message of any job that names it, on whatever
  runner takes the job. A secret a pull request must not read should be an environment secret with a main-only
  branch policy, never a plain repository secret.
- **A runner that holds standing logins** for GitHub or a tracker must not rely on a runner label as its guard, and
  a per-host job-started hook with a hard-coded allowlist is policy in the wrong place. The allowlist belongs in a
  grant on the authority, matched from GitHub's own answers (section 4.2); the credential is minted per job and
  dies with its box; and the events the hook accepts must be no wider than the broker's (path 3).

### Path 8. Standing credentials on a box host (guidance)

- A box host that keeps standing credentials (an identity token, tracker connection files, an agent login, an App
  key) has no lifetime but "until someone rotates it" and no revocation but by hand.
- **Rule:** an App key is the most powerful long-lived secret in a deployment, so it belongs only on the authority;
  box hosts should hold none.

### Path 9. A person hands a secret to an agent (a client's secret-request card) [V]

- The user answers a card; the value is kept in the client's secret store behind a one-use reference that expires
  in 24 hours, or is moved to an owner-only file for the agent's own shell. A scheduled task's webhook signature
  consumes such a reference.
- An earlier plan's decision D5: the client or the automations home reads its own reference and sends the value on
  exec stdin; no authority-side store.
- This is the entry point for a value a person holds. It ends at a local file, which is what path 1's
  `--secret NAME=@file` takes.

### Path 10. Identity tokens [V]

- `zm token mint --as NAME --role R [--project P [--area A]] [--ttl D]`: shown once, stored as sha256, scope is
  the identity's current grants capped by the token (SPEC.md:333-337). Events `token_minted` and `token_revoked`
  carry the id, never the secret (SPEC.md:1561).
- The `zm-runners` member token: minted per installation link, capped to the pool project and the link's area,
  only its id kept, revoked on unlink (`runners/links.go:264-306,366`; `runners/runners.go:41`).
- Join codes: one-time, 40+ bits, a TTL, at most 10 live per project (SPEC.md:2281-2283); tunnel invites carry 125
  bits (SPEC.md:2833-2834). An invite carries the redeeming machine's class (SPEC.md:797-805).
- Coordination-system tokens elsewhere follow the same shape: minted by an owner, scoped, no force.
- `potluck-e2b` API keys: a `0600` keys file, each line a key and optionally a zm token it acts as; a sandbox's
  data plane is authenticated by an HMAC per key and sandbox (`internal/e2bgateway/keys.go:20-51`, `envd.go:84`).

### Path 11. GitHub Apps

| App | Holds | Key lives | Used for | Source |
|---|---|---|---|---|
| The pool's App | administration write, actions and metadata read, members read | `file:` reference on the authority | the broker: JIT runners | [V] `ghapp/ghapp.go:636` (the App manifest); SPEC.md:4101-4104; bead zm-eakh.12.7.3.4 |
| zm GitHub link | per-project `zm-github` member token and webhook secret | the authority's secrets directory | PR linking and gates; not an App | [V] SPEC.md:3023-3037 |

### Path 12. Progress and notes out of a box with no token in it (guidance)

A sandbox task needs no coordination or tracker login in the box: the runner outside does the claim, the result mail
and the tracker comment, and notes can travel by a relay file the session appends to and the runner follows.

### Paths this pass added

- **Path 13. `potluck-e2b` environment variables [V].** An E2B SDK caller's `envVars` (sandbox) and `envs`
  (command) are held in the gateway's memory and sent as the command's argv, `env NAME=value ... cmd`
  (`internal/e2bgateway/gateway.go:63-68,175,208`; `envd.go:136,160,258-285`). SDK users put API keys there. The
  values are in the guest's process list and in whatever records an argv. No trusted-host check applies, because
  to zm it is an ordinary `exec`.
- **Path 14. The host-side runner, `runner=github` [V].** The older driver mode mints JIT configs on the host
  with a long-lived token in `github_token_file` (`vmhost/github.go:15-57`). The broker replaced it (plan D3) but
  the code and the config key remain.
- **Path 15. Host-local secrets [V].** A host's own secrets (like path 6) stay host-local. The rule is the landing
  rule of section 4.6: no secret is baked into a VM image, and a credential a job needs is delivered per job.
- **Path 16. Tracker and webhook secrets on the authority [V].** Tracker passwords in `$ZM_HOME/secrets/`, reaching
  only the adapter's environment (SPEC.md:2040-2049); webhook signing secrets; the VAPID key (SPEC.md:1608). All
  through path 5, none sent to a box.
- **Path 17. A device-pairing credential relayed through the authority [V].** The authority keeps a 5-minute
  pairing credential only while its requester waits, hands it out once and blanks it (SPEC.md:830-840). It moves
  a credential between machines, not into a box, but it is a second hand-built "hold in memory, hand out once".
- **Not a path, but used as one:** `sandbox cp`, `write_file` and `push` write to the box's disk and cross the
  tunnel readable (SPEC.md:3658). Nothing stops a caller copying a credentials file in that way.

## 3. What the whole must satisfy

1. **Policy is more than one bit.** "The host is trusted" is not enough. Who may send what, to which box, is written
   in one place (grants), so no client needs its own target filter, hook allowlist or environment gate.
2. **One minter per credential.** A one-repo GitHub token is minted in one place, on the authority. A client that
   holds an App key must not leave it on a box host.
3. **Lifetimes that match the work.** A secret that lasts one command (path 1) or until someone rotates it
   (paths 7, 8) is not enough: the door adds "this box" and "this job", so a client needs no hand-built renewal and
   a privileged job needs no long-lived login.
4. **Uniform revocation.** The authority revokes every credential it minted on every way a box can end, instead of
   leaving each client to do it; a host's own standing files are rotated by their owner.
5. **A record.** By design nothing notes that a secret was sent. That is right for values and wrong for
   credentials the authority minted: after an incident one must be able to say which box held which repo's token.
6. **No leaks at the edges.** A login token must not be placed in an environment that every child inherits, values
   must not travel in argv (path 13), files must not land on disk when a guest has no `/dev/shm` (path 1), and no
   secret is baked into an image.
7. **One sealing path.** Sealing must not depend on the transport (a clear path and a sealed path).

## 4. Design

### 4.1 The model in one picture

```
 caller (a person's client, a task dispatcher, the runner broker)
   |  "put <name> in box B": a value it holds, or a grant to mint from
   v
 AUTHORITY  -- grant check: subject x kind x ceiling x box's project/area x host class
   |          mint (github-token, potluck-token, github-jit), start renewal, write the ledger (no values)
   |  one transport: the run-target op, secret arguments sealed (HPKE) to the host's certified key
   v
 HOST (trusted) -- opens the seal, hands the driver the bytes on a pipe; stores nothing
   |  the driver's exec channel (never argv, env, VM config)
   v
 GUEST -- $POTLUCK_SECRETS/<NAME>, 0600 in a 0700 directory on a memory filesystem
           replaced in place on renewal; gone with the box; revoked at the issuer when the box ends
```

There is no pull. A box reaches the internet and nothing private, so a guest never calls the authority for a secret
and the authority exposes no secret route to guests.

### 4.2 Grants (policy)

A grant is a row on the authority, in the team project that owns the boxes. Only an admin of the area may write
one. It says who may use it (an identity, a role of the area, or a GitHub job the broker serves, named by
repository, workflow, ref and events), which kind, the file name in the box (`[A-Z][A-Z0-9_]{0,63}` as today), the
ceiling a request may ask for, the host class (`trusted`; a public host receives nothing from the door in v1, see
4.8) and the scope (`exec`, `box`, or both; see 4.6).

The shipped shape differs from the field table this section first proposed; the code is the reference:
`grants/doc.go`, and `docs/daemon.md` ("Secret grants", "Job grants").

A request names a grant and the actual parameters, which must sit inside the ceiling: one repository, a
permission map no wider than the ceiling's. The authority downscopes at mint; the box never sees the ceiling.

Rules:

- **Default deny.** No grant, no secret. A built-in default keeps today's behaviour: a member of an area may send
  `value` secrets on `exec` to a trusted box they hold (path 1 unchanged for existing clients). See
  `docs/daemon.md` for the defaults as shipped.
- **A job grant is matched by the authority from GitHub's own answers** (the run's repository, workflow path,
  head branch and event), never from anything the box or the workflow file says. A pull request cannot widen its
  own grant by editing a workflow, because the grant lives on the authority and names `refs/heads/main`.
- **A repository is in reach only through a linked installation.** A `github-token` grant in an area can name
  only repositories of installations linked to that area; the link already proves an org admin agreed
  (SPEC.md:4016-4036).
- **Elevated permissions are a second grant with a narrower subject.** "Workflows write only for cleared tasks" is
  two rows: the ordinary grant (contents, pull requests) for a dispatcher's identity, and one whose ceiling adds
  `workflows: write` and whose subject is the dispatching client's identity alone. That client asks for it only
  for a task its dispatcher cleared, and must send a `reason` (free text, recorded in the ledger: the task id and
  who cleared it). The door enforces who and how much; the client, which already reads the clearance,
  enforces when.

### 4.3 Kinds (issuers)

Preference order: a minted, short-lived, downscoped credential over a stored value, always.

| Kind | Minted by | Scope | Lifetime | Renewed | Revoked |
|---|---|---|---|---|---|
| `github-jit` (exists) | the authority, from the pool's App | one repository's runner registration | one job | no (single use) | runner deleted at job end, takeover, lost box, sweep |
| `github-token` (zm-eakh.12.7.3.4) | the authority, from an App named in config | one repository, the requested permissions only | GitHub's 1 hour | at 40 minutes while the box lives | `DELETE /installation/token` at box end; the old token 5 minutes after each renewal |
| `potluck-token` | the authority's own token mint | one role in one project or area | the box's lease, at most | with the lease | `token revoke` at box end |
| `value` | nobody: the caller holds it | whatever it is | the caller's business | the caller sends it again | the door cannot; the caller must |

- `github-token` is `ghapp.InstallationToken` (`ghapp/ghapp.go:306`) with the request's repository and
  permissions, plus what a client-side minter typically does and the broker did not: check that GitHub's answer
  names exactly that one repository and those permissions, and revoke and refuse otherwise. Tokens for boxes are
  never taken from the broker's cache: each box gets its own, so revoking one ends one box.
- The App is config: `secrets.issuers.<name>` is `{"kind": "github-app", ...}` with the same reference fields as
  `runners.github`, or `{"use": "runners.github"}`. A team may run one App for runners and tokens or two.
- `potluck-token` is the answer to "a tracker login for CI" (the question the owner's design pass answered): no
  long-lived login anywhere; a job that must comment on a work item gets a member token capped to its project that
  dies with its box. Reaching the authority from a box needs a client that signs tunnel requests with a key file
  and no daemon ([A]; bead 9). The kind delivers the token and a per-box key together.
- `github-jit` keeps its landing (the runner's stdin, consumed once) but moves under the same grant check, sealing
  and ledger as the rest. It is the proof that the model fits: it is already minted per job on the authority,
  sealed, trusted-host only and cleaned up.
- **No authority-side store in v1.** Plan D5 ruled it out and ask 01M738V26QEFB6ZZBFQDWXMVP6 confirmed it ("no store
  for now"): a `value` comes from the caller on each call. A stored, long-lived value that no caller holds at job
  time (a deploy key for a CI job) is the one case the door does not cover; section 4.9 says where those stay. A
  reference kind (a name resolved to a stored value at delivery) is a store by another name and would need a new
  ruling (section 9).
- The authority's in-memory copy of a minted token, kept so it can revoke the token at box end (4.7), is a handle,
  not a store under the "no store" ruling: it is never on disk, never served back to anyone, and lapses within the
  hour.

### 4.4 Transport

One: the run-target op that already carries `exec`, `spawn` and `acquire`.

- **Always sealed.** Secret-bearing arguments are sealed to the receiving daemon's certified key on every leg,
  tailnet and socket included, with the existing suite and info binding (path 2, 2b). One code path; the transport
  under it stops mattering. A peer that certifies no key gets the refusal it gets today.
- **End to end where the authority has no need to read.** For minted kinds the authority is the source, so
  authority-to-host sealing is end to end. For `value`, the client seals to the *host's* key (the authority
  passes the host's certificate along, signed by the host's own node key, which a member can pin) and the
  authority relays a blob it cannot open. Until that ships (bead 8), `value` stays hop by hop as it is today and
  the document says so plainly: the authority is trusted with values in memory for one call.
- **The last hop is the host's.** Host to guest is the hypervisor's exec channel on a pipe. The guest has no key;
  "trusted host" means trusting that host's root (SPEC.md:3805-3806). The seal ends at the host.

### 4.5 Runners use the same door

A runner box differs from a plain box in one way: the party asking is the broker, and it must not hand a job's
secret to a different job. GitHub gives a repository's runner to any job of that repository with matching labels
(SPEC.md:4084-4088), so a token minted for a privileged job's box could be read by a pull request job that took the box
first. Two independent checks close that:

1. **Mint after GitHub says who is on the box.** At `queued` the broker only acquires the box with its
   `jit_config`, as today, and records which grants the job matches. It mints nothing. When `workflow_job`
   `in_progress` arrives naming this job on this box's runner (the check at `runners/jobs.go:576` and `:1096`
   already does this), the broker mints and delivers.
2. **The guard holds the job until then, and kills any other.** `jit_guard` gains `run_id`, `workflow_ref` and
   `ref`. The job-started hook, which runs before the job's first step and which the runner waits for
   (SPEC.md:3766-3767), kills the job as it does today unless `GITHUB_RUN_ID`, `GITHUB_WORKFLOW_REF` and
   `GITHUB_REF` match; when they match and the box expects secrets, it waits (bounded, default 120 s) for the
   door's `ready` marker in the secrets directory, then lets the job start. No marker: the job fails closed with
   a message naming the door.

So the token exists only once the right job holds the box, and a wrong job never runs a step in a box that holds
one. A job with no matching grant is served exactly as today and its box holds nothing.

A late or lost `in_progress` webhook fails the job closed: the broker mints nothing, no `ready` marker appears, and
the guard's wait ends with the job failed and a message naming the door. Whether the broker's poll fallback
(`runners/jobs.go:1096`) can stand in for a lost webhook is for bead 5 to check; see `docs/daemon.md` for what
shipped.

[A] bead 5 checks: that `GET /actions/runs/{id}` gives the workflow `path` and `head_branch` the match needs
(`ghapp.Run` reads neither today, `ghapp/ghapp.go:402-408`), that a tag or branch named like the default branch
cannot pass both the authority's and the hook's check, and that a runner started with `POTLUCK_SECRETS` in its
environment passes it to steps.

The broker's event rule does not change: `workflow_run` and `issues` runs stay refused (with `allow_fork_prs` off,
the default). Jobs that fire on them use a dispatch from an unprivileged job.

### 4.6 Landing in the guest

- **One place:** a `0700` directory on a memory filesystem holding one `0600` file per name, its path in
  `POTLUCK_SECRETS` (and `ZM_SECRETS` until the rename ends). The door never sets a value in an environment,
  never passes one in argv, never writes one to VM config, a record, a log, or a disk.
- **Two lifetimes, one layout.** `exec` scope is today's: a fresh directory for one command. `box` scope is new:
  a directory at a fixed path (`/dev/shm/potluck-secrets`) for the box's life, written by a new box verb
  (`secrets`: set and unset, driver capability `box-secrets`), visible to every later `exec`, `spawn`, PTY and to
  the runner. A command with its own `exec` secrets gets its own directory as today; the box directory stays at
  its fixed path.
- **Renewal is a rename.** A new value is written beside the old and renamed over it, so a reader never sees half
  a file. Readers must read the file at each use: a git credential helper or a `gh` wrapper that `cat`s the file.
- **No memory filesystem, no secrets.** Today's fallback to `TMPDIR` goes. A guest without `/dev/shm` (a macOS
  Tart guest) refuses, as a kept-disk box already does, until its driver makes a RAM disk (bead 3).
- **One box is one trust domain** (SPEC.md:3787-3789). Everything in the box as that user can read the
  directory. The door bounds what a box holds; it does not separate processes inside it.
- What a job does with the file is the job's. The guidance in the docs: use a helper that reads the file, do not
  `export` it. Existing clients are moved onto the door (rows 4 and 13).

### 4.7 Renewal, revocation and the ledger

- **Renewal:** the authority keeps a timer per box and name for each minted secret. At two thirds of the
  lifetime it mints again, delivers, and revokes the old one after 5 minutes. If it cannot deliver, the old
  credential simply expires: the failure is closed.
- **Revocation:** every way a box ends (released, expired, lost, reclaimed, its host demoted to `public`, its job
  completed or taken over) revokes every credential minted for it, at the issuer, and the memory filesystem dies
  with the box. A sweep retries until the issuer confirms. An admin can revoke by box or by grant; deleting a
  grant revokes what it issued.
- **What the authority holds:** a minted GitHub token in memory, because GitHub's revoke call authenticates with
  the token itself. Never on disk. After an authority restart the tokens cannot be revoked and lapse within the
  hour; the ledger records that as `expired_unrevoked`.
- **Ledger** (events, like `token_minted`): `secret.granted`, `secret.delivered`, `secret.renewed`,
  `secret.revoked`, `secret.refused`. Each carries who asked, the grant, the kind, the name, the box, the host and
  its class, the parameters (repository, permissions, role), the expiry, the `reason` if given, and for a minted
  credential the digest of the token: the base64 encoding of its SHA-256, the form GitHub's audit log shows as
  `hashed_token`, so an incident can be matched across both (a hex digest would not match). Never a value. For
  `value`, the names and sizes only: this is a change from "records nothing" (SPEC.md:3797, plan D5), made because
  a name is policy data, and it is called out for the reviewer.

### 4.8 Trusted and public hosts

| | Trusted host | Public host |
|---|---|---|
| Who it is | a machine the project's owner or members run; you trust its root | a lender's or a stranger's |
| `value` | yes, under a grant | never (`403`, naming no secret) |
| Minted kinds, `github-jit` included | yes | never in v1 |
| Placement | a box that will receive a grant is placed only on trusted hosts | secret-free work only |
| Demotion | revokes everything minted for boxes on that host and stops renewals | - |

The class is the authority's decision, never the machine's own (SPEC.md:792-805). The door asks it at every
delivery and every renewal, not only at acquire. SPEC.md:4148-4150 leaves room for lease-bound credentials on
public hosts; this design does not use that room until there is a need and a threat review of it.

### 4.9 What the door does not carry

- **A daemon's own secrets** (paths 5, 6, 15, 16): read by reference on the machine that uses them.
- **GitHub Actions secrets.** GitHub delivers them inside the job message; Potluck cannot change that channel.
  The rule for a repository whose jobs run on pool boxes: a secret a pull request must not read is an
  *environment* secret with a main-only branch policy, never a repository secret (ruling
  01M737KZ2VP1Q4EDKNAZB1E6Z4). Anything the door can mint (a GitHub token, a tracker token) is not stored there
  at all.
- **Files** (`cp`, `write_file`, `push`): not a secret channel; the docs say so, and the CLI warns on a path that
  looks like a credentials file (bead 10).

### 4.10 Threats

| # | Threat | Closed by | Left open |
|---|---|---|---|
| 1 | A host operator reads a guest's secrets | Nothing goes to a public host; a box that needs a grant is placed on trusted hosts only; class asked again at each delivery | A trusted host's root can read everything. That is the definition |
| 2 | The network path reads or alters a secret (Cloudflare, a relay) | HPKE sealing on every leg, the info binding sender, recipient, op, box and expiry | The authority itself reads `value` in memory until client-to-host sealing ships (bead 8): see threat 11 |
| 3 | A sealed blob is replayed or sent to another box | The same binding; 10-minute expiry; one op id | - |
| 4 | A job other than the intended one takes the runner box | Mint only after `in_progress` names the job; the guard matches run id, workflow file and ref and kills anything else before a step runs | A step of the *intended* job can read its own grant: that is the grant |
| 5 | A pull request widens its own access by editing a workflow | Grants live on the authority and name `refs/heads/main`; the match uses GitHub's answers | A change merged to main: that is code review's job |
| 6 | Code in the box exfiltrates the token (a prompt-injected agent, a bad dependency) | Cannot be prevented. Bounded: one repository, the fewest permissions, an hour, revoked at box end, no `workflows` by default; the ledger's token hash finds its uses in GitHub's audit log | The window between theft and box end |
| 7 | A member asks for a token on a repository that is not theirs | A grant names only repositories of installations linked to its area; the request must sit inside the ceiling | - |
| 8 | A credential outlives its box (crash, lost host, reclaim) | Revocation on every end state, a retrying sweep, an hour's expiry as the backstop, tokens capped to the lease | An authority restart loses the handle to revoke: up to an hour |
| 9 | A value leaks through env, argv, logs or disk | The landing rule; the conformance checks that exist, extended to box scope; no `TMPDIR` fallback; clients moved onto the door, E2B included | Memory can be swapped: a host reports its swap in its advert, `potluck doctor` fails on plain disk swap, and the authority gives box secrets only to a host with safe swap (off, zram or encrypted; `docs/hosts.md`) |
| 10 | A long-lived key on a weak machine is stolen | App keys only on the authority; box hosts hold none; host-side `github_token_file` retired | The authority is the one place to protect |
| 11 | The authority is compromised | It can mint within every App's permissions. Keep the App's own permission set small; one repository and named permissions per token; the ledger; `value` sealed past it (bead 8) | Not closed. It is the root of trust |
| 12 | Mail or a note asks for a secret | A request is an authenticated API call checked against a grant. Mail is data and grants nothing | - |
| 13 | Renewal fails mid-job, or the `in_progress` webhook is late or lost | The credential expires, or is never minted; the job fails closed | - |

## 5. Every path: kept as a client of the door, or deleted

| # | Path | Verdict | Migration step |
|---|---|---|---|
| 1 | Renter secrets on `exec` | **Is the door** (`value`, `exec` scope) | Add grants with the default that preserves today's behaviour; add `box` scope; remove the `TMPDIR` fallback; ledger names |
| 2, 2b | Sealed bodies and ops | **Is the door's transport** | Seal on every leg, not only the tunnel |
| 3 | Broker `jit_config` | **Kept as a kind** (`github-jit`) | Same grant check, sealing and ledger; revoke the broker's own installation tokens |
| 4 | A client-side minter | **Client.** Its minting, re-mint loop and revoke are **deleted** | The runner asks the door for `GH_TOKEN` (box scope) under the client's grant and sends the agent login as a `value` (box scope); the session reads the login from the file at each use. Then the client-side mint, revoke and its re-mint loop go |
| 5 | Config references | **Kept, not a client** | Issuers read their keys through it. Nothing to move |
| 6 | Admission token | **Kept, not a client** (host-local, loopback) | None |
| 7 | Actions repository secrets | **Kept, GitHub's channel**, narrowed | The application moves what a PR must not read into a main-only environment |
| 7, 8 | Standing credentials on runners or box hosts (a runner guarded only by a label, a per-host hook allowlist, an App key on a box host) | **Not needed** | Run such jobs on pool boxes under a job grant (`GH_TOKEN`) and a `potluck-token`; App keys stay on the authority |
| 9 | A client's secret-request card | **Kept as the human entry.** A client | Its file delivery feeds `--secret NAME=@file`; a consumer of its reference for sandbox calls is a client follow-up, not a Potluck change |
| 10 | Identity tokens, join codes | **Kept.** The mint becomes the `potluck-token` issuer | `zm-runners` link tokens unchanged |
| 10 | `potluck-e2b` keys file | **Kept, not a client** (gateway-local) | None |
| 11 | The pool's App | **Kept: the pool's issuer** | Gains the permissions grants need (section 9, question 2, answered: one App) |
| 12 | Progress and notes out of a box | **Guidance, not a channel** | None |
| 13 | E2B `envVars` in argv | **Client** | The gateway sends them as `value` secrets and the command reads them through a wrapper that sets them from files inside the guest; refused on a public host like any secret (a behaviour change for E2B users, documented) |
| 14 | Host-side `runner=github` | **Deleted** | Remove the mode and `github_token_file` once no binding uses it (pool bindings are broker-served) |
| 15 | Host-local secrets | **Kept, host-local** | None |
| 16 | Tracker, webhook, VAPID secrets | **Kept, not a client** | None |
| 17 | Device-pairing relay | **Kept**; shares the "hold while waited for, hand out once" helper with the door | A refactor, last |

## 6. The coordinator gap (shipped in zm: `task_start` takes `project`)

Status: fixed. zm's `task_start` now takes `project`, as proposed below; see zm's own documentation for the shipped
behaviour. What follows is the design as written.

A session that is not in a team project cannot place work on a pool machine: `task_start` with a `machine` answers
"machine choice needs a team project: the parent session is in <project>" (zm's remote task code [V]). The project is taken from the parent session and nothing else, so a coordinator in a
personal or automations project has no way to say "place this in the pool".

The CLI does not have the gap: `zm -P <team project> sandbox create` names the project, and that is how a task runner names it.

Fix, in zm, with nothing named: `task_start` (API and MCP) takes an optional `project`, a team project the
*caller's identity* owns or administers. The who-may rule is the one SPEC.md:757-759 already states, checked
against that project instead of the parent's. The parent stays the caller's session, wherever it lives, so
status, cancel and the ending mail work as they do now. Without `project`, behaviour is unchanged. The same
field is how such a coordinator's task comes under that project's grants, so the door needs no special case for
it. Bead 11; [A] it must check how a parent address from another project is carried.

## 7. Rejected alternatives

- **A secret store on the authority** (named values, resolved at delivery). It turns the authority into a vault
  that must be backed up, rotated and audited, and plan D5 already said no. Minted kinds remove most of the need.
- **Pull by the guest with a workload identity** (the box presents a signed identity and fetches its secrets, as
  cloud metadata services do). It needs every box to reach the authority and the authority to serve guests. Push
  over the channel that already exists needs neither. A signed box identity for third parties (cloud role
  exchange) can be a later kind.
- **Minting in the job** from an App key held in an Actions environment. It works, but it puts an App private key
  in every job that needs a token and makes one App per purpose. Ruled against on 2026-11-10.
- **Environment variables as the landing.** Inherited by every child, printed by `env`, kept in crash reports.
- **Serving `workflow_run` in the broker for allowlisted workflows.** It would widen what the broker trusts for
  every pool to save one small dispatch job.

## 8. Implementation beads, in order

1. **potluck: grants.** The table, its API and CLI, the default grant that preserves path 1, checks on `exec`
   and `spawn`. Ledger events for `value` (names only). SPEC 20 amended.
2. **potluck: seal on every leg.** One path for tailnet, socket and tunnel; conformance for the refusal.
3. **potluck: box-scope secrets.** The `secrets` box verb, capability `box-secrets`, the fixed directory, atomic
   replace, removal with the box; no `TMPDIR` fallback; a RAM disk for macOS guests or a clean refusal.
   Conformance checks with a broken reference driver for each.
4. **potluck: the `github-token` issuer** (zm-eakh.12.7.3.4). Config `secrets.issuers`; mint per box with the
   answer check; renewal at two thirds; revoke on every end state and a sweep; `ghapp` gains revoke and uses it
   for the broker's own tokens too.
5. **potluck: job grants in the broker.** Match on GitHub's run answers; mint on `in_progress`; `jit_guard`
   gains run id, workflow ref and ref, and waits for the `ready` marker. The [A] checks of section 4.5.
6. **Clients that mint their own tokens become clients of the door.** Ask the door for `GH_TOKEN`; agent login as a
   box-scope `value`; the session reads it from the file; delete the client-side minting code. The elevated grant
   and its `reason`.
7. **Credentialed jobs on pool boxes:** job grants for privileged jobs; a main-only environment for what stays an
   Actions secret.
8. **potluck: client-to-host sealing for `value`**, so the authority relays what it cannot read.
9. **potluck: the `potluck-token` issuer and a daemon-less tunnel client** for a box to reach its authority.
10. **potluck: the edges.** `potluck-e2b` env values through the door; remove host-side `runner=github`; a CLI
    warning for credential-looking files in `cp`; a `potluck doctor` check on unencrypted swap, and the authority refusing secrets to such a host.
11. **zm: `task_start` takes `project`** (section 6).
12. **Retire:** the host-side `runner=github` mode and client-side minting in clients that move to the door. Share
    the hand-out-once helper with the pairing relay.

Beads 1 to 5 are Potluck's and unblock 6 and 7. Bead 11 is independent and can go first.

## 9. Questions only an owner can answer (answered 2026-11-10)

1. **Should the door ever hold stored values on the authority?** Answered, ask 01M738V26QEFB6ZZBFQDWXMVP6: "No
   store for now." Minted tokens cover GitHub and the tracker, a person's or a client's own values come with
   the call, and deploy keys for CI stay in a main-only Actions environment. A reference kind (a name resolved to a
   stored value at delivery) is a store by another name and would need a new ruling. The authority's in-memory
   minted token, kept for revocation (4.7), is a handle and not a store under this ruling.
2. **May the pool's App be given the permissions the job token needs** (contents and pull requests write;
   workflows write for cleared tasks; actions, checks and statuses read), or should tokens come from a second App?
   Answered, ask 01M738V27NJ9BNZJZWS4F7P61R: "One App." Every token is cut down to one repository and the named
   permissions, and both keys would sit on the same machine anyway. An org owner must approve the change on
   GitHub.
