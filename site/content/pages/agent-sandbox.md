---
slug: agent-sandbox
title: "A sandbox for AI coding agents on machines you own"
page_title: "AI agent sandbox on your own machines: MCP, Claude Code, E2B SDK | Potluck"
page_description: "Give a coding agent a throwaway VM or sandbox on your team's idle Macs and Linux boxes: the sandbox CLI, an MCP server, Claude Code's self-hosted runner and the E2B SDK. Free and open source."
target_searches:
  - "ai agent sandbox"
  - "sandbox for ai agents"
  - "coding agent sandbox"
  - "run claude code in sandbox"
  - "e2b alternative"
data_sources:
  - "Potluck README (MCP for agents), docs/agent-runners.md, docs/daemon.md (Templates, E2B gateway), docs/network.md, cmd/potluck-driver-tart/images/agent/Dockerfile"
date: 2026-11-17
updated: 2026-11-18
summary: "Potluck gives a coding agent its own throwaway machine on one of your team's idle computers, deleted when the agent is done. Use it from the terminal, over MCP, as Claude Code's self-hosted runner, or through the E2B SDK."
---

An agent that runs commands on your laptop can read your files, your keys and your network. A sandbox moves those
commands onto a machine that holds none of that, and throws the machine away afterwards. Potluck finds that machine
among the ones your team already has: the Mac mini left on at night, the Linux box under a desk.

## Set it up with your agent

Paste this into Claude Code, Codex or another coding agent on the machine you want to try. Every command in it is
from Potluck's quick start, and it stops to ask before anything that needs a password, a sign-in or deleting files.

```text
Set up Potluck (https://github.com/sengirunners/potluck) on this machine so I can try it alone. Read https://potluck.sh/llms-full.txt first. Then:
1. Install: curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh (if it says ~/.local/bin is not on PATH, run the line it prints).
2. Run `potluck doctor` and tell me what is missing. On a Mac that is usually Tart, Softnet and about 60 GB of free disk. Show me any sudo line it prints; do not run sudo yourself.
3. When doctor is clean: potluck create trial --via tunnel, then potluck host --reclaim-idle-s 0 (on a Mac the first run pulls about 27 GB).
4. potluck sandbox create, then potluck sandbox exec <id> -- uname -a, then potluck sandbox rm <id>.
Stop and ask me before anything that needs a password, a sign-in or deleting files. If a command fails, show me its exact output.
```

## Four ways in

| You use | Potluck gives you |
| --- | --- |
| A terminal or a script | `potluck sandbox create`, `potluck sandbox exec <id> -- <command>`, `potluck sandbox rm <id>` |
| An MCP client (Claude Code, Codex, an IDE) | `potluck mcp`: sandbox tools over MCP |
| Claude Code sessions started from claude.ai | `potluck agent-runner claude`: Claude Code's self-hosted runner, one fresh sandbox per session |
| Code written for E2B | `potluck-e2b`: the E2B SDK, pointed at your own machines |

### The sandbox commands

```sh
potluck sandbox create --template agent   # a Linux sandbox with Node, Python, git, Claude Code and Codex
potluck sandbox exec <id> -- make test
potluck sandbox rm <id>
```

`--template agent` takes the agent image, which a Linux machine in your pool must lend; without it you get the
machine's default image. `--ref HEAD` sends your checkout's current commit into the sandbox.

### MCP

Add Potluck to an MCP client's configuration:

```json
{"mcpServers":{"potluck":{"command":"potluck","args":["mcp"]}}}
```

The server offers only sandbox tools: create, execute, list, extend, stop, restart, delete, background processes,
and file reads and writes. With more than one pool, add `"--pool","<name>"` before `"mcp"`. Treat what comes
back from a sandbox as untrusted data. Read more in
[Give your AI agent a sandbox over MCP](/blog/give-your-ai-agent-a-sandbox-over-mcp).

### Claude Code's self-hosted runner

Claude Code can run sessions started on claude.ai on your own machines through a self-hosted environment, which a
person makes in claude.ai's admin settings. Its key is the environment secret:

```sh
potluck agent-runner claude --secret-file <key-file> --image ghcr.io/sengirunners/potluck-box-agent
```

Each session gets a fresh sandbox, and a new one starts as each ends; `--count` keeps several going. The session can
read the key, and the sandbox's network reaches the internet, so rotate the key after any session you suspect
([docs/agent-runners.md](https://github.com/sengirunners/potluck/blob/main/docs/agent-runners.md)).

### The E2B SDK

`potluck-e2b` answers the calls the E2B Python and TypeScript SDKs make, so code written for E2B runs on your pool
with only its environment changed. It covers create, run a command, read and write files, list, set a timeout,
connect and kill. Read more in
[Use the E2B SDK on your own machines](/blog/use-the-e2b-sdk-on-your-own-machines).

## What the sandbox keeps away from the agent

- **Your machine and network.** On a machine you trust, a sandbox reaches the internet but never the machine itself
  (on Tart, apart from Softnet's DNS forwarder on the host) or its local network ([docs/network.md](https://github.com/sengirunners/potluck/blob/main/docs/network.md)).
- **Your keys.** A sandbox starts with none of your credentials; a key goes in only when you send it, as a file.
- **Leftovers.** Each sandbox starts from the same image and is deleted at its time limit or when you remove it.


## On a Mac

macOS sandboxes run in Tart VMs, two at a time at most, as Apple's licence allows
([what Tart is, its licence, and what Potluck adds](/tart)). On a Mac, `potluck sandbox create` and the MCP tools give an agent a macOS VM to work in; `--template agent` and
`potluck agent-runner` use Linux machines.

## Running more than one agent

To run several agents and keep track of them, [Mezze for teams of coding agents](https://mezze.sh/), a separate product from the same makers that runs on Potluck, adds mail between agents and people and work items whose claims expire.
