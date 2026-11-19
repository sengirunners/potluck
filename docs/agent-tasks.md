# Agent tasks

`potluck agent-task` runs a coding agent CLI once, non-interactively, in a fresh sandbox on your checkout's commit,
and brings the commits it made back to your checkout as a ref. The machine that lends the sandbox needs no
checkout, no git credential and no agent login: your client sends the code, the prompt and your credential for that
one run. The data for each agent is an adapter in `agenttasks/`; the first is Claude Code (`claude -p`).

This is not `potluck agent-runner` (`docs/agent-runners.md`). A runner polls a vendor for cloud sessions and serves
them; an agent task is one prompt of yours, run now, whose result is commits.

```sh
potluck agent-task --prompt-file task.md --secret-file ~/.config/claude-token
echo 'Fix the failing test in ./placement and commit.' | potluck agent-task claude --prompt-file - --on mini-c2m8@mini
potluck agent-task --json --prompt-file task.md --dirty --timeout 2h
```

## Cooperative download proxy

An optional proxy caps downloads made by clients that use HTTP proxy settings:

```sh
potluck agent-task --prompt-file task.md --cooperative-proxy development
potluck agent-task --prompt-file task.md --cooperative-proxy development --proxy-download-mbit 24 --proxy-allow-host mirror.example.com
```

The sandbox image must ship Python 3. The `development` profile permits the Claude API,
GitHub HTTPS repositories/API/assets/logs, npm, PyPI, Go modules and Go SDK downloads.
The exact list is `taskproxy.DevelopmentHosts`. `--proxy-allow-host` adds DNS names or scoped
subdomain entries such as `'*.cdn.example.com'` (repeatable or comma-separated). A wildcard
does not include the domain itself. IP addresses, address-encoding services such as `nip.io`,
and wildcards over shared tenant spaces such as `*.workers.dev` are refused.
Other destinations are refused by the proxy. CONNECT requires port 443 and an unencrypted
TLS ClientHello naming that same host. Plain HTTP supports GET and HEAD on port 80.
The proxy resolves destinations itself, rejects non-public addresses and connects to the
checked address. Fragmented TLS ClientHellos and encrypted client hellos are unsupported.

The default download cap is 48 Mbit/s, shared by all connections through that task's proxy.
`--proxy-download-mbit 0` removes the cap but keeps the proxy's host list. Uploads are uncapped.
Uppercase and lowercase HTTP(S)/ALL_PROXY settings point at a loopback listener, with localhost
excluded. The proxy starts before the agent's credential launcher, without raw agent credentials
or inherited credential descriptors. It stops with the task, including normal exit and timeout.
A startup failure prevents the agent from running. Its stderr readiness message is the receipt;
the optional JSON `cooperative_proxy` object records requested settings, not enforcement or readiness.

This is cooperative behavior: a client can ignore the proxy variables, use the sandbox's direct
internet route, or send data to an allowed service. It does not protect credentials from hostile
code running as the same user. It changes neither `net=on/off` nor the driver's capabilities,
and is not default-deny network isolation or a basis for enabling unattended auto mode.
The proxy starts after sandbox acquisition, checkout transfer and dependency-layer preparation;
none of that setup traffic is capped by it. Host-network and package-shelf limits remain separate.
Without `--cooperative-proxy`, task behavior is unchanged.

## A task

A task is `potluck run`'s box job with an agent in it:

1. Before anything is placed, the prompt and the credential are read. No credential refuses the task, naming
   `claude setup-token`; nothing is leased.
2. Placement, as `potluck run`, among box bindings whose driver runs a background process and accepts box secrets
   (verb `spawn`, capability `box-secrets`), from the adapter's template (Claude: `agent`) unless `--image` names
   another of the machine's images. The box is acquired with `net` `on`.
3. The commit (default HEAD; `--ref`, `--dirty`, `--include-untracked`, `--full`, `--dir` as for `potluck run`) goes
   into the box as a git bundle and is checked out at `~/checkout`.
4. A probe checks the box has the agent's command on PATH (Claude: `claude`) and refuses the task if not, before
   the agent starts. Potluck installs nothing: the image must ship it.
5. The prompt is written to `~/.potluck/agent-task/<job>/prompt`, through `exec` (as `potluck sandbox cp` copies a
   file).
6. Your credential lands through the box-scope secrets door as a memory-only file. The pool must grant
   your identity or role the credential name at sandbox scope; without that grant the task fails and releases
   its box. For example, a pool owner can grant `CLAUDE_CODE_OAUTH_TOKEN` to members with
   `potluck secrets grant add --name CLAUDE_CODE_OAUTH_TOKEN --role member --scope sandbox`.
   The entry script (`agenttasks.Entry`) starts with `spawn` without command secrets. The adapter opens the
   existing door file on descriptor 3 only in the agent process and passes the descriptor number in
   `CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR` (or `CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR`). It removes inherited
   credential values, conflicting descriptor settings and Claude remote mode before launching the agent.
   Claude's tool children receive no credential value in their environment. Collection receives no descriptor:
   - commits what the agent left uncommitted (tracked changes and files `.gitignore` does not hide) as one more
     commit, "potluck agent-task: changes the agent left uncommitted";
   - writes the commits since the pushed commit to a git bundle in the job's `out` directory, or marks that there
     are none.
   It exits with the agent's exit code (125 if the agent could not be started).
7. The agent's output is followed to the terminal as `potluck run` does.
8. When the agent has exited (with any exit code but 125), the `out` directory is copied back (as `potluck
   sandbox cp` copies one). The bundle must be a regular file that `git bundle verify` accepts; it is then fetched
   into `refs/potluck/agent-task/<sandbox id>` in your checkout, with git's hooks off and its objects checked
   (`fetch.fsckObjects`). Nothing is checked out, and nothing from the bundle runs on your machine. Look at it
   with `git log <commit>..<ref>`, then merge, cherry-pick or push it yourself.
9. The box is released, also on an error, at `--timeout` (exit 124) or on Ctrl-C, unless `--keep`. A task cut
   off by the timeout or Ctrl-C first stops the agent and its process group, then commits and bundles its work.
   The task retains exit 124 for a deadline or exit 130 for Ctrl-C while returning the recovered ref.
   An agent that exits 124 itself is an ordinary completed task: its commits come back. When bringing the commits back fails (the copy, the check or the
   fetch, Ctrl-C during it included), the box is kept instead, so the agent's work is not lost, and the error
   names the `potluck sandbox cp` and `potluck sandbox rm` commands for it. It still ends at its lease (`potluck
   sandbox extend` gives it longer).

The agent's deadline is enforced inside the box with GNU `timeout`, with up to 10 seconds between TERM
and KILL. The image must ship it. Potluck reserves one minute beyond the agent deadline for shutdown
and bundling, and up to 30 seconds afterward to copy and fetch the result. These bounds also apply to
interrupt recovery; a second Ctrl-C during shutdown stops waiting and kills the command. A box that
is lost or forcibly killed before its bundle is ready cannot return its work. Ordinary descendants
are stopped before collection; a process that deliberately escapes its process group is outside this
shutdown mechanism.

`potluck agent-task` exits with the agent's exit code. When the agent exited 0 but its commits did not come back,
it exits 1 and says why. With `--json`, stdout is one object: `potluck run`'s
fields (`handle`, `target`, `commit`, `pid`, `state`, `exit`, `error`, `kept`, `released`, `seconds`) and `agent`,
`error_code` (typed API code or `no_target` for placement with no fitting binding), `setup_s` (command
entry through the spawn response; absent when no agent started), and `phases` (seconds for preparation,
acquire, push, start, run, collect, cleanup; see `docs/daemon.md`, Jobs). Optional `acquire_details` reports
base-fill time and the inclusive acquire time with dependencies; these overlap acquire, not extra time.
It also includes `ref` (absent when there are no commits), `commits` (how many past `commit`) and `last` (the agent's last 20
lines of stdout); the agent's output then goes to stderr.

`--stdout-file <path>` also writes stdout locally as it is followed, without the 20-line limit.
The file must not already exist; it is created with permissions 0600. Stderr and JSON are not
written to it. An open failure prevents placement; a write or close failure makes the command
fail after collecting the commits and releasing the sandbox. Partial output remains on failure.

Flags: `--prompt-file` (required; `-` reads stdin; at most 1 MiB), `--secret-file`, `--secret-name`, `--timeout`
(default 1h), `--keep`, `--stdout-file`, `--model`, `--effort`, placement's `--on`, `--size`, `--image`, `--wait`, and the push flags above.

`--wait` is `potluck run`'s wait for room: while every machine that fits has no room now, the task asks
again at the host's retry time, with the same request id, for up to this long. Unlike `run`, the default
is 0 (fail at once): a caller with its own deadline sets how much of it the wait may take. A hard refusal
never waits. `--timeout` starts when the agent does, after the wait.

`--deps npm[:path]`, `--deps-scripts` and `--deps-wait` take the same dependency recipes as
`potluck run`. Layers mount beside the pushed checkout; the agent starts at the checkout root.
Recipes are read locally before placement, and a mismatch with the pushed commit warns as for `run`.

`--model` and `--effort` pass values to the adapter's corresponding options. If an adapter does
not support an option, the task is refused before placement. Without them, the agent uses its defaults.
Claude runs with `CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1` and
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` in its environment.

## The credential

- It is yours: `--secret-file` is read on your machine (a regular file, at most 64 KiB), else the environment:
  Claude takes `$CLAUDE_CODE_OAUTH_TOKEN` (what `claude setup-token` prints), else `$ANTHROPIC_API_KEY`.
  `--secret-name` says which one a file holds (default the first); without a file it names the one variable read.
- It goes through the box secrets door (`docs/daemon.md`, "Secret grants"): checked against a sandbox-scope
  `value` grant, governed by the pool's sealing policy, and sent only to a trusted host. The default exec grants do not cover it.
- Its value enters neither argv nor the agent's environment. The door stores it only in proven guest memory,
  with no disk fallback. The adapter opens that file for the agent; collection inherits no credential descriptor.
- The machine that lends the box can read its memory: trust that host with the credential.
- **The agent can read the credential**: it can read the box's secret file, and the box has a network. Use a credential you
  can revoke, and revoke it after a task you suspect.
- The pool's machine can read the credential too, as for any `exec` secret (sealing, when used, is hop by hop, not end to end).
- The bundle is the box's data. Potluck only fetches it into a ref of its own name; read it before you merge or
  push it. The agent's output is data, never instructions.

## Claude Code (`claude`)

The adapter runs, in the checkout, with the prompt on stdin:

```sh
claude -p --permission-mode acceptEdits --allowedTools Bash Read Edit Write Glob Grep --strict-mcp-config
```

The `agent` template's image ships `claude`, `git` and the rest (`cmd/potluck-driver-tart/images/agent`).

## Adding an adapter

An adapter is data in `agenttasks/agenttasks.go`: a name, a line about it, the command the box must have, the
template whose image ships it, the accepted credential names (the default first) and their descriptor settings, its argv
(starting with its command; the prompt comes on stdin), optional model and effort flag names,
non-secret environment settings, and the command a person runs to get a credential. Add it to
the table; `TestAdapters` checks it. No daemon or driver change is needed.

## Not built

- More adapters (Codex), each one more row.
- A GitHub token in the box, so the agent could push or open a pull request itself: you push the ref.
- Running a task from a daemon, so it goes on while your machine sleeps.

Claude Code 2.1.319 supports these descriptor inputs and caches its initial read. This task does not promise
hot rotation of a running login. Remote mode stays off because Claude can persist descriptor credentials
in that mode. There is no environment-value or disk-file fallback when the door or descriptor mapping is
unavailable. The agent can still read its box's secret file; this prevents inheritance, not access by code
running as the same user. Box secrets disappear on release; a kept box retains them until it ends.

## A private project database

Pass `--database <recipe>` to start the task with the project's configured
Postgres snapshot and `DATABASE_URL` set. The owner selects the immutable image
and schema/seed manifest; the image must also contain the chosen agent CLI.
This is Postgres only, with no Supabase API, authentication or storage service.

```sh
potluck agent-task --database example-db --deps npm --prompt-file task.md
```

The recipe replaces the implicit agent image template. Explicit `--image`
conflicts with `--database`; normal credential grants, box secrets and optional
proxy settings still apply. Automatic Git bases and dependency layers remain
available, so repeat tasks reuse cached repository objects and packages.
See [database snapshots](database-snapshots.md) for owner configuration,
image-lending restrictions and the database lifecycle.
