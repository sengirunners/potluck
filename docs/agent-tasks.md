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

## A task

A task is `potluck run`'s box job with an agent in it:

1. Before anything is placed, the prompt and the credential are read. No credential refuses the task, naming
   `claude setup-token`; nothing is leased.
2. Placement, as `potluck run`, among box bindings whose driver runs a background process with secrets of its own
   (verb `spawn`, capability `secrets`), from the adapter's template (Claude: `agent`) unless `--image` names
   another of the machine's images. The box is acquired with `net` `on`.
3. The commit (default HEAD; `--ref`, `--dirty`, `--include-untracked`, `--full`, `--dir` as for `potluck run`) goes
   into the box as a git bundle and is checked out at `~/checkout`.
4. A probe checks the box has the agent's command on PATH (Claude: `claude`) and refuses the task if not, before
   the agent starts. Potluck installs nothing: the image must ship it.
5. The prompt is written to `~/.potluck/agent-task/<job>/prompt`, through `exec` (as `potluck sandbox cp` copies a
   file).
6. The entry script (`agenttasks.Entry`) is started with `spawn`, with your credential as that one command's secret.
   It exports the credential from its file under `$POTLUCK_SECRETS` into the agent's environment, runs the agent in
   the checkout with the prompt on its stdin, and then, with the credential unset:
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
   off by the timeout or Ctrl-C brings no commits back. An agent that exits 124 itself before `--timeout` is up
   is not the timeout: its commits come back. When bringing the commits back fails (the copy, the check or the
   fetch, Ctrl-C during it included), the box is kept instead, so the agent's work is not lost, and the error
   names the `potluck sandbox cp` and `potluck sandbox rm` commands for it. It still ends at its lease (`potluck
   sandbox extend` gives it longer).

`potluck agent-task` exits with the agent's exit code. When the agent exited 0 but its commits did not come back,
it exits 1 and says why. With `--json`, stdout is one object: `potluck run`'s
fields (`handle`, `target`, `commit`, `pid`, `state`, `exit`, `error`, `kept`, `released`, `seconds`) and `agent`,
`ref` (absent when there are no commits), `commits` (how many past `commit`) and `last` (the agent's last 20
lines of stdout); the agent's output then goes to stderr.

Flags: `--prompt-file` (required; `-` reads stdin; at most 1 MiB), `--secret-file`, `--secret-name`, `--timeout`
(default 1h), `--keep`, placement's `--on`, `--size`, `--image`, and the push flags above.

## The credential

- It is yours: `--secret-file` is read on your machine (a regular file, at most 64 KiB), else the environment:
  Claude takes `$CLAUDE_CODE_OAUTH_TOKEN` (what `claude setup-token` prints), else `$ANTHROPIC_API_KEY`.
  `--secret-name` says which one a file holds (default the first); without a file it names the one variable read.
- It goes with the `spawn` alone, through the secrets door like any `exec` secret (`docs/daemon.md`, "Secret
  grants"): checked against the pool's grants, sealed between daemons, and sent only to a `trusted` host. With the
  pool's default grants off, the pool needs a `value` grant at scope `exec` for its name, for example
  `potluck secrets grant add --name CLAUDE_CODE_OAUTH_TOKEN --role member`.
- It is in no argv, no file Potluck writes and no environment but the agent's. No daemon keeps it.

## Trust

- **The machine that runs the box can read everything in it**: your code, the prompt, the credential, and what the
  agent does and prints.
- **The agent can read the credential**: it is in its environment, and the box has a network. Use a credential you
  can revoke, and revoke it after a task you suspect.
- The pool's machine can read the credential too, as for any `exec` secret (sealed hop by hop, not end to end).
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
template whose image ships it, the credential names it reads from its environment (the default first), its argv
(starting with its command; the prompt comes on stdin) and the command a person runs to get a credential. Add it to
the table; `TestAdapters` checks it. No daemon or driver change is needed.

## Not built

- More adapters (Codex), each one more row.
- A GitHub token in the box, so the agent could push or open a pull request itself: you push the ref.
- Running a task from a daemon, so it goes on while your machine sleeps.
