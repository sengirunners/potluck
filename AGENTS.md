# Working on Potluck (for coding agents)

Go module `github.com/sengirunners/potluck`. Apache-2.0. `cmd/potluck` is the program (daemon and commands, in `cli/`).

## Map

| path | what |
| --- | --- |
| `cmd/potluck`, `cli/` | the program: `potluck daemon` (kit/server with `wiring.Compute`) and the commands; home `$POTLUCK_HOME`, default `~/.potluck` |
| `cli/mount.go` | `cli.Commands(Host)`: Potluck's commands for a program that mounts them; every hint names a command through the host's prefix (`a.cmd`), never a literal `potluck <verb>` (`docs/mounting.md`) |
| `DRIVERS.md` | where the daemon finds drivers, and their sha256 pins (`runtarget/pin.go`) |
| `docs/protocol.md` | the driver protocol (potluck/1; the old names it still reads: "Names") |
| `protocol/` | its wire types |
| `vmhost/` | VM host layer: Tart, Incus, OCI (Kata, gVisor); supervisor, leases, admission |
| `boxproc/` | host side of a box's background processes and PTYs |
| `vmhost/shelfhelper/` | `potluck-deps`, the in-box helper for the package shelf (`docs/shelf.md`), embedded as `shelfhelper.Script` |
| `conformance/` | the driver conformance suite |
| `conformance/refdriver/` | reference driver with a Bug switch (tests only) |
| `cmd/potluck-conformance` (`internal/conformancecmd`) | runs the suite against a driver |
| `cmd/potluck-driver-tart`, `cmd/potluck-driver-local` (`internal/localdriver`) | drivers: VMs on this host; jobs on this machine |
| `cmd/zm-run-tart`, `cmd/zm-run-local`, `cmd/zm-run-conformance`, `cmd/zm-e2b` | one-line mains: the same programs under their old names (zm's build lists and old updaters name these paths; keep them) |
| `ghapp/` | GitHub App auth for runners; one-repository tokens minted and revoked per holder |
| `issuers/` | the secrets door's minted kinds (`github-token`, `potluck-token`): one lifecycle (`minted.go`), a backend per kind; ceiling, renewal, revocation, ledger |
| `jobrule/` | the one copy of which GitHub jobs may hold a secret (events, ref, fork), shared by grants and issuers |
| `site/` | static site (wrangler config) |
| `kit/` | KIT: identity, projects, transports, the daemon shell (`kit/server`), teamlink |
| `kit/daemonrecord/` | the machine's record of its running Potluck daemons (`<program>.json`), written by `kit/server`; how `potluck` and the menu bar item find a daemon of another program built on Potluck (`cli/attach.go`, `docs/daemon.md`) |
| `database/` | project database recipes (`[[databases]]` in `project.toml`) and the snapshot manifest rules; `docs/database-snapshots.md` |
| `agentrunners/` | the agent runner job kind (`potluck agent-runner`): a vendor's self-hosted runner in a fresh box; adapters are data (`claude.go`); `docs/agent-runners.md` |
| `agenttasks/` | the agent task (`potluck agent-task`): a coding agent CLI run once in a fresh box on a pushed commit, its commits back as a local ref; adapters are data, the box's entry script; `docs/agent-tasks.md` |
| `runtarget/`, `runners/`, `usage/`, `grants/` (secret grants), `hostprobe/`, `hostswap/` (the host's swap state: reported in its advert, gates box secrets), `computeapi/`, `computeclient/`, `boxcp/`, `boxpush/`, `wiring/`, `cmd/potluck-e2b` (`internal/e2bgateway`) | COMPUTE: boxes, sandboxes, runners, usage, wirings |
| `softnet/` | which Softnet a Mac uses (`For`, the one version matrix), the Softnet builds Potluck pins (`Builds`, sha256) and their managed root-owned install with rollback (`Store`); `potluck softnet` (`cli/softnet.go`), `docs/softnet.md`. Softnet is FSL-1.1-ALv2 (`softnet/UPSTREAM-LICENSE`), not Apache |
| `hostsetup/` | a host run's stages (preflight, images, validation, join, usable capacity) as it goes, in the home's `host-setup.json`; `menubar/setup.go` turns them and the menu app's and daily update's state into `host status`'s setup checklist (`docs/hosts.md`) |
| `roomwait/` | the one wait of a command told "no room now" (`computeapi.RefusedNoRoom`, or a caller's `roomwait.Full`): asks again up to `--wait`, a line per change; `potluck sandbox create`, `run` and `host` use it (`cli/room.go`) |
| `placement/` | the one rule for which box binding takes a new box (sandboxes and runner jobs): tier order, refusals with reasons, ranking |
| `pushbase/` | client-side repository identity and host base fill/retry (`docs/push-base.md`) |
| `depscli/` | the client side of dependency layers: the `--deps` flags, the acquire's deps, the answer's line, the mismatch warning (`potluck sandbox create`, `potluck run`; zm imports it) |
| `internal/forkguard`, `internal/workflowlint` | the fork-guarded `runs-on` rule and its edit (`potluck ci pr`, `docs/ci-pr.md`); the fork-safe workflow lint; `potluck ci doctor` reads guarded `runs-on` through forkguard |
| `internal/layers` | the layer rule: KIT non-test code imports no COMPUTE; no dependency on zm |

## Commands

```sh
go build ./... && go vet ./... && go test ./...
gofmt -l .        # must print nothing
```

CI (`.github/workflows/ci.yml`) runs build, vet and test.

## Rules

- Tests never touch a real home directory and never start real VMs. Test mains set `HOME` to a temp dir
  (`conformance/conformance_test.go`, `vmhost/fake_test.go`). Use `t.TempDir()`.
- Use the fakes: the `fake` VM layer (`dirLayer` in `vmhost/fake_test.go`: a VM is a directory), `fakeIncus`
  (`vmhost/incus_test.go`) and `fakeOCI` (`vmhost/oci_test.go`), which are shell scripts standing in for the CLIs.
  There is no fake Tart; Tart code is tested through `dirLayer`.
- A vmhost test whose driver starts supervisors takes its state directory from `stateDir` or `fakeBinding`
  (`vmhost/started_test.go`): its cleanup waits for every process started from it before any TempDir goes.
- Never weaken a conformance check to make a driver pass. Fix the driver.
- Every new check must be shown to fail on a broken driver. `conformance/refdriver` takes a bug name (see
  `Install`); the driver reads it as `d.bug` and misbehaves in one specific way. Add a bug that your check
  catches, and add a row to `brokenDrivers` in `conformance/conformance_test.go`. `TestBrokenDrivers` fails with
  "no broken driver fails ..." if a passing check has no broken driver that fails it (only on a full
  `go test ./conformance/`; a `-run` filter skips that part).
- Isolation refusals fail closed. A layer that cannot isolate refuses `isolated` (`vmhost/layer.go`); an ACL it
  cannot read as safe counts as unsafe (`vmhost/incus_acl.go`).
- No new dependency without a reason. Prefer the standard library.
- Docs and comments: plain English, no marketing.
- Do not describe a command or program that does not exist.
- Sign off commits (`git commit -s`).

## Engineering principles

Agents write and maintain all of this code, many at once, with no memory between them (an owner
decision, 2026-11-11). Make the next agent's change small, local and safe. These are concepts, not
metrics; a design regression is a review finding, not style.

1. **One concept, one home.** Search before writing a helper, rule, query or type; import or extend
   it. Copies drift apart.
2. **The producer enforces the invariant.** Validity, visibility and correction rules live where the
   data comes from, so every reader inherits them.
3. **Split by what changes together.** An entry point assembles; a feature's state, rules and
   interface live in a unit named for it. A child unreadable without its parent is no boundary.
4. **Extend through a seam.** When a new kind needs edits in N places, first build the one table or
   interface they consult; a one-caller flag is not a seam.
5. **Store facts once; derive the rest.** A record of what was decided or published is a fact.
6. **Bad states unrepresentable, missing data loud.** Validate at the boundary, then trust the type;
   no placeholder for a missing entity, no empty string for an id.
7. **Finish the change.** A move deletes the old path; a removal takes every layer that only served
   it.
8. **Refactor first, separately.** New code never goes inline in a file too big to read whole; move
   what it needs out first, in its own PR. Fixes and reverts are exempt (file the move).
9. **Comments say what is true now and why**, never history; **tests go through a public
   interface**, never source text.

Act, don't wait: fix what is wrong on your path; file what would double the PR; never stop at a
report an agent could act on. Never meet a check by moving lines without their state, trimming lines
or raising a number: each of those passes the check and keeps the defect it was there to catch.
