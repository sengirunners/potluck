# Potluck driver protocol (potluck/1)

A **driver** is a program that lends one kind of resource for a while. Potluck's daemon starts it once per call,
writes one JSON request to its stdin and reads one JSON answer from its stdout. This document is everything you
need to write one. The wire types are in `protocol/` and `computeapi/runtarget.go`; the daemon's side of each call
is in `runtarget/`.

The other guides: `docs/conformance.md` (how to check a driver), `docs/hosts.md` (sharing a machine),
`docs/network.md` (box networking), `docs/daemon.md` (what the daemon does around a driver), and
[`docs/vm-layers.md`](vm-layers.md) (a new hypervisor for the existing VM driver, rather than a new driver).

## Overview

### What a driver lends

A driver serves one **binding**: a named entry the machine's owner made, which says which driver to run, which
kind it lends and the driver's own config keys. One driver program may serve several bindings, of several kinds.

| kind | what it lends | example |
| --- | --- | --- |
| `db` | a throwaway database | a Postgres template copy |
| `run` | one run of a named script, at an exact commit, somewhere other than the caller's machine | `potluck-driver-local` |
| `box` | a machine (VM or sandboxed container) kept up for its lease, which its holder sends commands with `exec` | `potluck-driver-tart`; a Potluck sandbox |

Each resource the driver makes has a **handle** (`box:sandbox/b1`) and a deadline, `expires_at`. The driver ends
the resource at that deadline itself, whether or not the daemon is up.

### How a driver is found and started

- The daemon looks on its `PATH` for `potluck-driver-<driver>`, then `zm-run-<driver>` (`protocol.LookDriver`,
  `runtarget.LookPath`). `<driver>` is the binding's `driver` key.
- Each call is a new process, in its own process group, with the working directory set to the system temporary
  directory.
- Its environment holds only `PATH`, `HOME`, `LANG`, `TERM` and `TZ` from the daemon's environment, and
  `POTLUCK_PREFIX`: how a person types Potluck's commands on this machine (`potluck`, or a program's words such as
  `supper pool`), for a hint in a refusal's message (`potluck doctor`). Anything else the driver needs comes from its
  config.
- It reads the whole of stdin (one JSON object), does the work, writes one JSON answer to stdout and exits 0.
- Human text goes to stderr. The daemon throws stderr away; it never reaches logs, events or answers.
- If the call outlives its time limit (below), fails (non-zero exit, a child holding stdout or stderr open) or its
  answer is refused (see "The answer"), the daemon kills the whole process group. Only a process that left the group
  (`setsid`) survives.
- A driver has no long-lived connection to the daemon. A resource that must outlive the call (a running script, a
  booted VM, the deadline timer) needs a process the driver leaves running outside its process group, such as a detached supervisor (`setsid`).
- Between calls the daemon keeps only what the answers told it (the handle, `expires_at`, state, spend). Keep your
  own records in a directory of your own, never the working directory. The convention (`protocol.DriverStateDir`) is
  `~/.potluck-driver-<name>` under the `HOME` the driver gets; `potluck-driver-local` and `potluck-driver-tart` also
  take a `state_dir` config key. One directory may serve several bindings: key records by config `target`.

### The request

```json
{"potluck": "potluck/1", "zm": "zm/v0", "verb": "exec", "id": "box:sandbox/b1",
 "args": {"argv": ["uname", "-a"], "timeout_s": 30},
 "config": {"target": "sandbox", "kind": "box", "images": "ref-image@sha256:...", "size": "c2m4"}}
```

| key | always | meaning |
| --- | --- | --- |
| `potluck` | yes | the protocol, `"potluck/1"` |
| `zm` | yes | the same protocol under its old key, `"zm/v0"` (see [Names](#names)) |
| `verb` | yes | the operation |
| `id` | no | the handle, for every verb that acts on one resource |
| `args` | no | the operation's arguments. The daemon leaves it out when there are none; the conformance suite sends `{}`. Accept both. |
| `config` | yes | the binding's driver config (strings only), plus `target` (the binding's name) and `kind`; for a box binding with pinned images, also `images`: the pins joined with commas |

### The answer

One JSON value on stdout, usually one line:

```json
{"ok": true, "result": {...}}
{"ok": false, "error": {"code": "not_found", "message": "no box:sandbox/b9"}}
```

- Exit 0 after writing it, also for a refusal. Nothing the driver starts may keep its stdout or stderr open after it exits.
- Write exactly one JSON value; blank space around it is fine. Human text goes to stderr.
- `ok: true` carries `result` (`null` is a result); `ok: false` carries `error` with `code` (one of the codes
  below) and `message`.
- The daemon and the conformance suite read an answer by the same rule (`protocol.ReadAnswer`): an answer that
  breaks any of the three rules above is refused (`internal` to the daemon's caller; a failed check in the suite),
  even when it holds a good answer.
- An answer over 16 MiB is refused by both (`internal` to the daemon's caller).
- `error.message` is shown to the caller cut to 300 characters on one line. Never put a secret in it.
- Stdin that is not JSON, or not a request, still gets an answer: `ok: false` with one of the codes below
  (`bad_request` fits), exit 0. The suite sends `this is not json` and checks for exactly that.

### Error codes

| code | when |
| --- | --- |
| `bad_request` | the request is malformed: bad arguments, a malformed handle, a handle of another binding, a value past a limit |
| `forbidden` | the request is well formed but not allowed: a database server not in `create_in`, a commit the ref does not hold |
| `not_found` | a handle this binding never issued, or a process id it does not know |
| `conflict` | the request clashes with the resource's state: `exec` on a box that is not `ready`, a name taken under another `request_id` |
| `not_implemented` | a verb this driver does not serve |
| `unavailable` | the driver cannot do it now: no capacity, the owner's reclaim policy says no, a dependency is down |
| `internal` | anything else |

The daemon turns any other code into `internal`.

### Time limits

The daemon waits this long for an answer, then kills the driver's process group (`runtarget/driver.go`,
`runtarget/rt/rt.go`).

| verbs | limit |
| --- | --- |
| `describe`, `status`, `release`, `list`, `renew`, `spawn`, `signal`, `stop`, `secrets`, `progress`, `cancel` | 30 s |
| `acquire`, `setup`, `start` | 120 s |
| `exec` | its `timeout_s` + 30 s |
| `output` | its `wait_s` + 10 s, at most 30 s |
| `input` | 15 s |

`acquire` answers once the resource is usable: the database exists, the run has started (never when it ends), the
box answers `exec`.

### Versioning

The protocol is `potluck/1`. A reader treats `potluck/1` and `zm/v0` as the same major version
(`protocol.SameProtocol`). `describe` answers with `"potluck": "potluck/1"`, `"zm": "zm/v0"`, or both. The
conformance suite fails a describe with neither key, or with a value of another major. The daemon itself does not
check the value today.

## Names

Potluck was renamed from zm in two steps. Readers take both names (step 1); senders and creators now use the
Potluck names (step 2), and the old names stay accepted (`kit/api/names.go`, `protocol/`, `kit/authz/token.go`).

| Potluck name (sent and created) | old name (still read) |
| --- | --- |
| wire value `potluck/1` under the key `"potluck"` in requests and in `describe` (the daemon also sends `"zm": "zm/v0"`) | `zm/v0` under the key `"zm"`, the same major: a driver that answers only that key passes |
| driver program `potluck-driver-<driver>` (looked for first) | `zm-run-<driver>` |
| `POTLUCK_SECRETS` in a box (a driver sets both) | `ZM_SECRETS` |
| `POTLUCK_CONFORMANCE_URL` for the db probe (both are set) | `ZM_CONFORMANCE_URL` |
| state directory `~/.potluck-driver-<driver>` on a machine that has neither | `~/.zm-run-<driver>`, kept where it exists |
| daemon HTTP headers `Potluck-*`, bearer tokens `potluck_...`, new tailnet nodes `potluck-<machine>` | `Zm-*`, `zm_...`, existing nodes `zm-<machine>` (they keep their names) |
| box labels `dev.potluck.run-target`, `dev.potluck.lower`, Incus `user.potluck.run-target`; VM names `potluck-<hash>-` | `dev.zm.*`, `user.zm.*`, `zmrt-<hash>-` (swept and counted as before) |
| named volumes labelled `dev.potluck.volume=1` | `dev.zm.volume=1` is not read |
| filtered docker network `potluck-box` on bridge `br-potluckbox`, kept by `potluck-box-net.service` (`docs/network.md`) | `zm-box`, `br-zmbox`, `zm-box-net.service`: read, with steps and caps under them, on a machine that has only those; never made beside live ones |
| a box's push directories `$HOME/.potluck/push` and `$HOME/.potluck/push-git` | `$HOME/.zm/push-git/<key>`: a checkout's git directory made there is kept in use; `$HOME/.zm/push` chunks are not read (a resumed push sends them again) |
| GitHub runners named `potluck-<job id>[-<n>]` | `zm-<job id>[-<n>]`, still cleaned up and adopted |

The programs also exist under their old names (`cmd/zm-run-tart`, `cmd/zm-run-local`, `cmd/zm-run-conformance`,
`cmd/zm-e2b`) for build lists and updaters that still name them.

## Operations

Required for every kind: `describe`, `acquire`, `status`, `release`, `list`. A box also requires `exec`. The daemon
refuses a binding whose `describe` lacks one (`runtarget.describeOf`). Everything else is optional: list it in
`describe.verbs` when you serve it, and answer `not_implemented` for any verb you do not serve.

### Who calls what

| verb | required | `id` | the daemon calls it |
| --- | --- | --- | --- |
| `describe` | yes | - | often: at bind, when targets are listed, before `acquire`, `renew` and `start`, before an `exec` or `spawn` with secrets, encoding or a pty, before `secrets`, after `release` and a `status` with spend, and for a shared machine's adverts |
| `acquire` | yes | - | when a caller acquires; once more with the same `request_id` if the first call timed out |
| `status` | yes | handle | when a caller asks; once after `release`; at the deadline (the sweep, every 30 s) |
| `release` | yes | handle | when the holder (or the machine's owner) ends it; when the booted image or network is not what was asked; by the sweep |
| `list` | yes | - | after an `acquire` timed out twice, to find the handle by its `request_id` |
| `progress`, `cancel` | no, together, with capability `acquire-progress` | - | when a caller asks how far its acquire got, or cancels it, by `request_id` |
| `renew` | no | handle | when the holder extends the lease; by the sweep for a lease held by a live session |
| `setup` | no | - | only on the owner's bind with setup: `dry_run` true, then false. Never on `acquire`. |
| `exec` | box | handle | when the holder runs a command |
| `spawn`, `output`, `signal` | no, together | handle | background processes |
| `input` | no, with capability `pty` | handle | terminals |
| `stop`, `start` | no, with capability `persist` | handle | kept disks |
| `secrets` | no, with capability `box-secrets` | handle | when the holder (or, renewing a credential, the authority) sets or unsets the box's secrets |

### Fields

`?` marks an optional field.

| verb | `args` | `result` |
| --- | --- | --- |
| `describe` | none | see [describe](#describe) below |
| `acquire` | all kinds: `request_id`, `actor`, `ttl_s`?. db: `name`, `ref`?, `commit`?. run: `ref`, `commit`, `script`, `args`? (array of strings). box: `image`?, `ref`? and `commit`?, `jit_config`?, `jit_guard`? (`{"repo", "events"?}`), `net`? (`"on"` or `"off"`), `persist`? (bool), `volumes`? (`[{"name", "path", "mode"?, "sealed"?, "checkout"?}]`) with `volume_owner` and `volume_project` | `handle`, `expires_at`, `ttl_s`, `commit`?, `spend`?. db: `url`. run: `log`?. box: `size` (the size name), `image_digest`, `net`?, `volumes`? (`[{"name", "path", "mode", "new", "sealed"?, "checkout"?}]`) |
| `status` | none | `handle`, `state`, `expires_at`, `commit`?, `exit`?, `spend`? |
| `release` | none; box: `seal`? (bool, with capability `volumes-seal`), `keep`? (array of paths, with `seal` and capability `volumes-keep`) | `released` (bool), `exit`?, `spend`? |
| `list` | none | `handles`: `[{"handle", "request_id", "state", "expires_at"}]` |
| `progress` | `request_id` | `request_id`, `state` (`starting`, `ready`, `failed` or `cancelled`), `phase` (while `starting`), `message`? |
| `cancel` | `request_id` | `cancelled` (bool), `state` (as `progress` answers after the call) |
| `renew` | `ttl_s` | `expires_at` |
| `setup` | `dry_run` (bool) | `notes`? (array of strings) |
| `exec` | `argv` (array of strings), `stdin`?, `timeout_s`?, `cwd`?, `secrets`? (`{"NAME": "value"}`), `encoding`? (`"base64"`) | `exit`, `stdout`, `stderr`, `truncated` (bool), `encoding`? |
| `spawn` | exec's `argv`, `stdin`?, `cwd`?, `secrets`?, `encoding`?, plus `request_id`, `timeout_s`?, `pty`? (`{"rows", "cols", "term"?}`) | `pid` (a string) |
| `output` | `pid`, `stdout_from`?, `stderr_from`?, `max_bytes`?, `wait_s`?, `encoding`? | `stdout`, `stderr`, `stdout_next`, `stderr_next`, `dropped_stdout`?, `dropped_stderr`?, `running` (bool), `exit`?, `encoding`? |
| `signal` | `pid`, `signal` (`"TERM"`, `"INT"` or `"KILL"`) | `sent` (bool) |
| `input` | `pid`, `data`?, `encoding`?, `rows`?, `cols`? | `written`, `running` (bool) |
| `stop` | none | `stopped` (true), `expires_at` (when the kept disk is deleted), `spend`? |
| `start` | `ttl_s`? | as `acquire` answers for a box |
| `secrets` | `secrets`? (`{"NAME": "value"}`, each set or replaced), `unset`? (array of names) | `dir` (the directory, a path), `count` (names the box holds now) |

`spend` is `{"unit", "amount"}`. Times are RFC 3339 strings.

### Refusals

Every verb answers `not_implemented` when the driver does not serve it. Every verb that takes a handle answers
`bad_request` for a malformed handle or one of another binding, and `not_found` for one this binding never issued.

| verb | other refusals |
| --- | --- |
| `acquire` | `bad_request`: no `request_id`; `ttl_s` past the lifetime cap; an `image` not in config `images`; a script that is a shell string; an argument for a capability the driver lacks; `request_id` reused with other `jit_config`, `net` or `volumes`. `conflict`: a db name already made under another `request_id`; a volume in `build` that exists; `rw` or `build` of a sealed volume. `not_found`: a volume asked `sealed` that is not. `forbidden`: a db server not in `create_in`; a run's `commit` not in `ref`'s history. `unavailable`: no capacity, the reclaim policy says no, a volume another box holds (`data` reason `building` while it is built). An `unavailable` answer to a box acquire means nothing was made and nothing is held under the `request_id`: the daemon forgets the request and may give the same `request_id` to another binding. A driver whose box may still come up answers `internal` (or a timeout) instead, and the daemon looks it up in `list`. |
| `release` | none: a resource already gone answers `released: false` |
| `progress` | `bad_request`: no `request_id`. `not_found`: a `request_id` this binding never received (an acquire refused before it began may answer this too). |
| `cancel` | `bad_request`: no `request_id`. Never `not_found`: see [Acquire progress and cancel](#acquire-progress-and-cancel). |
| `renew` | `bad_request`: past the lifetime cap. `conflict`: a stopped box. |
| `exec` | `bad_request`: `argv` not a non-empty array; `timeout_s` past 600; `secrets` or `encoding` without the capability; a bad secret name or size. `conflict`: the box is not `ready`, or is past `expires_at`. |
| `spawn` | as `exec`; also `conflict` with 32 processes running, `bad_request` for `pty` without capability `pty` or `pty` with `secrets` |
| `output` | `not_found`: unknown `pid`. `bad_request`: an offset past what was written. |
| `signal` | `not_found`: unknown `pid` |
| `input` | `not_found`: unknown `pid`. `bad_request`: a process without a terminal. |
| `stop` | `bad_request`: a box acquired without `persist` |
| `start` | `unavailable`: no free slot, the reclaim policy says no, too little free disk. `conflict`: the binding's `root` or `docker` changed since the box was made. |
| `secrets` | `bad_request`: neither `secrets` nor `unset`; a bad name; a name in both; more than 64 KiB in the call, or held by the box after it; more than 256 names unset. `conflict`: the box is not `ready`, or is past `expires_at`. `forbidden`: the guest has no memory filesystem, swaps to a disk, or something not a private directory of the box's user is at the directory's path. |

### describe

```json
{"potluck": "potluck/1", "zm": "zm/v0", "name": "ref", "kind": "box", "version": "0.0.1",
 "verbs": ["describe", "acquire", "status", "release", "list", "renew", "exec"],
 "available": true, "isolation": "ephemeral", "credentials": false,
 "cost": {"unit": "cpu_s", "amount": 0}, "reports_spend": true, "ttl_max_s": 3600,
 "capabilities": ["base64", "secrets"],
 "size": {"name": "micro", "os": "linux", "cpu": 1, "memory_mb": 512, "disk_gb": 1}}
```

"Required" is what the conformance suite checks (`describeShape` in `conformance/checks.go`); the daemon checks
less (`kind`, the required verbs, and a box's `size.name`).

| field | required | meaning |
| --- | --- | --- |
| `potluck`, `zm` | at least one | the protocol (see [Versioning](#versioning)) |
| `name`, `version` | yes, not empty | the driver's name and version |
| `kind` | yes | `db`, `run` or `box`; must equal the binding's |
| `verbs` | yes | every verb served; must hold the required ones |
| `available` | yes | `false` while the driver cannot take an `acquire` |
| `why` | when `available` is false | the reason |
| `isolation` | yes | `"none"` (runs as some user on some machine) or `"ephemeral"` (a fresh, isolated resource each time); nothing else |
| `credentials` | yes | `true` when the resource can reach something beyond itself (a db server holding other databases, the machine user's files) |
| `cost` | yes: `unit` and `amount` | `{"unit", "amount", "currency"?}`: the unit spend is counted in |
| `reports_spend` | yes | whether `acquire`, `status` and `release` report `spend` |
| `ttl_max_s` | yes, above 0 | the lifetime cap, renewals included |
| `capabilities` | yes, an array (may be empty) | optional features, below |
| `size` | box: `name`, `os`, `cpu` above 0, `memory_mb` above 0 | `{"name", "os", "cpu", "memory_mb", "disk_gb"?}`; `disk_gb`, when given, not negative |
| `setup` | with verb `setup`: `touches` | `{"touches": [...]}`, what setup changes outside Potluck |
| `disks` | no | with capability `persist`: `{"kept", "max", "keep_s", "root"}` |
| `volumes` | no | with capability `volumes`: `{"max_gb", "keep_s", "modes"}` |
| `warm` | no | a driver that keeps booted boxes: `{"target", "ready"}` |
| `shelf` | with capability `shelf`: `path` | `{"path", "bytes", "files"}`: where a box finds the package shelf, and its size (see [The package shelf](#the-package-shelf)) |
| `runners` | no | a driver that starts a GitHub runner: a list with one entry per image, what that image's newest box's runner refresh found and did, below. Not checked by the suite |
| `host` | no | a driver with a host switch: `{"mode", "state", "until"?, "drain_since"?, "drain_deadline"?, "drain_cut"?, "refused"?}` (`protocol.HostStatus`); `refused` is `{"reason", "at", "until"}`, the host's admission controller's last "no room" answer while it stands; the daemon passes it through. Not checked by the suite |
| `serve` | no | an optional serve mode (unverified: neither the daemon nor the suite uses it) |

A box must also list `exec` and describe `isolation: "ephemeral"` and `credentials: false`. The daemon uses the
stricter of the binding's class and `describe`'s (`none` over `ephemeral`, `true` over `false`).

Capabilities a driver of any kind may list:

| capability | means |
| --- | --- |
| `acquire-progress` | the driver serves `progress` and `cancel` |

Capabilities a box driver may list:

| capability | means |
| --- | --- |
| `base64` | `exec`, `spawn` and `output` take `encoding: "base64"` |
| `secrets` | `exec` and `spawn` take `secrets` |
| `box-secrets` | the driver serves the `secrets` verb |
| `pty` | `spawn` takes `pty`, and the driver serves `input` |
| `net-off` | `acquire` takes `net`, and the answer says which network the box got |
| `persist` | `acquire` takes `persist`, and the driver serves `stop` and `start` |
| `volumes` | `acquire` takes `volumes` |
| `volumes-seal` | volume mode `build`, `release`'s `seal`, a volume's `sealed` and `checkout` (Sealed volumes) |
| `volumes-keep` | `release`'s `keep` beside `seal` (Sealed volumes) |
| `shelf` | every box sees the package shelf, read-only, at `describe.shelf.path` |
| `root` | the box user has working `sudo` with no password |
| `docker` | the box user can `docker run` (always with `root`) |
| `github-jit` | `acquire` takes `jit_config` |
| `github-jit-guard` | `acquire` takes `jit_guard` |
| `github-jit-guard-run` | `jit_guard` takes `run`: one run, held for the door's ready marker |
| `github-jit-guard-private` | `jit_guard` takes `open_if_private`: beyond `events`, any run whose event payload says the repository is private |

The daemon refuses an argument to a driver that does not list its capability (`bad_request`), before it calls the
driver: an older driver would take base64 text as the bytes, run a command without its secrets or terminal, or take a
`jit_config` and start no runner.

## A worked exchange

A box from the reference driver (`conformance/refdriver`), run by hand. To reproduce it, build a small main that
calls `refdriver.Main()`, which returns `(code int, handled bool)`, and exits with `code` when `handled`; then a
wrapper script that runs it as `<binary> -zm-ref-driver <state dir> ""`; then pipe
each request in with `env -i PATH=/usr/bin:/bin HOME=<dir>`. Every call below exited 0. Answers are as printed, with
`config` shortened to `{...}` after the first request.

**1. describe**

```json
{"potluck":"potluck/1","zm":"zm/v0","verb":"describe","config":{"target":"sandbox","kind":"box","images":"ref-image@sha256:0000000000000000000000000000000000000000000000000000000000000001"}}
```
```json
{"ok":true,"result":{"available":true,"capabilities":["github-jit-guard","base64","secrets","pty","persist","root","docker","volumes"],"cost":{"amount":0,"unit":"cpu_s"},"credentials":false,"isolation":"ephemeral","kind":"box","name":"ref","potluck":"potluck/1","reports_spend":true,"size":{"cpu":1,"disk_gb":1,"memory_mb":512,"name":"micro","os":"linux"},"ttl_max_s":3600,"verbs":["describe","acquire","status","release","list","renew","exec","spawn","output","signal","input","stop","start"],"version":"0.0.1","zm":"zm/v0"}}
```

**2. acquire**

```json
{"potluck":"potluck/1","zm":"zm/v0","verb":"acquire","args":{"request_id":"a1b2-job-7","actor":"ana","ttl_s":600,"image":"ref-image"},"config":{...}}
```
```json
{"ok":true,"result":{"expires_at":"2026-11-10T17:59:55.506841Z","handle":"box:sandbox/b1","image_digest":"sha256:0000000000000000000000000000000000000000000000000000000000000001","size":"micro","spend":{"amount":0.001771,"unit":"cpu_s"},"ttl_s":600}}
```

The same request again answers the same handle and makes nothing new.

**3. exec**

```json
{"potluck":"potluck/1","zm":"zm/v0","verb":"exec","id":"box:sandbox/b1","args":{"argv":["sh","-c","echo hello; echo oops >&2; exit 3"],"timeout_s":30},"config":{...}}
```
```json
{"ok":true,"result":{"exit":3,"stderr":"oops\n","stdout":"hello\n","truncated":false}}
```

A command's non-zero exit is a result, not an error.

**4. status**

```json
{"potluck":"potluck/1","zm":"zm/v0","verb":"status","id":"box:sandbox/b1","config":{...}}
```
```json
{"ok":true,"result":{"expires_at":"2026-11-10T17:59:55.506841Z","handle":"box:sandbox/b1","spend":{"amount":0.030482,"unit":"cpu_s"},"state":"ready"}}
```

**5. release, twice**

```json
{"potluck":"potluck/1","zm":"zm/v0","verb":"release","id":"box:sandbox/b1","config":{...}}
```
```json
{"ok":true,"result":{"released":true,"spend":{"amount":0.041077,"unit":"cpu_s"}}}
{"ok":true,"result":{"released":false,"spend":{"amount":0.041077,"unit":"cpu_s"}}}
```

**6. what comes after**

```json
{"potluck":"potluck/1","zm":"zm/v0","verb":"exec","id":"box:sandbox/b1","args":{"argv":["true"]},"config":{...}}
{"error":{"code":"conflict","message":"box:sandbox/b1 is released, not ready"},"ok":false}

{"potluck":"potluck/1","zm":"zm/v0","verb":"status","id":"box:sandbox/b9","config":{...}}
{"error":{"code":"not_found","message":"no box:sandbox/b9"},"ok":false}

{"potluck":"potluck/1","zm":"zm/v0","verb":"status","id":"box:other/b1","config":{...}}
{"error":{"code":"bad_request","message":"box:other/b1 is not a handle of this binding"},"ok":false}

{"zm":"zm/v0","verb":"list","config":{...}}
{"ok":true,"result":{"handles":[{"expires_at":"2026-11-10T17:59:55.506841Z","handle":"box:sandbox/b1","request_id":"a1b2-job-7","state":"released"}]}}
```

The last request carries only the old key; the driver answers it the same.

A run looks the same with `potluck-driver-local` (`kind: "run"`, config `remote` and `state_dir`): `acquire` with
`ref`, the full 40-character `commit` and `script` answered `handle: "run:home/r7f8154f03a4c8baf284e"`, the
`commit` echoed and a `log` path; once the script finished, `status` answered `state: "ended", exit: 0`;
`release` then answered `released: false, exit: 0`, and `renew` answered `not_implemented`.

## Rules a driver must hold

### Handles

- Handles are `db:<target>/<name>`, `run:<target>/<id>` and `box:<target>/<id>`, where `<target>` is config
  `target`. The daemon refuses an answer whose handle does not start with `<kind>:<target>/`.
- A db `<name>` is the caller's `name` (the driver may prefix it internally).
- Keep an ended handle's record for at least 24 hours, so `status` and `release` still answer it.

### Idempotent acquire

- `request_id` is required. A repeat with the same key answers the same handle and never makes a second resource.
- `ttl_s` is optional. The daemon sends it only when the caller gave one, and the protocol sets no default: the
  driver picks its own, at most its `ttl_max_s`. The drivers here use 60 s (the reference driver), 600 s
  (`potluck-driver-tart`) and config `default_ttl_s`, default 1800 s (`potluck-driver-local`). Answer the TTL you
  chose as `ttl_s`.
- The daemon sends `<actor tag>-<request_id>`, so two actors' keys never meet. A machine serving another machine's
  pool adds a tag of that pool's project in front as well.
- A db name that exists under another key is `conflict`.
- After an `acquire` times out, the daemon sends it again with the same key, then reads `list` to find it. It
  never acquires afresh. So a slow acquire must be safe to receive twice.
- The arguments are part of the request: a reused key with a different `jit_config`, `net` or `volumes` is
  `bad_request`.

### Acquire progress and cancel

`acquire` may take up to its 120 s limit and has no handle until it answers. With capability `acquire-progress`
(verbs `progress` and `cancel`, listed together with it) the driver says how far an acquire got and can stop it,
both by its `request_id` (the key the daemon sent, as for `acquire`). A driver without it still works: the daemon
then tells callers only that the acquire is starting, and refuses their cancel.

- Both answer within their 30 s limit while that acquire is still running: they must not wait for it (the
  reference driver keeps progress under a lock of its own, apart from the one its acquire holds). `progress` knows
  an acquire as soon as the driver has received it; the suite fails a `not_found` 3 s into an acquire still running.
- `progress` answers the latest acquire with that key. `state` is `starting` (in flight), `ready` (it made the
  resource; an `acquire` with the same key answers its handle), `failed` (refused or failed; it holds nothing; a
  later `acquire` with the key may try again) or `cancelled`. While `starting`, `phase` is a lowercase word of at
  most 32 characters (letters, digits, `-`) naming the step: the reference driver says `queued`, then `booting`;
  a VM driver might say `cloning`, `booting`, `checkout`. `message` is one line for a person about the step, never a
  secret and never a quote of what the acquire was sent; the daemon passes it on only while `starting`, and
  not for an acquire that carried a `jit_config` or one relayed from a lending host.
  A key never received is `not_found`.
- `cancel` stops an acquire in flight: anything it made is ended, as on release; the acquire answers `conflict`;
  `progress` says `cancelled`; `list` shows nothing live for the key. Answer `cancelled: true`.
- A cancel that lands after the resource was made but before `acquire` answered still wins: end the resource and
  answer the acquire `conflict`. A resource the driver cannot end is answered as the acquire's handle instead
  (`progress` then says `ready`), so it is held and released rather than live with no holder. Once `acquire` answered a handle (`ready`), `cancel` changes nothing and
  answers `cancelled: false`; the caller releases the handle.
- An `acquire` refused for a key that already made its resource changes nothing: `progress` still says `ready`.
- A cancel after an acquire that `failed` is kept and answers `cancelled: true`: the failed acquire holds nothing,
  and a retry with the same key, perhaps already sent and queued, must not make the resource after all.
- A cancel of a key never received is kept (for at least 24 hours, as handles are) and answers `cancelled: true`:
  the daemon may cancel an acquire it holds queued before sending it, and that acquire, if it comes, must be refused.
- After a cancel, an `acquire` with that key answers `conflict` and makes nothing, and a second `cancel` answers
  `cancelled: true`. If an earlier acquire with the key made a resource and then died (killed at its time limit, a
  crash) before its cancel could end it, that later `acquire` ends it first; one it cannot end it answers as the
  handle. A caller that wants to try again uses a new `request_id`.

### States

| state | meaning |
| --- | --- |
| `ready` | the database exists, or the box answers `exec` |
| `running` | the run is running |
| `stopped` | a box acquired with `persist`, stopped with its disk kept (not an end) |
| `ended` | the run exited on its own; `exit` is its code |
| `expired` | the deadline ended it; a run has `exit: 124` |
| `released` | `release` ended it |
| `reclaimed` | the machine's owner took it back (the driver's own reclaim policy); no `exit` |
| `lost` | the driver cannot say how it ended |

- Only `ended` reports a script's own exit, so a run that was cut off never reports success.
- A box is never `running` or `ended` and has no `exit`.
- `release` answers `released: true` when it ended the resource and `false` when it was already gone, never an
  error. It carries `exit` only when a run had already ended.
- The daemon records any other state as an error (`internal`), and a `not_found` from the driver's `status` as `lost`
  (a host that answers `not_found` for another reason, such as not being in the project, has not spoken about the
  box: `unavailable`). Once a handle's record has ended, `status` answers the record and the driver is not asked
  again.

### Lifetime, leases and expiry

- A resource lives exactly as long as its lease. On `release` or at `expires_at` the driver ends it: drops the
  database, kills the run's process group, wipes the box.
- The driver enforces `expires_at` itself and never relies on the daemon to clean up: the daemon may be down at
  the deadline. The reference driver's per-resource supervisor shows how.
- `ttl_max_s` (the binding's or describe's, the lower) caps the whole lifetime since `acquire` (a persistent box's:
  since its last `start`), renewals included. A `ttl_s` past it is `bad_request`, on `acquire` and `renew`.
- `renew` sets `expires_at` to now plus `ttl_s` (as the reference driver does). Without `renew` a lease cannot be
  extended.
- The daemon renews a lease held by a live session once a third of its TTL is left, at most once a minute.

### Reclaim

A driver on a lent machine may let its owner take it back (owner activity, a schedule window). While its policy
says no:

- `describe` answers `available: false` with `why`, and `acquire` answers `unavailable`.
- The driver ends every live resource, as on release, and answers `reclaimed` for it from then on. It does this
  itself, like expiry, whether or not the daemon is up.
- `release` of a reclaimed handle answers `released: false`, with no `exit`.
- A reclaimed handle's spend is still reported, but it is never billed.
- A driver may drain first: refuse new work while running work finishes, then end what is still running after a
  bounded time (potluck-driver-tart's `drain_deadline_s` and `host set drain`). What it ends is `reclaimed`.

### Spend

- `acquire`, `status` and `release` may report `spend`: the handle's total so far, in describe's `cost.unit`.
- It never falls, and `status` still reports it after the end. The daemon keeps the highest figure it sees and asks
  `status` once more after release, expiry or reclaim for the final one.

### Runs

- A run executes a clean checkout of exactly `commit`, never anyone's working tree, and echoes `commit` in
  `acquire` and `status`. The daemon refuses an echo that differs.
- `script` is a name the driver resolves (an npm script, a make target, a file in the checkout), never a shell
  string; `args` are literal arguments. That is hygiene, not protection: the code at `commit` defines what the
  script does.
- Exit codes are the run's channel: pass the script's exit code through as `exit`.
- On release or expiry, kill the run's whole process group.

### Databases

- `create_in` (a binding key the daemon passes in config) lists where the driver may create: each entry a server
  `host:port` plus the maintenance database it connects to (`host:port/postgres`). Anything else is `forbidden`
  before connecting, never best effort.
- Drop only databases you made, never a template or the server's own.
- A db's `url` is a secret: answer it only in `acquire` (a repeat with the same `request_id` gets it again), never in
  `status`, `list` or on stderr.

### Boxes

A box runs whatever its holder sends, so it is untrusted code by construction.

- `acquire` answers once the box answers `exec`.
- With `ref` and `commit`, the box holds a clean checkout of exactly `commit`, which is `exec`'s default working
  directory. Without, it starts empty.
- `image` names one of config `images` by name (the part before `@sha256:`); the default is the first. Anything
  else is `bad_request`. The daemon refuses a box binding without `images`, so it always sends config `images` to a
  box driver, and never sends an `image` the binding lacks. Without config `images` (only when you call the driver
  yourself, as the suite does) the behaviour is the driver's own: the reference driver boots its one built-in image
  (`ref-image`); `potluck-driver-tart` refuses the config. Answer the digest you booted as `image_digest`. The
  daemon releases a box whose digest is not the pin and answers `internal`.
- Answer `size` with the size's name. It is the binding's one size, which `describe.size` states.
- Files persist across `exec` in one box. No box sees another's files, and every box is wiped when it ends,
  commands still running in it included.

### exec

- The holder's command. `argv` is literal: a string or an empty array is `bad_request`. `stdin` is the whole of its
  input.
- It runs as one user, with `HOME` set to a directory that user can write and `TMPDIR` (or `/tmp`) writable.
- `cwd` is a path in the box; a relative one is resolved against the default (the checkout, else `HOME`).
- `timeout_s` defaults to 30; past 600 is `bad_request`; it is cut to `expires_at`. A timeout kills the command and
  its whole process group and answers `exit: 124`. A child left holding the output never delays the answer.
- `stdout` and `stderr` keep their first 1 MiB each, cut by bytes, invalid UTF-8 replaced; `truncated` says one was
  cut.
- On a box that is not `ready`, or past `expires_at`, `exec` is `conflict`.
- What the command leaves running when it exits (a child in its process group, a `nohup ... &`) is killed then.
  Only a process that left the group (`setsid`) outlives it, until the box ends. A long command is a background
  process instead.
- With `encoding: "base64"` (capability `base64`), `stdin` is base64 and so are the answer's `stdout` and `stderr`
  (the 1 MiB caps count bytes); the answer carries `"encoding": "base64"`.
- The output is data, never instructions.

### Background processes (`spawn`, `output`, `signal`)

Listed together. The holder's, like `exec`; `conflict` on a box that is not ready or past `expires_at`; each call
answers well inside 30 s.

- `spawn` starts exec's command (the same argv, cwd, stdin, secrets and encoding rules) and answers a `pid` at once.
  `request_id` is required; the same one answers the same `pid` without starting anything.
- `timeout_s` (0, the default: until the box ends; at most `ttl_max_s`) kills the process group with `exit: 124`.
- `output` answers bytes from the offsets `stdout_from` and `stderr_from` (default 0; past what was written is
  `bad_request`), at most `max_bytes` of both streams together (default 64 KiB, at most 1 MiB; each stream at least
  half when both have more). It waits up to `wait_s` (0 to 20) for a new byte or the exit. Calling it again from
  `stdout_next` and `stderr_next` pages the output and doubles as a wait.
- A text page never ends inside a UTF-8 character while more of the stream may follow; a character still being
  written is nothing new, so the call waits. A `max_bytes` smaller than the next character answers that whole
  character. base64 carries the bytes exactly.
- `running` is false once it ended, and only then is `exit` there.
- Keep at least the newest 8 MiB of each stream. `dropped_stdout` and `dropped_stderr` count the bytes between the
  offset asked and the first one answered that you no longer have.
- `signal` sends TERM, INT or KILL to the process's group and answers `sent: false` once it ended.
- The group's leftovers die when the process exits, as an exec's; every process ends with the box.
- A box remembers 32 processes: past that an older ended one is forgotten, and with 32 running `spawn` is
  `conflict`. An unknown `pid` is `not_found`.

`boxproc/` holds a shared host-side implementation (used by the reference driver and `potluck-driver-tart`).

### Terminals (`spawn` with `pty`, `input`)

With capability `pty`. A terminal needs no ssh key and no open port: it goes over the exec channel.

- `spawn` with `pty` runs the command on a pseudo-terminal of `rows` by `cols` (1 to 1000), with `TERM` set to
  `term` (default `xterm-256color`), in its own session, the terminal its controlling one.
- Both streams are its output and come as `stdout`; `stderr` stays empty. `stdin` is typed first.
- A pty takes no `secrets` (`bad_request`): a terminal echoes what it reads.
- `input` writes `data` (at most 64 KiB; base64 with `encoding`, which every driver serving `input` must take) to
  the terminal, applying `rows` and `cols` (together, 1 to 1000) first. It answers `written` (bytes taken; fewer
  when the terminal read nothing for 10 s) and `running`.
- An ended process answers `written: 0, running: false`; one without a terminal is `bad_request`; an unknown `pid`
  `not_found`.
- Ctrl-C and Ctrl-D are bytes like any other; the box's own terminal turns them into SIGINT or an end of input.
- `output` and `signal` work as for any background process. `signal` reaches the command's own process group, not a
  job its shell moved to the foreground.

### Secrets

With capability `secrets`. A renter may hand a command its own secrets in `exec`'s (or `spawn`'s) `secrets`. Who may
send which is the daemon's policy, checked before a driver is called (`docs/daemon.md`, "Secret grants"); a driver
sees only secrets that passed it and needs nothing for it.

- Names match `[A-Z][A-Z0-9_]{0,63}`; at most 64 KiB of names and values in all; an empty map is none.
- A driver without the capability answers `bad_request` and never runs the command without them. A box that is not
  ready answers `conflict` first.
- Put each value in a fresh `0700` directory as a `0600` file named for it, in memory in the guest: on a tmpfs or
  ramfs (Linux `/dev/shm`) in a guest that swaps nowhere but zram, never on a disk, never swapped to one and never
  under `TMPDIR` as a fallback. A guest without such memory refuses the secrets (`forbidden`), and the command or
  process does not run.
- The guest receives the values on the exec channel, never in argv, the environment or the VM's config, in the
  guest or in any host-side process.
- The command runs with `POTLUCK_SECRETS` and `ZM_SECRETS` set to that directory (a path, never a value). An `exec`
  without `secrets` has neither.
- Remove the directory before answering, when the command ends or times out, and with the box. A driver killed at
  the deadline (`timeout_s` + 30 s) cannot clean up: the directory then lasts until the box ends.
- Never put a name or a value on stderr or in an answer, a refusal included.
- One box is one trust domain: anything else running in it as the same user can read the directory while it exists.
- Between machines a call is sealed only when it carries `secrets` (or a `jit_config`): then its whole body is
  (`docs/daemon.md`, "Sealing between daemons"). Without them `argv`, `stdin` and `cwd` travel readable, and the
  answer's `stdout` and `stderr` always do: the pool's machine reads them and, over a Cloudflare Tunnel, Cloudflare
  can too. A command that prints a secret sends it back readable.

### Box secrets

With capability `box-secrets`. The `secrets` verb puts secrets in a box for the box's life, not one command's: a
token renewed while a long job runs, a login every later command reads.

- `secrets` sets each name to its value, replacing a file the box holds; `unset` removes names (one it does not
  hold is no error). One call does either or both, never naming a name in both. Names are exec's, and the box
  holds at most 64 KiB of names and values in all: a call past it is `bad_request` and changes nothing.
- Each value is a `0600` file named for it in one `0700` directory of the user commands run as, at a path fixed for
  the box's life, in memory in the guest: drivers whose guest has `/dev/shm` use `/dev/shm/potluck-secrets`. In
  memory means a tmpfs or ramfs in a guest whose only swap, if any, is zram (`/proc/swaps`; without it, `SwapTotal`
  0 in `/proc/meminfo`): never swapped to a disk, or refused. Never a disk, never `TMPDIR`: such a guest refuses
  (`forbidden`).
- Something at the path that is not a directory of the box's user (a symlink, another user's directory) is never
  used: the call is refused (`forbidden`), nothing is written through it, and no command is pointed at it.
- A write a killed call left beside a value (the driver's temporary file) is removed by the next call.
- macOS guests take no secrets through this path, nor through `exec`'s: they have no memory filesystem a driver can
  prove, so a binding whose image is macOS lists neither capability, until a driver proves a RAM disk.
- Under gVisor the guest's view is the sandbox's own: its tmpfs and swap table say nothing about whether the
  sandbox's memory reaches the host's swap. That is the host's to rule out (no swap, or encrypted swap), and
  nothing inside the guest can check it.
- A value reaches the guest on the exec channel, never in argv, an environment, the VM's config, a record or a
  log, on the host or in the guest.
- A new value is written beside the old one in the same directory and renamed over it, so a reader never sees
  half a file. Readers read the file at each use.
- Every later `exec`, `spawn` and terminal without secrets of its own runs with `POTLUCK_SECRETS` and `ZM_SECRETS`
  set to the directory while it holds a name (a path, never a value). A command with secrets of its own gets its
  own directory, as above, and the box's stays where it is. The answer's `dir` is the same path.
- The directory goes with the box on every way it ends (release, expiry, reclaim, a lost host, a supervisor's
  death, a host restart) and on a kept disk's `stop`: it lives in the guest's memory. A `start` boots with none.
- Never put a name or a value on stderr or in an answer, a refusal included; `count` is a number.
- The daemon sends `secrets` only to a trusted host's driver listing the capability, for the box's holder, under
  a secret grant at scope `box` (no built-in default covers it), never for a name the pool's machine mints into
  boxes itself; and sealed, as exec's secrets (`docs/daemon.md`, "Secret grants").

### Network

With capability `net-off`.

- `acquire`'s `net` is `"on"` (the default) or `"off"`; anything else is `bad_request`.
- `on`: the box gets the binding's network. `off`: the box gets no network at all.
- Answer `net` with what the box got, for every box. The daemon ends a box it sent `off` whose answer is not exactly
  `off`, and answers `internal`.
- `net` is part of the request: `on` and unset are the same request, and a `request_id` reused with the other is
  `bad_request`.
- A driver without `net-off` is sent no `net`.

What the daemon sends, and how the VM driver builds a filtered network, is in `docs/network.md`.

### Kept disks (`persist`, `stop`, `start`)

With capability `persist`.

- `acquire`'s `persist` makes a box that stops instead of ending. Never with a `jit_config` or `volumes`
  (`bad_request`).
- `stop` ends everything running in the box and keeps its disk. It answers `stopped` and, as `expires_at`, when the
  kept disk will be deleted. `stop` of a box acquired without `persist` is `bad_request`.
- The lease reaching `expires_at`, a reclaim, or the guest shutting itself down stop the box the same way, never
  wiping it.
- `start` (`ttl_s`, as acquire's) boots that same disk on the same host with a new lease and answers as `acquire`
  does. The lifetime cap counts from it.
- `release` deletes a stopped box's disk at once. Past `expires_at` the driver deletes it itself and the box is
  `expired`.
- A stopped box is `stopped` in `status` and `list`; `exec`, `spawn`, `renew` and the rest are `conflict`. It holds
  no slot, only its disk, and its spend does not grow (report time up only).
- `describe.disks` says `{"kept", "max", "keep_s", "root"}`. `root` says whether the root filesystem is kept or only
  `HOME`; conformance's `persist` group holds a driver to it.

### Named volumes

With capability `volumes`.

- `acquire`'s `volumes`: up to 8 of `{"name", "path", "mode"}`, each a directory that outlives the box and is
  mounted at `path` in every box that names it.
- `path` (`protocol.VolumePathOK`): relative to the box user's `HOME`, never absolute, at most 256 characters, only
  `A-Z a-z 0-9 . _ / @ + -` (so no `,` or `=`), every segment a plain name (no empty segment, `.` or `..`). Paths
  must not overlap, nor a name come twice. A driver that knows `HOME` also refuses a path `HOME` makes a system
  directory.
- `name` starts with a lowercase letter or a digit, then lowercase letters, digits, `.`, `_` and `-`, at most 63 in
  all.
- A volume belongs to `volume_owner` (the daemon sets it, never the caller), else `actor`. Two owners naming the same
  volume have two volumes. `volume_project` is the project it counts against.
- `mode` is a readers-writer lock per volume, held while the box is live. `rw` (the default) is one box's at a
  time: an acquire naming a volume another live box holds in any mode is `unavailable`. `ro` mounts it read-only;
  `cow` gives the box a copy-on-write view of its own, dropped with the box. Both are shared by any number of the
  owner's boxes, and `unavailable` while a box holds it `rw` (so a copy's base never changes under it). So a cache
  is filled once by a `rw` box, then read or copied by every other box at once.
- Answer `volumes`: `[{"name", "path", "mode", "new"}]`, `new` when this acquire made the volume.
- A box with volumes is never `persist` and never a `jit_config`'s (`bad_request`).
- `describe.volumes` says `{"max_gb", "keep_s", "modes"}`. There is no verb to list or delete a volume: the keep
  time and the caps delete them.

### Sealed volumes

With capability `volumes-seal` (and `volumes`). A sealed volume is filled once by one box, then never written again;
readers can tell it was filled to the end.

- Mode `build`: the volume must not exist; the acquire makes it (`new` true). It is one box's like `rw`.
- `release` with `{"seal": true}` from the box holding a volume in `build` seals it, once every process of the box
  has ended and nothing references the volume. Any other end of that box (a release without `seal`, expiry, reclaim,
  a crash, a quota reclaim) deletes the volume. So a half-filled volume is never there for a reader. `seal` on a box
  holding no volume in `build` does nothing; a driver without `volumes-seal` ignores it. `release` answers only once
  the volume is sealed or deleted, so the next acquire sees it so. A box past `expires_at` has expired, even before
  the driver reaped it: its release with `seal` deletes the volume.
- With capability `volumes-keep`, a `release` with `seal` may carry `keep`: 1 to 4096 paths relative to the
  volume's root, each a volume path (plain segments). Once every process of the box has ended and nothing references
  the volume, the driver deletes every entry but those paths and the directories on the way to them, then seals it:
  a directory on the way keeps only those entries, and a kept path or a directory on the way that is not a real
  directory (a link, a file) is deleted. No link is ever followed; what a kept directory holds stays as it is. A
  volume the driver could not prune so is deleted, never sealed. `keep` without `seal`, or with a path that is not
  a volume path, is `bad_request` (the box is not released).
- A sealed volume carries no setuid or setgid file and no device node, FIFO or socket. Once every process of the
  box has ended (and after any `keep`), the driver clears every setuid and setgid bit and deletes every such entry,
  never following a link, then seals; what the files hold stays. A volume it could not clean so (an entry left, or
  a directory it could not walk) is deleted, never sealed. So nothing a build box made, even with `root`, gives a
  later box of the owner a privilege a fresh box lacks. The cleanup does not remove file capabilities
  (`security.capability`). A driver should also make a setuid or setgid bit, a file capability or a device node in
  an `ro` or `cow` view inert: `nosuid,nodev` on the view, boxes that run with `no-new-privileges`, or a runtime
  that ignores them. A view's mount need not carry `nosuid,nodev` itself (the OCI layer's under gVisor carry neither
  in the box; see daemon.md).
- An acquire that is refused, or whose box never starts, leaves no volume it would have made in `build`.
- A sealed volume refuses `rw` and `build` for good. `ro` and `cow` read it as any volume. It is deleted only whole,
  by the keep time and the caps, its sealed record first.
- A volume in `ro` or `cow` mode may carry `"sealed": true`: the acquire then makes nothing and needs that owner's
  volume of that name to be sealed. `sealed` with `rw` or `build` is `bad_request`, and so is `sealed` on a driver
  that does not list `volumes-seal` (it must never mount a plain volume for it). Answer `volumes` echoes `sealed`.
- Sealed is the driver's own record, never a file in the volume: nothing a box writes can make a volume pass as
  sealed. A volume a caller filled `rw` is never sealed, whatever its box's release says. A plain `ro` or `cow` of a
  volume that does not exist makes it, empty, as for any volume; a `build` of that name is then `conflict`.
- A box holding a volume in `build` is never `persist` (`bad_request`).
- A volume in `cow` mode may carry `"checkout": true`: when the box is asked for a `commit`, its checkout goes into
  that volume's directory (`HOME/path`) instead of the driver's own place, and exec's default working directory is
  there. So code sits beside what the volume holds (a dependency layer's `node_modules`) at the paths it was built
  at. The driver never removes or remakes that directory (it may be a mount point): the commit's files are written
  over what is there, every other file is left as it is, and a `.git` the volume holds is replaced unread (its config and hooks never apply to the checkout). As with any cow
  write, none of it reaches the volume. At most one volume a box carries `checkout`; with `rw`, `ro` or `build`, on
  two volumes, or on a driver without `volumes-seal` it is `bad_request`. Without a `commit` it changes nothing
  (the caller puts code there itself). Answer `volumes` echoes `checkout`.
- `describe.volumes.modes` lists `build` when the driver lists `volumes-seal`.

Argument refusals (`bad_request`, above) come first. Then each volume of an acquire is answered by the first of
these that applies, in this order:

1. `not_found`: it asks `sealed` and the volume is not sealed (also while a box is building it).
   Optional `data: {"reason":"volume_missing", "name":"<requested volume>"}` identifies which volume
   refused the acquire. When supplied, the name must be exactly that requested volume. Older drivers may omit
   this detail; a daemon must not infer the missing volume from the message or from another volume's state.
2. `conflict`: `rw` or `build` of a sealed volume.
3. `unavailable`: another live box holds it in `build`, with `data` `{"reason": "building", "retry_s": N}`
   (`protocol.Busy`): ask again after `retry_s` seconds. Or another live box holds it in a mode the lock forbids (the
   `mode` rules above).
4. `conflict`: `build` of a volume that exists.

### Warm boxes

A box driver may keep boxes booted and idle; `describe.warm` then says `{"target", "ready"}`.

- An acquire a warm box can serve takes one at once; the pool refills in the background.
- A warm box is nobody's: it has no handle on the wire (`list` shows none, `status` of its id is `not_found`), its
  time is not spend, and a taken box's lease, lifetime cap and spend start at the take.
- It is a fresh box: nothing of an earlier box's.

### The package shelf

With capability `shelf`. The shelf is a read-only tree every box of the binding sees: public package tarballs
(`docs/shelf.md` has their layout and the in-box helper), and on the OCI layer also the owner's `tools/` and
`goproxy/` trees.

- `describe.shelf.path` is where a box finds it: a clean absolute path in the box (`/opt/potluck-cache` on the OCI
  layer), or, for a driver whose box is a directory with no root of its own, a clean path relative to the box
  user's `HOME`. `bytes` and `files` are its size; when it cannot be measured, `why` says so instead. Describe never
  names what is on it.
- Only the host writes the shelf. No box can create, overwrite, append to, delete or rename anything on it, as
  root either where root is on; a chmod fails, or leaves nothing the box's next command can write. Nothing a box
  does at the shelf path reaches another box, live or later.
- A box never places a byte on the shelf. It may name what it wants in its wants file, `$HOME/.potluck/wants`
  (`protocol.ShelfWants`): one want per line, fields split by one space, `npm <name> <version> sha512-<base64>`
  or `go <module> <version>` (`<version>/go.mod` for a go.mod alone, as go.sum writes it).
  The file is data, read only from inside the box: a driver that fills the shelf from wants validates every line,
  drops what it cannot read, fetches by name and version from the registry without credentials, and keeps a
  tarball only if its hash is the registry's and the line's; a Go module only from the public Go proxy (never from
  a server the line names), checked against the public checksum database. A blob in the box's own cache is never
  copied out.
  vmhost reads wants only when the host's owner sets `shelf_fill=boxes` (docs/shelf.md); with the default `seed`
  the shelf holds only what the owner put on it.
- A box's cache, its links into the shelf and its wants file are the box's, gone with it (a warm box too): the
  next box has none of them.

Conformance's `shelf` group, and the warm group's shelf check, hold a driver to this; both skip without the
capability, and the write checks skip when the shelf holds no file.

### Root and Docker

- List `root` only when the box user has working `sudo` with no password, and `docker` (always with `root`) only
  when that user can `docker run`.
- A driver listing neither must not give sudo.
- Root holds the box's own bounding set and no more: it must lack `NET_ADMIN`, `NET_RAW` and `MKNOD`. Conformance's
  `root` group checks this.
- A box never runs with more root than describe says, nor advertises a docker it lacks.

### GitHub runners

- With capability `github-jit`, `acquire` takes `jit_config`: a just-in-time runner config (GitHub's
  `encoded_jit_config`). The box's first command is that runner, started before `acquire` answers.
- A driver without the capability answers `bad_request`, as does a `jit_config` on any other kind.
- It is a single-use credential. Hand it to the guest on the exec channel, never in the host's argv or environment,
  the VM config, a record or a log, and keep no copy.
- A runner that exits within the first seconds fails the `acquire` (`unavailable`). Reusing a `request_id` with a
  different `jit_config` is `bad_request`.
- The box stays `ready` after the runner exits, until it is released or expires. A runner box is never persistent.
- Whoever may acquire the box may bring their own `jit_config` (the box then serves the repo that config names; the
  class rules still hold). The runner broker brings one it minted for a linked installation.
- With capability `github-jit-guard`, `acquire` also takes `jit_guard`: `{"repo", "events"?}` (at most 32 event
  names). The runner then refuses a job unless `GITHUB_REPOSITORY` is `repo` (case-folded) and, when `events` is
  given, `GITHUB_EVENT_NAME` is one of them and, for a `pull_request*` event, the event payload names a
  `pull_request.head.repo` whose `full_name` is `repo` (its `fork` flag is not read: a repository that is itself a
  fork has it set on its own branches). No `events`: any event, any origin.
- With capability `github-jit-guard-private`, `jit_guard` also takes `open_if_private` (a bool, only with `events`;
  without them `bad_request`): a job `events` refuse (another event's run, a run from a fork) is served too when the
  event payload's `repository` has `private` true and `visibility` `"private"` (the payload is the run's, as GitHub
  wrote it when it created the run). A payload that says anything else
  (public, internal, either field missing, no payload) refuses it. Reusing a `request_id` with another
  `open_if_private` is `bad_request`.
  How `potluck-driver-tart` enforces it is in `docs/hosts.md`.
- With capability `github-jit-guard-run` (listed only beside `box-secrets`), `jit_guard` also takes `run`:
  `{"id", "workflow_ref", "ref", "hold_s"?}`, a branch ref (`refs/heads/<branch>`), a workflow ref of `repo`'s
  `.github/workflows/` at that ref, and a hold of at most 600 s (default 120). The runner then also refuses a job
  unless `GITHUB_RUN_ID` is `id`, `GITHUB_WORKFLOW_REF` is `workflow_ref` (case-folded) and `GITHUB_REF` is `ref`,
  and holds a job that passes until the box secret `POTLUCK_READY` (the door's ready marker) holds `id`; no marker
  within the hold refuses it. With a run, the runner's environment has `POTLUCK_SECRETS` (and `ZM_SECRETS`) set to
  the box secrets' directory.
- Registering and deregistering the runner, and sweeping orphans, are the daemon's, never the driver's.
- A driver that replaces a runner GitHub would refuse (`potluck-driver-tart` does, before the runner starts) says
  what its newest box of each image found in `describe.runners`: a list with one entry per image of the binding, in
  the binding's order, `{"image", "version", "image_version"?, "refresh", "why"?, "at"}`. `image` is the image as the
  binding names it. `refresh` is `not_needed` (the image's runner was recent enough), `refreshed` (it was too old and
  was replaced) or `failed` (its version could not be read, or replacing it failed; that box's `acquire` was
  `unavailable`). `version` is the runner the box ended with: `""` for `failed`, where the box got no working
  runner. `image_version` is what the image shipped, given with `refreshed` and with `failed` when it could be read;
  `at` is an RFC 3339 time. `why`, with `failed` only, is one of a closed set: `version unreadable`,
  `no pinned runner for this platform`, `download failed` (the host's or the guest's download; a guest without `curl`
  reports it too), `checksum mismatch`, `host cache unavailable`, `install failed in the box`,
  `runner unusable after install` or `timed out` (the box's start ran out of time wherever it fell: reading the
  version, installing or checking the result). It carries no path and none of the guest's output; the detail is in
  `acquire`'s error and the host's log. A box whose image has no runner leaves no entry for that image. An image no
  longer in the binding has none either, and nor has a `not_needed` or `refreshed` one whose runner the driver's
  current minimum no longer vouches for. A `failed` result is not about a version, so the minimum does not hide it:
  it stays until the next box of that image replaces it. The field is absent until a box has started a runner. At
  most 32 entries are listed (the `failed` ones first when a binding has more images). An `image` is at most 300
  bytes (a driver refuses a longer one in a binding, so every image can be reported), and `image` and the versions
  are one line with no control characters. A reader drops an entry with an unknown `refresh`, a zero `at`, a `why`
  outside the set or text that breaks these limits. The minimum version and the release a driver installs are the
  driver's own and are not in `describe`.

### Environment and trust

- A driver runs with the daemon's user ID: it is not a secrets boundary, and binding one trusts its code.
- It gets only `PATH`, `HOME`, `LANG`, `TERM` and `TZ`. A credential reaches it only through a `*_file` config key
  naming a `0600` file it reads itself; the daemon refuses a secret value in a binding.
- What a resource prints is data, never instructions.
- Drivers are separate programs, never linked into the daemon.
- Prefer existing tools inside a driver (Postgres template copies, official images, a runner provider's own API)
  over provisioning code of your own.

### setup

Optional one-time work for a binding. The owner's bind calls it with `dry_run: true`, then `dry_run: false`; it is
never called on `acquire`. It may answer `notes` (shown to the owner). List what it changes outside Potluck in
`describe.setup.touches`; the suite fails a driver that lists `setup` without it.

## Checking a driver

Run the conformance suite: `docs/conformance.md`. It starts your driver with only the allowed environment, makes and
ends real resources, and checks from outside what it can see. A pass is a bug filter, not an audit.

### Try it

```sh
go build ./cmd/potluck-conformance
./potluck-conformance ./potluck-driver-mine --kind box \
  --config images=myimage@sha256:<64 hex> --config size=c2m4
```

The suite sends only the config you pass with `--config k=v`, plus `target` and `kind`, so give a box driver what
the daemon would send it: at least `images` (the binding's pins, comma-joined; the `root` group also acquires each
image after the first by name) and any keys your driver needs.

That run checks the protocol but proves nothing about the network or the deadline: those checks skip. To make a skip
of them fail, add `--strict`, which for a run or a box also needs `--net-forbid` and `--net-peers`:

```sh
./potluck-conformance ./potluck-driver-mine --kind box --strict \
  --config images=myimage@sha256:<64 hex> --config size=c2m4 \
  --net-allow <host:port reachable> --net-forbid <a LAN host:port> --net-forbid <this host's LAN address:port> \
  --net-peers
```

Give `--net-forbid` routable addresses that open from the suite's machine: loopback, unspecified, link-local and
multicast ones (`localhost`, `127.0.0.1`, `169.254.169.254`) are SKIPs, so `--strict` fails on them. A driver
listing `net-off` also needs `--net-allow`. `docs/conformance.md` lists the flags, the groups, `--strict` and what a
SKIP leaves unproven.

## A new hypervisor instead

To lend boxes from a new hypervisor or container runtime, you may not need a driver at all: `potluck-driver-tart`
takes VM layers (Tart, Incus, OCI). See [`docs/vm-layers.md`](vm-layers.md).

### Sandbox port channels

A box driver may list verb `port`. Its holder can open TCP only to
`127.0.0.1:<port>` inside that same box, never to a caller-supplied host.
`protocol.PortRequest` names `op`: `open` takes `port` (1..65535) and answers a
random opaque `stream`; `read`, `write` and `close` take that stream. `write`
takes binary `data` (JSON base64). Each page is at most 65536 bytes. A read
answers `data` and `eof`; a final EOF may include data, and an empty non-EOF page
means no bytes are ready yet. A read and write can progress independently under
backpressure. Read EOF leaves the upload direction usable until close. The
net.Conn adapter serializes each direction; the guest helper retains at most one
active operation and one queued 64 KiB page per direction. Additional concurrent
calls may be refused. Pages are not replayed or automatically retried. An acknowledged write may still be buffered; it does
not certify that the peer has drained its bytes. Read EOF leaves the upload
direction available until close or lease end.

Every operation checks that the original box is ready and before its lease
expiration. Streams never name a host or another box. Closing, lease expiration
or destroying the box ends its guest-side connection and helper. The generic
HTTP, WebSocket and private browser/CDP transports all consume this one verb.
The channel's original deadline is not extended by renewing the box.

### Project database snapshots

A driver listing `database-snapshot-v1` accepts acquire's `database` object:
`project` (authority project ID), `name` (recipe name), `image` (immutable final
image pin) and `manifest` (the exact packaged JSON text). These are resolved by
the authority from a project-owned recipe; public acquire requests contain only
the recipe name. A host relay rejects a database whose project differs from its
authenticated authority scope. The manifest's own `project` identifies its source
repository, which may differ from the pool project's identity.

The driver validates before allocating, compares all four fields on a repeated
request ID, and confirms the same `database` object in a successful acquire
answer. It must not return success or permit commands before the private
Postgres snapshot has passed its readiness and provenance checks. Credentials
are never in this object or driver metadata. The authority releases an answer
with different or missing provenance. See [database snapshots](database-snapshots.md)
for the supported manifest, command environment and lifecycle restrictions.
