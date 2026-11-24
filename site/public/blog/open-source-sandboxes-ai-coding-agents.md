# Open source sandbox for AI coding agents, self-hosted (2026): Claude Code sandboxing and your own hardware

Published 2026-11-24. https://potluck.sh/blog/open-source-sandboxes-ai-coding-agents

**Short answer:** In November 2026 you have two layers to choose from. The first is the sandbox built into Claude Code. It is off by default; you turn it on with `/sandbox`. It confines only the shell commands Claude runs: writes are limited to the working directory, and network access goes through a proxy that checks each host against an allowlist. The second layer puts the whole agent, or the commands it runs, inside a container or VM. Open-source tools for that second layer include Anthropic's sandbox runtime, OpenSandbox (Apache-2.0, on Docker or Kubernetes) and Potluck (Apache-2.0, free, early). Potluck runs each sandbox in a fresh VM or sandbox on Macs and Linux boxes your team already owns. Potluck is made by Sengi, which sells hosted GitHub Actions runners; Potluck itself is a separate free project.

## The options compared

| Option | What it isolates | Where it runs | Open source | Where it costs more or needs more work |
|---|---|---|---|---|
| Claude Code Bash sandbox | Shell commands and their child processes | Your own machine: Seatbelt on macOS, bubblewrap on Linux and WSL2 | Built on Anthropic's open-source sandbox runtime | File tools, MCP servers and hooks run outside it; reads most of your disk by default |
| Anthropic sandbox runtime around the whole process | The Claude Code process itself | Your own machine | Yes | You write and maintain the filesystem and network policy |
| Dev container or local VM | Everything inside the container or VM | Your own machine | Depends on the tool | Uses your laptop's cores and RAM; setup per project |
| OpenSandbox | Agent commands, code interpreters, browsers | Your servers on Docker or Kubernetes; gVisor, Kata or Firecracker | Yes, Apache-2.0 | You run and scale the servers |
| E2B, Daytona | Agent code in hosted sandboxes | The vendor's cloud, with an open-source core | Partly | Pay as you go; code runs on someone else's machines |
| Potluck | Each job in a fresh VM or sandbox | Spare Macs (Tart) and Linux boxes (Docker or Podman with Kata or gVisor) on your team | Yes, Apache-2.0 | Early; the host machine can read what runs on it |

The hosted services do well when you want no machines to look after. The self-hosted options do well when you already have hardware and want code to stay on it.

## What the Claude Code sandbox restricts

Anthropic's Claude Code documentation describes the Bash sandbox as a boundary that the operating system enforces around shell commands. On macOS it uses Seatbelt, which ships with the system. On Linux and WSL2 it needs bubblewrap and socat from your package manager. On native Windows, commands run unsandboxed, so the docs point Windows users to WSL2.

With it on, by default:

- **Writes** go only to the working directory, a per-user temp directory and directories you added. Claude Code's own settings, hooks directories and shell startup files stay write-protected.
- **Reads** cover most of the machine, including `~/.ssh` and `~/.aws/credentials`. You close that with `sandbox.filesystem.denyRead` or the `sandbox.credentials` block.
- **Network** has no direct route out. Traffic goes through a local proxy that checks each hostname against `allowedDomains`, which starts empty. Tools that ignore proxy variables, such as plain `ssh` and most database drivers, cannot connect at all.
- **Environment variables** are inherited, secrets included, unless you deny or mask them.

To turn it on, run `/sandbox` in a session and pick auto-allow or regular permissions. To turn it on everywhere, set `sandbox.enabled` to `true` in `~/.claude/settings.json`. To check it works, ask Claude (not the `!` prompt) to run `touch ~/sandbox-probe`; inside the sandbox it should fail.

## What runs outside it

This is the part people miss. The Claude Code docs list what the Bash sandbox does not cover:

- The built-in Read, Edit, Write, WebFetch and WebSearch tools, which follow permission rules instead.
- Hooks, local MCP servers, LSP servers and helper commands, which run with your full access.
- Commands you type yourself at the `!` prompt, in most sessions.
- Commands matched by `excludedCommands`, and commands Claude retries unsandboxed after a failure. Setting `allowUnsandboxedCommands` to `false` turns that retry off.

The docs also say plainly that the sandbox reduces risk but is not a complete isolation boundary, and that the proxy does not inspect TLS by default. Their advice for putting everything behind one boundary is to run the Claude Code process itself in a container, a VM or the sandbox runtime.

Some common tools also fail inside it: `docker`, jest with watchman, and on Linux, Chromium and rootless Podman. Each workaround gives up some isolation.

## How to run Claude Code in a sandbox on your own computer

Pick by what you want to protect:

1. **Protect your files and network from stray shell commands.** Turn on `/sandbox`, deny reads of credential paths, keep `allowedDomains` short, and turn off unsandboxed retries. This costs nothing and stays on your laptop.
2. **Protect your whole machine from the agent.** Run Claude Code inside a dev container, a local VM or the sandbox runtime. Everything the agent starts is inside the boundary, but it shares your laptop's CPU and memory.
3. **Keep the agent's commands off your laptop entirely.** Send the commands to a throwaway sandbox on another machine. This is where OpenSandbox, the hosted services and Potluck sit.

Many teams do 1 and 3 together: the built-in sandbox for local work, and a separate box for builds, tests and anything that needs network access you would not grant on your own machine.

## Open-source, self-hosted sandboxes for AI coding agents

**OpenSandbox** manages the sandbox lifecycle (start, monitor, pause, terminate) on Docker for local testing and Kubernetes for production. It ships SDKs in several languages, lets you choose gVisor, Kata Containers or Firecracker for isolation, and has an MCP server so Claude Code or Cursor can call it. You run and scale the servers yourself.

**Anthropic's sandbox runtime** is the open-source package the Claude Code sandbox is built on. You can use it on its own to wrap the whole Claude Code process on one machine.

**Potluck** takes a different angle: it does not ship its own sandbox runtime. It drives Tart on Apple silicon Macs, and Docker or Podman with Kata or gVisor on Linux (Incus is also supported), and it pools the idle machines a team already owns. Each job gets a fresh VM or sandbox. It runs without Sengi and without a Sengi account. It is not a hosted cloud and not a scheduler for clusters.

What Potluck does for agents today:

- `potluck --pool friends mcp` runs an MCP server that exposes sandbox tools to an agent, so Claude Code can run commands in a pooled sandbox instead of on your laptop. More in [give your AI agent a sandbox over MCP](/blog/give-your-ai-agent-a-sandbox-over-mcp).
- `potluck agent-runner` runs a coding-agent vendor's self-hosted runners in fresh sandboxes.
- On the OCI layer the default is no network at all, and `potluck isolation test <size>` checks that a box cannot reach the host, its LAN or its tailnet.

What to know before you use it:

- The machine that runs a VM can read everything in it, secrets included. The machine that runs a pool can read commands and output passing through it. Join only a pool whose owner you trust.
- A Mac sandbox ends when its owner touches the keyboard or pointer. When an owner takes a machine back, running VMs end and new work is refused. On Linux, hand-back works only as a time window today.
- Apple's licence allows up to two macOS VMs on a Mac you own, and Potluck caps macOS guests at two per Mac (Potluck facts). Lending them to your team is fine; selling Mac time to the public is not. This is not legal advice.

See [the agent sandbox page](/agent-sandbox) and [run coding agents in a sandbox on your own machines](/blog/run-coding-agents-in-a-sandbox-on-your-own-machines) for more.

## How to start with Potluck

Install on macOS or Linux (no Go needed) and start the daemon:

```
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
potluck daemon --detach
```

On the machine that will run the pool:

```
potluck create friends
potluck invite ana
potluck invite carl --host
```

The first `create` prints a Tailscale login URL. Machines reach each other over Tailscale, or over a tunnel you configure. Ana runs `potluck join '<code>'` to use sandboxes. Carl first checks his spare box with `potluck doctor` and `potluck host --dry-run`, then runs `potluck host '<code>'`. That checks the machine, pulls the image, runs a quick conformance check per size, and joins only when a size passes. By default `--share` is 0.5, so half the cores and RAM are shared (Potluck facts). `potluck host stop` ends sharing.

A worked example from Ana's laptop:

```
potluck sandbox create
potluck sandbox exec <id> -- uname -a
potluck sandbox rm <id>
```

To hand the pool to Claude Code, register `potluck --pool friends mcp` as an MCP server. For Mac specifics, see [Tart](/tart); for spreading work over several boxes, see [pool spare Macs and Linux boxes](/blog/pool-spare-macs-and-linux-boxes-for-builds-and-agents).

## Try it early

Potluck is early. Its makers want people with a spare Mac or Linux box to try it and say what breaks. Start a thread at https://github.com/sengirunners/potluck/discussions. If you want to contribute, the good first issues are at https://github.com/sengirunners/potluck/issues.

## Questions people ask

### Is the Claude Code sandbox on by default?

No. Anthropic's docs say it is off until you run `/sandbox` or set `sandbox.enabled` to `true` in a settings file.

### Does the Claude Code sandbox cover file edits and MCP servers?

No. It covers shell commands and their child processes. The Read, Edit and Write tools, hooks and local MCP servers run outside it.

### What is an open-source sandbox for AI coding agents I can run on my own hardware?

Anthropic's sandbox runtime for one machine, OpenSandbox on your own Docker or Kubernetes servers, and Potluck for pooling a team's spare Macs and Linux boxes. All are open source; you supply the hardware.

### Can I sandbox Claude Code on Windows?

The Claude Code docs say native Windows runs commands unsandboxed and recommend WSL2. To share a machine in a Potluck pool you need an Apple silicon Mac or a Linux box.

### Does Potluck cost anything or need a Sengi account?

Potluck is free and open source under Apache-2.0, and it runs without Sengi and without a Sengi account. Sengi, its maker, sells hosted GitHub Actions runners as a separate product.

### Can the person who hosts my sandbox see my code?

Yes. The machine that runs a VM can read everything in it, including secrets, so join only pools whose owners you trust.

### Does Potluck run GitHub Actions too?

A real Actions job has been run through the runner broker by hand, on a Mac with Tart and on a Linux box with gVisor. It is not yet running daily in Potluck's own CI. See [the GitHub Actions runner page](/github-actions-runner).
