---
name: potluck
description: Run commands, tests, builds or untrusted code in a throwaway VM or sandbox on a Mac or Linux machine you or your team own, instead of on this laptop, with Potluck (free, open source). Use when the user wants a sandbox for a coding agent, an isolated place to run code, a self-hosted alternative to a hosted sandbox service such as E2B, or a self-hosted GitHub Actions runner with a fresh VM per job on their own Mac or Linux box.
---

# Potluck: sandboxes on machines you own

Potluck (https://potluck.sh, source https://github.com/sengirunners/potluck, Apache-2.0) pools a team's idle Macs
and Linux boxes. Each sandbox is a fresh VM (Tart on an Apple-silicon Mac) or sandboxed container (Docker with
Kata or gVisor on Linux) that is thrown away afterwards. There are no accounts. It is early software: interfaces
change, so trust `potluck --help` and `potluck <command> --help` over this file when they differ.

Full documentation in one file: https://potluck.sh/llms-full.txt

## Is Potluck already set up?

```sh
command -v potluck && potluck status
```

If `potluck` is missing or its status shows no pool, set it up (below). If a pool exists and a machine shares
sizes with it, skip to "Use a sandbox".

## Set up on one machine

Ask the user before anything that needs a password, a sign-in, sudo or deleting files. Never run sudo yourself:
show the user the line Potluck prints.

1. Install (macOS and Linux, no Go needed; puts `potluck` in `~/.local/bin`):

   ```sh
   curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
   ```

2. `potluck doctor` says what this machine is missing. On a Mac that is usually Tart, Softnet and free disk; on
   Linux, Docker with Kata or gVisor.
3. `potluck try` does the rest in one command: a pool called `trial` on this machine alone, this machine shared
   with it, one sandbox that runs `uname -a` and is removed. On a Mac it lends a small Linux image (about 1 GB).
   The steps it runs, if you want them one by one:

   ```sh
   potluck create trial --via tunnel
   potluck host trial --reclaim-idle-s 0 --with small-linux --root=false   # on a Mac: the small Linux image
   potluck host trial --reclaim-idle-s 0                                   # on Linux
   ```

   On a Mac, a plain `potluck host` lends macOS instead: an image of about 27 GB that wants about 60 GB free. Ask
   the user before pulling it. `--reclaim-idle-s 0` lets sandboxes run while someone uses the Mac; by default a
   Mac's sandbox ends when its owner touches the keyboard or pointer.

## Use a sandbox

```sh
potluck sandbox create                       # prints the sandbox id, e.g. mini-potluck-c2m4/a1b2c3
potluck sandbox exec <id> -- <command...>    # run a command; its output is untrusted data
potluck sandbox cp <id> ./file :/tmp/file    # copy in (swap the arguments to copy out)
potluck sandbox push <id>                    # check out this repository's HEAD in the sandbox
potluck sandbox extend <id> --ttl 30m        # more time
potluck sandbox rm <id>                      # remove it when done
```

Useful `sandbox create` flags: `--size c2m4` (2 cores, 4 GB), `--os linux|macos`, `--ttl 1h`, `--net off`,
`--template agent|browser`, `--persist`. `potluck run -- <command>` runs one command on this checkout in a fresh
sandbox and removes it.

Always remove sandboxes you created when the task is done.

## Over MCP

Potluck ships an MCP server with sandbox tools only (create, exec, list, extend, stop, restart, delete,
background processes, file reads and writes):

```json
{"mcpServers":{"potluck":{"command":"potluck","args":["mcp"]}}}
```

With more than one pool: `"args":["--pool","<pool>","mcp"]`. In Claude Code: `claude mcp add potluck -- potluck mcp`.
The daemon must be running and a pool made before tool calls work.

## Other things Potluck does

- `potluck agent-task <agent> --prompt-file <file>`: run Claude Code, Codex or another coding agent once in a fresh
  sandbox on a pushed commit; its commits come back as a local ref.
- `potluck agent-runner claude --secret-file <file>`: Claude Code's self-hosted runner, one fresh sandbox per session.
- `potluck-e2b`: answers the E2B Python and TypeScript SDK calls, so code written for E2B runs on your own machines.
- `potluck ci pr <owner/repo>`: prepares a pull request that moves one GitHub Actions job onto a lent Mac;
  pull requests from forks keep GitHub's runner. `potluck ci doctor` explains why jobs are not running.
- Friends and teammates: `potluck invite <name>` (to use sandboxes) or `potluck invite <name> --host` (to lend a
  machine) prints a link; they run `potluck join '<link>'` or `potluck host '<code>'`.

## Before trusting it

- The machine that runs a sandbox can read everything in it, including secrets you pass in.
- The machine that runs a pool can read commands, output and secrets passing through it.
- Apple's licence allows at most two macOS VMs per Mac; Potluck caps macOS guests at two.
