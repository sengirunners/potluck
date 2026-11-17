# Driver conformance suite

The conformance suite is a black-box test kit for drivers: executables named `potluck-driver-<name>` (or `zm-run-<name>`, their old name) that speak the
protocol in `docs/protocol.md` (one JSON request on stdin, one JSON answer on stdout). It starts your driver, makes
and ends real resources through it, and checks what it can see from outside; what it cannot see, it reports as SKIP
or takes from `describe`.

It proves that a driver behaves as the protocol says, on the machine where you ran it, with the flags you gave it.
It does not prove the driver's author can be trusted, and it cannot see what a hostile driver does when it is not
being tested. A driver can recognise the suite and behave. Treat a pass as a bug filter, not as an audit.

## Build and run

```sh
go build ./cmd/potluck-conformance         # writes ./potluck-conformance
go build ./cmd/potluck-driver-local        # writes ./potluck-driver-local, used in the example below
./potluck-conformance --help               # lists every flag; exits 2
./potluck-conformance --print-probe > probe.sh
```

`--print-probe` prints the probe script. Commit it to a git repository as an executable file. For a run (and for a
box's checkout check) the driver runs this script, and the suite uses it to see what a run really did.

The first argument is a path to the driver, or a name: `scratch` finds `potluck-driver-scratch`, else `zm-run-scratch`, on PATH. Flags may come
before or after it. `--kind db|run|box` is needed only when the driver serves more than one kind; otherwise the
suite asks `describe`. Exit codes: 0 no check failed, 1 at least one check failed (with `--strict`, also a skipped
network or expiry check: see below), 2 usage error.

A real run against `cmd/potluck-driver-local`, which needs no root, VM or container. The repository holds `probe.sh` in its
first commit and one more commit on top, so `--commit` is not the tip of `--ref` (otherwise the HEAD check is a
SKIP):

```sh
./potluck-conformance ./potluck-driver-local --ref main --commit <older commit> --repo /path/to/remote.git \
  --script probe.sh --config remote=/path/to/remote.git --config state_dir=/tmp/state \
  --ttl-short 2 --grace 5s --run-wait 30s
```

That ended with exit 0 after about 9 seconds: 38 PASS, 1 WARN, 5 SKIP, and the line `5 check(s) skipped: not
proven`. The WARN is the driver's class (`isolation none`, credentials true), which is a pass a person should read.
Invocations for `potluck-driver-tart`, which start real VMs, are in `cmd/potluck-driver-tart/README.md`.

A db driver also needs a way to see the database: `--db-probe '<shell command>'` (the url is in
`$POTLUCK_CONFORMANCE_URL`, and in its old name `$ZM_CONFORMANCE_URL`; exit 0 when the database is there). The
default is `psql "$POTLUCK_CONFORMANCE_URL" -Atc 'select 1'` when `psql` is on PATH. Without either, the db checks that need it are SKIPs.

The suite makes and ends real resources. Point it at a throwaway binding. It hands the driver only `PATH`, `HOME`,
`LANG`, `TERM` and `TZ` from its environment, and a driver that needs more must take it from `--config`.

## Check groups

`--only a,b` runs only the named groups; `describe`, the envelope checks and the final checks (nothing left
behind, no url or secret leaked, verb timing, spend) always run. Groups run in this order.

| group | kinds | proves | needs |
| --- | --- | --- | --- |
| `boot` | box | one box comes up, reports its size, runs exec, releases | runs only when named in `--only` |
| `handles` | all | unknown verbs, malformed requests, malformed, foreign and never-issued handles are refused with the right codes | nothing |
| `lifecycle` | all | acquire/status/list/release shapes, idempotent `request_id`, release kills a run's process group, the database or box is really there and really gone, spend, box exec, stdin, timeouts, output caps, renter secrets | run: `--ref --commit --script`; db: `--db-probe` or psql; run on a shared filesystem, else `--remote` |
| `acquire-progress` | all | `progress` and `cancel`: listed with the capability, `not_found` for a key never sent, a cancel before the acquire or after a refused one refuses it, `starting` with a phase while in flight and `ready` after, a refused repeat and a late cancel change nothing, a cancel in flight ends it (`conflict`, nothing live) and the key stays refused | capability `acquire-progress`; the in-flight checks skip when the acquire answers before `progress` sees it |
| `procs` | box | background processes: spawn, output paging and retention, signal, base64 | driver lists `spawn`, `base64`, `secrets` capabilities for the optional parts |
| `agent-runner` | box | the agent runner job kind (`docs/agent-runners.md`): a job's files arrive, its entry point gets the key's file and its arguments, its exit passes through, the key is still there seconds in, TERM lets it drain | verb `spawn`, capability `secrets` |
| `pty` | box | terminals: size, input, resize, Ctrl-C | capability `pty` and verb `input` |
| `box-secrets` | box | the `secrets` verb's isolation: modes, a memory filesystem, no swap, the owner, a planted path, no value in env, argv or the driver's state, gone with the box; without the capability, refused | capability `box-secrets`; `--state-dir` (or config `state_dir`) for the state check |
| `box-secrets-ops` | box | what the verb does: unset, an exec's own secrets beside the box's, atomic replace, refusals, conflict after release | capability `box-secrets` |
| `box-authority` | box | from inside a box, a command reaches the pool's machine with only the credential a `potluck-token` grant delivers, and reaches nothing once it is unset | capability `box-secrets`; `--box-credential`, `--box-client`, `--box-want` |
| `persist` | box | kept disks: stop/start keep the disk, the deadline stops rather than wipes | capability `persist`, verbs `stop` and `start` |
| `ended` | run | a script that exits 7 shows `ended` with that exit; release after the end | run flags |
| `code` | run, box | the run's HEAD is exactly the commit sent and the tree is clean; a dirty `--worktree` does not leak in | `--repo` (HEAD proof); `--worktree`; for a box, `--ref --commit` |
| `renew` | all | renew's lifetime cap and a later `expires_at`, or `not_implemented` when not listed | verb `renew` |
| `expiry` | all | a short ttl ends the resource with no release and no call to the driver; status then says `expired` (run: exit 124); with `--strict`, for a box, `list` is the first call after the deadline and grace and must show it `expired` | `--ttl-short`, `--grace`; see skips |
| `reclaim` | all | the owner's reclaim policy ends the resource with no call to the driver; status `reclaimed` | `--reclaim-config` |
| `refuse` | all | missing `request_id`, `ttl_s` past the cap, shell-string script, unknown box image, bad exec arguments; for db a host not in `create_in` is refused before any connection | db: `--outside-config` with `{port}` |
| `net` | run, box | from inside: reaches what it should, resolves names, cannot reach forbidden addresses or send mail, peers cannot see each other, a box made with `net off` reaches nothing | `--net-*` flags |
| `root` | box | with capability `root` or `docker`, sudo works and root lacks `NET_ADMIN`, `NET_RAW`, `MKNOD`; docker runs a container; without them sudo must not work | capabilities `root`, `docker` |

Network flags: `--net-allow host:port` (must open; proves a network), `--net-forbid host:port` (must not open: a
LAN host, this host's LAN address, a tailnet or metadata address; see below for which addresses it can prove),
`--net-mail host[:port]` (a bare host means ports 25, 465 and 587), `--net-resolve name[:port]`, `--net-peers` and `--net-peer-port`. Other tuning: `--target`, `--config k=v`,
`--ttl-short` (default 5), `--grace` (default 30s), `--run-wait` (default 5m), `--reclaim-in` (default 1m).

A `--net-forbid` host is resolved on the suite's machine and each address it resolves to is checked as a literal,
the name never sent to the guest. An address of one of these kinds is a SKIP (`not proven`), never probed:
loopback (`127.0.0.0/8`, `::1`, so `localhost`), unspecified (`0.0.0.0`, `::`), link-local (`169.254/16`, `fe80::/10`,
zoned or not) and multicast. On this machine they mean this machine; inside a VM they mean the guest itself or
nothing, so a probe there proves nothing about reaching the host. To forbid the host, give its LAN address. Any
other address must open from the suite's machine, else it is a SKIP too: a closed port would pass however open the
guest is.

## Checks in detail

The suite sends the envelope with only the allowlisted environment, and sees what a resource does without taking the
driver's word: a run through a probe script committed at `commit` (`--print-probe` prints it; exit codes are its
channel, so a driver passes them through), a database through `--db-probe`. What cannot be seen is a SKIP, not a pass.

Flags that set up what it can see:

- A run's `--commit` should be older than the tip of `--ref` on `--repo` (the ref moving after the daemon resolved
  it): only then does a driver running the ref instead fail; else that check is a SKIP.
- `--worktree` names the checkout to dirty. `--remote` says a run shares no filesystem with it.
- `--outside-config` is the config that points a db target outside `create_in`.
- `--reclaim-config` is the config whose reclaim policy says no once `{window}` closes. The suite fills it with
  `HH:MM:SS-HH:MM:SS`, local time, closing `--reclaim-in` after the acquire (default 1m).
- `--net-allow` and `--net-forbid` (`host:port`, `[v6]:port`, repeatable) are addresses a run or a box must and must
  not open a TCP connection to from inside (the internet; a LAN host, the host's LAN address, a tailnet, private
  IPv6). Which forbidden addresses can prove anything is under "Check groups" above: loopback, unspecified,
  link-local and multicast ones are SKIPs. A box runs the same probe through `exec`, as `sh -c`'s script, so it
  needs no commit.
- `--net-mail` (a host, checked on 25, 465 and 587, or `host:port`, repeatable) is an outside mail server none may
  open. It is resolved on the suite's machine and each address probed as a literal, so a guest that cannot resolve
  the name does not pass. It is a SKIP unless it opens from the suite's machine: a closed port proves nothing.
- `--net-resolve` (a DNS name, or `name:port`, repeatable) is a name a run or box must resolve (`getent hosts`; a
  guest without getent must open `name:port`, default 443). By default it is `--net-allow`'s names, or `github.com`
  when all are IP literals, so a guest that reaches addresses but resolves no name fails here, not in its first job.
  It is a SKIP when the name does not resolve on the suite's machine.
- `--net-peers` runs two at once and checks neither reaches the other over IPv4 or IPv6 link-local neighbours (the
  probe's `net` subcommand). A guest without `nc`, `ip` or `ping` is a SKIP; a probe committed before that subcommand
  existed fails with "re-commit `--print-probe`".

For every kind it checks:

- describe's shape (class, `reports_spend`, spend in `cost.unit`);
- acquire, status, release, with `commit` echoed;
- a run's HEAD equal to the `commit` sent, with a dirty working tree where the driver runs not leaking in;
- release twice (`released: false`), and `exit` only for a run already ended;
- an unknown handle `not_found` and another binding's `bad_request`;
- the same `request_id` twice giving one handle and one resource; an existing name under a new key `conflict`;
- `list` showing the handle and its key;
- renew bounds (or `not_implemented`) and the lifetime cap across renewals;
- a short ttl expiring with `exit: 124` and the resource gone without a release;
- a reclaim ending the resource as `reclaimed` with no `exit`, `describe` then unavailable and `acquire`
  `unavailable`;
- spend never falling, and readable by `status` after the end;
- a db host not in `create_in` `forbidden`; no `url` on stderr;
- only the allowlisted environment reaching the driver; a shell-string script refused; and nothing left behind.

With `--kind box` (`--ref` and `--commit` optional; the guest needs a POSIX `sh`, `awk`, `sleep`, `cat`, `kill`,
`ps`, for the checkout `git`, and for the net checks `nc`: a box without it fails them, never a SKIP), the handle,
renew, expiry, reclaim and refusal groups run as for the other kinds, and in place of a run's checks it checks:

- `exec` and `size` in describe, and the untrusted-capable class;
- acquire answering once `exec` works, with `size` and `image_digest`;
- `exec`'s stdout, stderr and exit, stdin, files kept across `exec`, a `timeout_s` answering 124 in time with the
  command's child ended, exactly the first 1 MiB of stdout and of stderr with `truncated`;
- `exec` after release and after the deadline `conflict`;
- a new box not seeing the last one's files (in `$HOME` and `${TMPDIR:-/tmp}`);
- a clean checkout at `commit`;
- an image not in `images`, a shell-string or empty `argv` and `timeout_s` past 600 `bad_request`;
- with capability `base64`, every byte value through exec's stdin and stdout.

Group `procs` (a box listing `spawn`; else `spawn` must answer `not_implemented`; not in `potluck host`'s quick
run): a pid per `request_id`, the same one again starting nothing; `output` with `wait_s` waiting on a quiet process;
TERM to a process that traps it ending its child (its group); `sent` false after the end; the 64 KiB default page,
`max_bytes`, offsets and bad args; past 8 MiB the newest 8 MiB kept and the rest counted as dropped; base64 through
spawn and output; secrets as for exec, gone once it ends; a 22 s process read in pages while it runs, then its exit;
all three `conflict` after release.

With the `secrets` capability: the value a `0600` file in a `0700` `$POTLUCK_SECRETS` (or `$ZM_SECRETS`; read back
in hex), in no process's environment or argv the box can see (`env`, `ps`, and `/proc/*/environ` where there is one;
best effort: a host-side leak is beyond a black box), the directory gone after the command and after a timeout, no
secrets variable without secrets, bad names (with a space, empty, 65 characters, a trailing newline, lower case) and
64 KiB plus one byte `bad_request` while exactly 64 KiB runs, and neither a value nor a name sent on the driver's
stderr or in an answer. Without it, an `exec` with secrets must be `bad_request`.

Group `agent-runner` (a box listing verb `spawn` and capability `secrets`; else every check is a SKIP: such a box
serves no agent runner): it starts jobs with `agentrunners.Start`, the code `potluck agent-runner` uses, with
stand-in adapters in place of a vendor's runner (the guest needs `od`, `tr`, `ls`, `cut` and `sleep`). The first
stand-in echoes the arguments its file carries (one holds `$(...)`, backquotes and quotes: never run), checks its
key's file holds the value sent (in hex) and is `-rw-------`, reads it again 3 s later and exits 7; the suite
wants those lines and exit 7. The second says `ready`, and on TERM says `drained` 2 s later and exits 0; the suite
sends TERM once it is ready and wants that exit. The value counts for the final "no secret on stderr or in any
answer".

Groups `box-secrets` and `box-secrets-ops` (the second: unset, an exec's own secrets, the reader, the refusals and
conflict after release; the first: the rest) (a driver listing capability `box-secrets`; without it, a `secrets` call must answer
`not_implemented` or `bad_request` and leave no secrets variable in a later exec): one box gets two values. A later
exec finds `$POTLUCK_SECRETS` equal to the `dir` the call answered; the value reads back whole (in hex) from a `0600`
file in a `0700` directory owned by the user commands run as (`ls -ldn` against `id -u`); `stat -f -c %T` names a
`tmpfs` or `ramfs`; `/proc/swaps` lists no swap but zram (without it, `/proc/meminfo` says `SwapTotal` 0); the
value is in no `env`, `ps -ww -eo args` or `/proc/*/environ` line;
unset removes one name and keeps the other; an exec with secrets of its own gets another directory and the box's is
still there after; a reader looping in the box for 5 s, while the suite replaces a 24 KiB value with another again
and again, sees only whole values, and both; bad names (set and unset), an empty call, a call past 64 KiB and a
call taking the box past 64 KiB held are `bad_request` and change nothing; once the box holds no name, a symlink
planted at the directory's path is refused, its target gets nothing and no later exec is pointed at it, and (with
capability `root`, commands not run as root) so is a directory of root's planted there; after release the verb answers
`conflict`, and a new box sees neither the old directory nor a secrets variable. The value is searched for in every
file under the driver's state directories (`--state-dir`, repeatable; by default config `state_dir` and
`~/.potluck-driver-<name>` where they exist), outside the box's own secrets directory, while the box holds it and
after its release; with no state directory that check is a SKIP, which `--strict` fails. Names and values sent count
for the final "no secret on stderr or in any answer". With `persist` listed too, group `persist` checks that a kept
box set a secret before its stop starts with none. The filesystem type and the swap table are what the guest
reports: a driver that fakes them inside the guest passes, as it could fake any probe.

Group `box-authority` (a driver listing capability `box-secrets`, and `--box-credential <file>`, `--box-client
<command>` and `--box-want <text>` given together; without them, a SKIP): the file is a box credential, what a
`potluck-token` grant delivers into a box (`docs/daemon.md` "Calling the authority from a box"), for a pool's
machine the driver's boxes can reach over its tunnel. One box gets it through the `secrets` verb; an exec then runs
the command (split on spaces) with `POTLUCK_BOX_CREDENTIAL` naming the landed file, and must exit 0 printing the
text (`potluck status --json` and the pool's name, with a `potluck` built for the guest on its `PATH`). After the
name is unset the same command must fail and not print it: nothing else in the box reaches the authority. The file,
and the token inside it, count for the final "no secret on stderr or in any answer".

Group `persist` (a driver listing it): a file in `HOME` survives stop and start; a file in `/var/tmp` too when
describe's `disks.root` says `root`; stop of a box without persist is `bad_request`; release of a stopped box; a
lease's end stops rather than expires.

Group `volumes` (a driver listing it): what a `rw` box wrote is read by the next; `rw` is one box's; another owner's
volume of the same name is another; `ro` refuses writes; a `cow` box's writes are its own; bad names, paths and modes
are `bad_request`.

Group `shelf` (a driver listing `shelf`): `describe.shelf.path`; no box can create, overwrite, append to, delete,
rename or chmod anything on the shelf (as root too where root is on); nothing a box does at the shelf path reaches a
live or later box; a blob a box put in its own cache and named in its wants never shows up on the shelf; the next box
has none of the last box's links into the shelf or its wants file. The write checks try only creating when the shelf
holds no file.

Group `warm` (a driver whose describe says `warm`): its shape, no handle in `list`, the lease from the take, the pool
refilling, and a fresh box after a release; with a shelf, also none of the last box's links or wants.

Group `net` for a box, and group `root`, are described in `docs/network.md` and `docs/hosts.md`.

## What a skip means

A skipped check is reported as `SKIP` but is a passing result: `skip` in `conformance/conformance.go` records
`OK: true`, so it does not count as a failure, and without `--strict` the exit code is 0. Every run ends with
`N check(s) skipped: not proven` and one `  - <check> (<group>)` line per skipped check. In `--json` mode those
lines go to stderr, so stdout stays one JSON array; there each skipped result has `"skipped":true` and its
`"group"`. A report with skips is not a report that the skipped thing works.

Skips that happen without being asked for:

- `net` skips as a whole when none of `--net-allow`, `--net-forbid`, `--net-mail`, `--net-resolve`, `--net-peers`
  is given (`conformance/net.go`). A report with that skip says nothing about isolation.
- For a box, the reclaim check "a reclaim ends the resource with no call to the driver" is always a SKIP. Only
  runs (with a heartbeat file, so not under `--remote`) and databases (with a database probe) can be seen from
  outside. For a box, expiry's "ends with no call" check is a SKIP too, unless `--strict`, which runs a weaker box
  form of it instead (see "`--strict`" below).
- A run under `--remote` skips the heartbeat checks: the run is going, release kills the process group, and the
  expiry and reclaim "ends with no call" checks.
- No `--repo`, or `--commit` equal to the tip of `--ref`: the HEAD check is "passed, not proven" (a SKIP), since a
  driver ignoring `commit` would pass. For a box, no `--ref/--commit` skips the checkout check.
- No `--reclaim-config`: reclaim skips. No `--worktree`: the dirty-tree check skips. No `--outside-config` (or no
  `{port}` in it): the db `create_in` check skips or is "before connecting not proven". No db probe: db existence
  checks skip.
- Checks that depend on what the driver lists in `describe`: no `renew`, `spawn`, `pty`, `persist`,
  `root`/`docker`, `secrets`, `box-secrets`, `base64`, or `reports_spend` each skip their part. That is legitimate for a driver
  without them, but it is still unproven.
- Network sub-checks skip when the guest lacks `nc` (or `bash` and `timeout`), `ip` or `ping`; a `--net-forbid`
  address, name or mail server that does not resolve or open from the suite's own machine; a `--net-forbid` address
  that is loopback, unspecified, link-local or multicast; and `net off` without `--net-allow`.
- `serve mode` and `setup` are skipped whenever the driver lists them (`describe` says it serves, or lists the
  `setup` verb): the suite runs one-shot and never sends `setup`.

For a box driver, the flags that leave no skip you can remove (the skips that cannot be removed remain) are:

```sh
potluck-conformance potluck-driver-<name> --kind box --ref main --commit <older pushed commit> --repo <remote> \
  --config remote=<remote> --reclaim-config 'window={window}' --reclaim-in 90s \
  --net-allow <host:port reachable> --net-forbid <LAN host:port> --net-forbid <this host:port> \
  --net-forbid <metadata or tailnet host:port> --net-mail <outside mail host> --net-resolve <name> --net-peers
```

A driver that drains before it reclaims needs its drain off for the reclaim check: for potluck-driver-tart add
`--reclaim-config drain_deadline_s=0`. For a run driver use `--script` and `--worktree` as well, and `--remote` if the run shares no filesystem with the
suite. Whatever the flags, read the output for the SKIP lines left. A group that belongs to another kind prints one
SKIP (`ended` for a box or db; `net` and `code` for a db).

## `--strict`

`--strict` adds one last check, `strict: the network and expiry checks ran, none skipped`, which fails (so the exit
code is 1) when:

- a check in the `net` or `expiry` group was skipped (for a db, only `expiry`: a database's outbound reach is not
  covered by this suite; `conformance.StrictGroups`);
- one of those groups did not run (left out of `--only`, or the run was cut off);
- the target is a run or a box and `--net-forbid` or `--net-peers` was not given. `--net-mail` is not required (it
  needs a mail server this machine reaches), but without it nothing shows the guest cannot send mail.
- the `box-secrets` group ran and skipped "the value is in no file under the driver's state directory": give
  `--state-dir` (or config `state_dir`).

Its detail names each reason, and the run ends with a `--strict failed: ...` line. Without `--strict` nothing
changes: the same check is not added, the box expiry check below does not run, and the exit codes are as above.

Some drivers cannot pass `--strict` at all:

- A run under `--remote`: the expiry group's "ends with no call" check needs the run's heartbeat, which `--remote`
  cannot see, so it always skips.
- A box driver listing `net-off` needs `--net-allow` too: without it the "a box made with net off reaches nothing"
  check skips.

What a strict pass proves, beyond a plain pass: the net probe ran inside the guest and was shown able to fail; every
`--net-forbid`, `--net-allow`, `--net-mail` and `--net-resolve` address you gave was really probed, none skipped for a
missing tool, a name or address that does not resolve or open from this machine, or a forbidden address of a kind
that means something else inside a guest (so each forbidden address is a routable literal that opens from here and
did not from inside); two runs or boxes at once did not reach each other on the scanned neighbours; and a short ttl
ended the resource with no release.
For a run (not under `--remote`) and a db (with a database probe) that end is seen from outside. For a box, once one
`status` and one `exec` have shown it alive, the suite makes no call until the deadline plus `--grace` has passed,
and then its first call is `list`, which must show the box `expired`. A driver that ends boxes only when `status`
asks fails that.

What it still does not prove:

- That the network results are true. The probe runs inside the guest and reports through the driver (a run's exit
  code, a box's `exec` answer); a driver that recognises the probe can fake its answers. Like the box expiry check,
  the net group rests on the driver's word.
- That a box's VM really stopped at its deadline. The box check rests on the driver's word: a driver that ends a box
  lazily on any call, `list` included, passes it. Nothing outside the guest is watched.
- Anything about addresses you did not name. There is no automatic probe of the guest's default gateway or the
  host: pass the gateway and the host's LAN address (with a port that is open on the host, so a pass means blocked
  rather than closed) as `--net-forbid`. The host's loopback cannot be named from inside a VM (above). Strict only
  requires at least one `--net-forbid`; it cannot tell a thorough list from a thin one.
- Outbound mail, unless you gave `--net-mail`; and a database's outbound reach (not covered).
- Reclaim. `reclaim` is not in the strict groups: without `--reclaim-config` it skips, and for a box its "no call to
  the driver" check always skips.
- Every other skip (renew, spawn, pty, persist, root, secrets, box secrets, spend, the HEAD check, the dirty worktree, `serve`,
  `setup`) still passes under `--strict`. Read the skip list.

## Reading the output

Each line starts with `PASS`, `FAIL`, `SKIP` or `WARN`, then the check name and, when there is one, `: detail`.
`WARN` is a pass a person should look at (for example, a driver whose class is unisolated or credentialed). Secrets
and database urls are scrubbed from the details. `--json` prints one array of
`{name, ok, skipped?, warn?, detail?, group?}` (`group` is the check group that made the result; describe, the
envelope and the final checks have none). Failed checks are counted and the exit code is 1.

When you publish a result for your driver, include:

- the exact command line, with hosts and secrets replaced but every flag kept;
- the host OS and architecture, and the runtime versions (the driver's version from `describe`, Go, the hypervisor
  or container runtime, the guest image and its digest);
- the whole output, unedited, and the commit of the suite you built;
- the list of SKIP lines, each with what it leaves unproven.

## How the suite is tested

`conformance/refdriver` is a correct driver for all three kinds, plus a `Bug` switch: `Install(dir, state, bug)`
writes a `zm-run-ref` wrapper that re-executes the test binary with the bug name, and `driver` consults `d.is(...)`
at the places where it can be wrong. It exists only for tests; a test binary calls `refdriver.Main` from
`TestMain`, so the command line tool cannot be pointed at it.

Example, from `conformance/refdriver/refdriver.go`: with bug `wrong-head`, a run clones the remote but skips
`git checkout <commit>`, so HEAD is the tip. In `conformance/conformance_test.go`, `brokenDrivers` lists it as
`{"run", "wrong-head", "code", "FAIL  a run's HEAD is the commit sent", nil}`: the `code` group must print
that line.

`TestGoodDrivers` requires a correct driver (and variants without optional verbs) to pass with no FAIL. At the end
of `TestBrokenDrivers`, every check name a good driver passed must also appear in the set some broken driver
failed; otherwise the test fails with `no broken driver fails "<check>"`. So the rule for a new check: add a bug to
`refdriver` that breaks exactly what the check tests, add a row to `brokenDrivers`, and show the check passes on
the good driver and fails on the bad one. A check with no failing driver fails CI. The same suite also runs in
`internal/localdriver/localdriver_test.go` (`TestConformance`) and `vmhost/fake_test.go` over a fake VM layer.

```sh
go test ./conformance/ -run 'TestBrokenDrivers/drivers/run/wrong-head/code' -v
```

## Known gaps

All checked in the code:

- Without `--strict`, skips pass and exit 0. A CI job that only checks the exit code cannot tell a thorough run
  from a hollow one. With `--strict`, only the network and expiry skips fail (above).
- Nothing checks that a hypervisor or other hardware isolation sits under a box. The suite sees a box only through
  `exec`. `describe` must declare `isolation` ephemeral and credentials false for a box, but that is only a
  declaration; no check tests it.
- The network checks use only the addresses you give. There is no automatic probe of the guest's default gateway or
  the host's addresses as seen through user-mode networking: pass the gateway and the host's LAN address as
  `--net-forbid`. Loopback, unspecified, link-local and multicast forbidden addresses are SKIPs.
- A box's reclaim "no call to the driver" check is always skipped. Its expiry one is skipped too without
  `--strict`, and with it takes the driver's word (above).
- The network checks take the driver's word for what the probe saw inside the guest (above).
- The probe is a shell script that depends on `nc`, `bash`/`timeout`, `ip`, `ping` and `getent` in the guest; where
  they are missing the net checks skip or fail with the missing tool named.
- `serve mode` and `setup` are not exercised.
- The suite runs on one machine at one moment, from one caller.
