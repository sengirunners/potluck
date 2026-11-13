# potluck-driver-local

Built from `cmd/potluck-driver-local` (and under its old name `zm-run-local` from `cmd/zm-run-local`); the code is in `internal/localdriver`.

A run target ([driver protocol](../../docs/protocol.md); `kind = "run"`, `isolation = "none"`, `credentials = true`) that runs each job on the machine
whose daemon calls it, as the daemon's user: credentialed jobs that need that user's files and tokens, such as the
automations home's (bead reminders, sweeps, digests). Bind it on that machine; acquire is then local-only.

A zm run target that places a run on another machine of the team cannot do this: it makes the run a task of a
live session there, and a script run on the caller's own machine is refused. Here there is no zm task and no session, so a
headless Z3 acquiring on the owner's socket works.

Per run: a bare mirror of `remote` (one per remote, under `state_dir`) fetches `ref` and refuses a `commit` that
ref's history on the remote does not hold (`forbidden`), then makes a detached checkout of exactly the commit.
The script must be an executable file in it. A detached supervisor runs it there with the protocol's environment
allowlist (`PATH`, `HOME`, `LANG`, `TERM`, `TZ`) and stdin closed, writes stdout and stderr to the run's `log`,
ends it at the deadline (TERM, then KILL after 5 s; `exit: 124`) and removes the checkout. The script sets up
anything else it needs (a `PATH` with its own tools, a settings file under `HOME`).

`remote` must be the same repository as the binding's `--repo`: zm checks the commit is the head of the binding's
branch there, and the driver only that it is in `ref`'s history on `remote`. Both are the owner's settings.

Clone, fetch and checkout share a 90 s budget, inside `acquire`'s 120 s. An acquire killed while it prepared leaves
its record for a repeat of the same `request_id`, which takes it over; a refused acquire (`forbidden`,
`bad_request`) leaves nothing, so its repeat answers the same refusal. Liveness is a lock in the run's directory, never a
bare PID: `prep.lock` (the acquire preparing it), `sup.lock` (the supervisor) and `run.lock` (the supervisor, and
the script as fd 3). A lock frees when its holder dies, a crash or a reboot, so a PID is signalled only while its
lock shows it is still the run's. A supervisor gone without a verdict makes the run `lost`; one alive past the
deadline without ending it (hung) is killed by the next `status` or `list`, and the run is `expired`.

## Config (the binding's driver keys)

| key | default | meaning |
| --- | --- | --- |
| `remote` | required | the binding's repo as a git URL (the one `--repo` names) |
| `state_dir` | `~/.potluck-driver-local` (`~/.zm-run-local` where that already exists) | `mirrors/` (one bare mirror per remote) and `bindings/<target>/` (one record per handle plus each run's output, kept 7 days after the end) |
| `default_ttl_s` | 1800 | the TTL when `acquire` sends none |
| `ttl_max_s` | 14400 | lifetime cap; there is no `renew` |

```sh
zm target bind home --driver local --isolation none --credentials --repo https://github.com/acme/ops \
  --script jobs/reminders --config remote=https://github.com/acme/ops
```

## States

| run | state |
| --- | --- |
| preparing, running | `running` |
| the script exited N (128+signal if killed) | `ended`, `exit: N` |
| deadline | `expired`, `exit: 124` |
| release | `released` |
| the supervisor died without a verdict (a reboot, or a daemon restart that killed it) | `lost` |

## Check

```sh
potluck-conformance potluck-driver-local --ref main --commit <a commit with probe.sh> --repo <remote> \
  --script probe.sh --config remote=<remote>
```

`go test ./internal/localdriver/` runs the same suite on a local fixture.
