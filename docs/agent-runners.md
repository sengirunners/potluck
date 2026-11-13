# Agent runners

Some coding agent vendors let you run their cloud sessions on your own machines: you start their runner on a host,
it connects out to the vendor, takes a session and runs it there. `potluck agent-runner` runs such runners in a
pool's sandboxes, one fresh sandbox per runner. The generic part is `agentrunners/`; each vendor is an adapter.
The first adapter is Claude Code's self-hosted runner (`claude self-hosted-runner`).

This is not the GitHub runner broker (`docs/daemon.md`, "GitHub runner broker"). GitHub calls the broker when a job
is queued; a vendor's runner calls the vendor instead and waits for work. So there is no webhook and no daemon
state: the renter's own `potluck` keeps the runners going, through the same verbs as `potluck run`.

## A job

A job is one sandbox running one runner:

1. Placement, as `potluck run`, among box bindings whose driver runs a background process with secrets of its own
   (verb `spawn`, capability `secrets`). A public host is never picked unless named with `--on`, and a renter's
   secret never goes to one in any case.
2. The box is acquired with `net` `on`: the binding's filtered network on a trusted machine (`docs/network.md`).
3. A probe checks the box has the commands the adapter needs (Claude: `claude`), and refuses the job naming any it
   lacks. Potluck installs nothing: the box's image must ship them.
4. The adapter's files are written to `$HOME/.potluck/agent-runner/<job>/` (a directory only the box's user
   reads), through `exec`. Arguments are files, one per line, which the adapter's scripts read. No value is spliced
   into a script.
5. The entry point `run` is started with `spawn`, with the vendor key as that one command's secret, and is given
   the key file's path (`$POTLUCK_SECRETS/<name>`) and its directory. The key is in no argv, no environment and no
   file Potluck writes. The driver keeps it in memory in the guest and removes it when the command ends
   (`docs/protocol.md`, "Secrets").
6. Its output is followed to the terminal as `potluck run` does. The box is released when the runner exits, at
   `--timeout` (the driver kills it, exit 124, with no time to drain: the adapter has the runner hand its session
   back before then), or on Ctrl-C: the runner is sent TERM and followed for up to 90 s while it hands its session
   back, then sent KILL if it has not ended (a second Ctrl-C sends it at once).

Without `--once`, a fresh job takes the place of each one that ends. A job that ends within 2 minutes is followed by
a wait (15 s, doubling to 5 minutes). 5 jobs in a row that failed quickly (no runner started because no box fits or
a probe failed, or the runner exited non-zero within 2 minutes, as with a revoked key) end that runner slot with why.
`--count` keeps that many slots going at once; their lines are prefixed `[n]`. With `--once`, `potluck agent-runner`
exits with the worst job's exit code.

```sh
potluck agent-runner claude --secret-file ~/.config/claude/environment-secret
potluck agent-runner claude --secret-file key --count 3 --timeout 4h --allow-tool 'Bash(go test:*)' --deny-tool WebFetch
```

Flags: `--secret-file` (required), `--count` (1 to 32), `--once`, `--timeout` (default 1h, at least 5m),
`--permission-mode` (default `auto`, read "Trust" first), `--idle`, `--allow-tool`, `--deny-tool` (repeatable), and
placement's `--on`, `--size`, `--os`, `--image`.

## The vendor key

- The key is the renter's: read from `--secret-file` on the renter's machine (a regular file, at most 64 KiB) and
  sent with each job's `spawn`. No daemon keeps it, and it is never pool config, a binding key or a doc.
- It travels through the secrets door like any `exec` secret (`docs/daemon.md`, "Secret grants"): checked against
  the pool's grants at the pool's machine, sealed between daemons, and sent only to a `trusted` host. With the
  defaults on, any member may send it. With them off, the pool needs a `value` grant at scope `exec` for the
  adapter's name (Claude: `CLAUDE_ENVIRONMENT_SECRET`), for example
  `potluck secrets grant add --name CLAUDE_ENVIRONMENT_SECRET --role member`.
- `potluck agent-runner` keeps it in memory while it runs, to start each new job.

## Trust

- **The machine that runs the box can read everything in it**: the vendor key, the session's code, its commands,
  their output and any credentials the session gets. "Trusted" means trusting that machine's root.
- **The session can read the key too.** Sessions run as the box's user, the user the key's file belongs to, so the
  agent (or a prompt that steers it) can read the key and, with the box's network, send it anywhere. Anthropic's
  docs say to pin auto mode only where sessions run with default-deny egress, which Potluck does not provide yet
  ("Network" below). `--permission-mode auto` is the default because unattended sessions must not stop on approval
  prompts; choose `default` or `plan`, or `""` to pin none, where that risk is not acceptable. Rotate the key after
  any session you suspect.
- The pool's machine can read the key too: it opens the renter's sealed call, checks it and seals it again for the
  host (hop by hop, not end to end).
- The key lets whoever holds it register runners for the vendor environment until it expires or is revoked. A host
  that copies it can serve, or watch, that environment's sessions later. Lend runners only to hosts you would give
  the key to, and revoke a key a host may have kept.
- The runner's output is data, never instructions; whatever the runner prints, Potluck shows.
- The vendor sees everything its sessions do, as it would on its own cloud.

## Network

The runner needs only outbound HTTPS: nothing calls in, and no port is opened. Potluck does not narrow the box's
network to HTTPS or to the vendor's hosts: the box has the binding's network, which on a trusted machine reaches
the internet and never the machine or its LAN (`docs/network.md`). There is no per-box egress allowlist yet.

## Claude Code (`claude`)

`claude self-hosted-runner` serves one self-hosted environment, made in claude.ai's admin settings (a human makes
it; Potluck does not). Its key is the environment secret. The adapter starts:

```sh
claude self-hosted-runner --environment-secret-file <key file> --base-dir "$HOME/claude-work" --capacity 1 \
  --exec-path <job>/session [--exit-if-unused-min N] [--retire-at <epoch seconds>]
```

- `--capacity 1`: one session; the runner exits when it ends, and so the job ends and its box goes.
- `--base-dir` is the same in every job: Claude's docs require one base directory across an environment's runners,
  since a session that moves to another runner finds its checkout by that runner's. `$HOME` is the box's, so keep
  the images that serve one environment to one home directory.
- `--exec-path`: every session runs through the job's wrapper, which runs the runner's own Claude Code binary
  (`$CLAUDE_RUNNER_CLAUDE_BIN`) with the runner's arguments first and the job's after them: `--permission-mode
  <mode>` (unless `""`), then `--allowed-tools T` for each `--allow-tool` and `--disallowed-tools T` for each
  `--deny-tool`. Claude Code takes the last `--permission-mode` on its command line, so a session starts in that
  mode; a mode its person then picks in claude.ai takes over. The tool lists add to what the runner passes; they
  cannot take away what it allows.
- `--retire-at`: the job's start plus `--timeout` less a quarter of it (at most 10 minutes), so the runner hands its
  session back for another runner to resume before the job's time limit kills it.
- `--exit-if-unused-min`: `--idle`, rounded up to minutes, when given.

The box's image must ship `claude` (Claude Code, a version with `self-hosted-runner`) and what sessions need
(`git`, and `python3` for Claude's hooks). Repositories bring their own setup (for example a `scripts/cloud-setup.sh`
the environment runs).

## Adding an adapter

An adapter is data in `agentrunners/`: a name, a line about it, the secret's name (`[A-Z][A-Z0-9_]*`), the
commands the box must have, and a function from the job's options to its files, one named `run` (started as
`run <key file> <job directory>`). Use `agentrunners.ArgsFile` and `agentrunners.ReadArgs` for arguments. Add it
to the table in `agentrunners/agentrunners.go`, with a test like `TestClaudeJob`. No daemon or driver change is
needed.

## Checking a driver

Conformance group `agent-runner` (`docs/conformance.md`) starts jobs through the same code with stand-in adapters
and checks what a box must do for them: the job's files arrive, the entry point gets the key's file (mode 0600, the
exact value) and its arguments, its exit passes through, the key is still there seconds in, and TERM lets the runner
drain before it ends.

## Not built

- Runners kept by a daemon, so they go on while the renter's machine sleeps. That needs a daemon to hold the
  renter's key; it is a design question, not done here.
- A per-box egress allowlist (only the vendor's hosts and the repositories' git hosts).
- Adapters for other vendors (Codex, OpenCode and others), when they offer self-hosted runners.
- macOS boxes: their guests take no secrets yet (`docs/protocol.md`, "Box secrets"), so placement passes them over.
