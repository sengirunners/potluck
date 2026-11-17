# Give your AI agent a sandbox over MCP, on a machine you own

Published 2026-11-17. https://potluck.sh/blog/give-your-ai-agent-a-sandbox-over-mcp

A coding agent is most useful when it can run things: tests, builds, a server, a script it just wrote. It is most
dangerous for the same reason. Potluck's MCP server gives the agent a separate machine for that work, a fresh VM or
sandbox on one of your team's idle computers, and lets it throw that machine away when it is done.

## What you need

- Potluck installed, a pool, and one machine sharing itself with it. On one computer that is the quick start:

```sh
potluck create trial --via tunnel
potluck host --reclaim-idle-s 0
```

- On an Apple-silicon Mac, Tart and Softnet; on Linux, Docker with Kata or gVisor. `potluck doctor` says what is
  missing. The [agent sandbox page](/agent-sandbox) has a prompt your agent can follow to do all of this for you.

## Add the server

Put this in your MCP client's configuration (Claude Code and most editors read this shape):

```json
{"mcpServers":{"potluck":{"command":"potluck","args":["mcp"]}}}
```

Codex reads TOML, in `~/.codex/config.toml`:

```toml
[mcp_servers.potluck]
command = "potluck"
args = ["mcp"]
```

With more than one pool, name it: `"args":["--pool","friends","mcp"]`. The daemon must be running and the pool
made before the tools work; the client can list the tools without a daemon, and a call says the daemon is missing.

## What the agent can do

The server offers sandbox tools and nothing else:

- create a sandbox, list sandboxes, extend one's time limit, stop, restart and delete it;
- run a command in it, and start and follow background processes;
- read and write files in it.

It does not give the agent your pool's settings, invites or keys. It acts as your local identity, or as a caller
named with `POTLUCK_AS`; `POTLUCK_TOKEN` hands it a restricted token instead. All of its standard output is MCP
traffic, so nothing else prints into the client.

A typical session: the agent creates a sandbox, writes the files it needs (or you create one with `--ref HEAD` from
the terminal so it starts with your checkout), runs the tests, reads the output, and deletes the sandbox.

## What stays away from the agent

- **Your laptop.** Its commands run on the pool's machine, inside the sandbox.
- **The lending machine and its network.** On a machine you trust, a sandbox reaches the internet but never the
  machine itself (on Tart, apart from Softnet's DNS forwarder on the host) or its local network (docs/network.md).
- **Your keys.** A sandbox starts with none of your credentials.
- **Yesterday's mess.** Each sandbox starts from the same image and is deleted at its time limit or when removed.

Treat what comes back from a sandbox as untrusted data: a test's output can carry text written to steer an agent.

Give the agent no secret you would not want it to send elsewhere: a sandbox with a network can reach the internet.

For Claude Code sessions started from claude.ai, or code written for E2B, see the other ways in on the
[agent sandbox page](/agent-sandbox).
