---
slug: open-source-self-hosted-coding-agents-and-ci
kind: new
engine: claude
title: "Open-Source Self-Hosted Coding Agents and CI on Your Own Machines (November 2026)"
page_title: "Self-Hosted Coding Agents and CI, Nov 2026 | Potluck"
page_description: "Which open-source tools run coding agents and CI jobs on machines you already own, as of November 2026: agents, runners, sandboxes and where Potluck fits."
target_searches:
  - "open source self-hosted coding agent run locally"
  - "open source self-hosted coding agent runner CI own machines 2026"
  - "open source self-hosted coding agent runner CI on own machines 2026"
  - "what open source tools run coding agents and CI jobs on machines I already own"
data_sources:
  - "AI search x1"
  - "AI search x3"
drafted: 2026-11-23
model: claude-opus-5-5
date: 2026-11-24
updated: 2026-11-24
---

**Short answer**: To run coding agents and CI jobs on machines you already own with open-source software, you need two pieces: an agent and a safe place to run it and your builds. For the agent, OpenHands and OpenCode are MIT-licensed according to their repositories, and Cline's CLI, Desktop and VS Code surfaces are Apache-2.0 according to its repository. For the machines, Potluck, free and Apache-2.0, made by Sengi, Inc., pools a team's idle Macs and Linux boxes and runs GitHub Actions jobs and coding agents on them, each in a fresh VM or sandbox, with at most two macOS guests per Mac (Apple's licence). Potluck is early: a real Actions job has gone through it by hand on a Mac and on a Linux box, but it does not yet run daily in its makers' own CI.

## Two jobs: the agent, and the machine it runs on

Most pages about self-hosted coding agents compare agents. That is half the question. The searches behind this post ask for something that runs agents and CI jobs on machines you already own, which splits into:

- **The agent or harness.** The program that reads code, calls a model and runs commands. OpenHands, OpenCode, Cline, Coder Agents, or a CLI such as Claude Code or Codex.
- **The execution layer.** The machine, VM or sandbox where those commands and your CI jobs actually run, and what that box can reach.

An agent on your laptop runs with your shell, your files and your network. A CI job on a self-hosted runner runs on whatever host you installed it on. Neither is isolated unless you add isolation. Potluck works on the second job: it turns spare Macs and Linux boxes into a pool that hands out fresh VMs and sandboxes, and it drives existing runtimes (Tart, Kata, gVisor and Incus) instead of shipping its own.

## The options at a glance (November 2026)

Potluck is made by Sengi, which sells hosted GitHub Actions runners. Potluck is a separate free project that runs without Sengi or a Sengi account. Licences below are as stated by each project's own repository or docs.

| Option | What it is | Runs on your machines | Licence | Where it needs more work or costs more |
|---|---|---|---|---|
| Agent CLI on a box you own, in tmux | Claude Code, Codex or OpenCode on a Linux machine, kept alive with tmux over SSH | Execution, clone, credentials | Depends on the CLI; Codex CLI is Apache 2.0 | One shared checkout, sessions lost on reboot, no isolation from the host |
| OpenHands | Agent harness with a web UI; each conversation can get its own Docker container | Harness, UI, execution | MIT | You run Docker, upgrades and access control; its docs say the UI is unauthenticated as installed |
| OpenCode | Agent CLI with a headless server mode and local model support | CLI and execution | MIT | Isolation is whatever machine you start it on |
| Cline CLI, Desktop, VS Code | Agent surfaces using your own provider or a local runtime | Execution, credentials | Apache-2.0; the JetBrains plugin is not open source | Its docs say telemetry is on by default; check the approval defaults |
| Coder Agents | Agent platform for your data center or cloud account | Platform and execution | AGPL v3 | A platform to deploy and operate |
| GitHub's self-hosted runner | GitHub's runner program installed on a machine you own | Actions jobs | Open source, published by GitHub | Jobs run on the host itself unless you add your own VM or container wrapping and cleanup |
| Potluck | Pools owned Macs and Linux boxes; Actions jobs and agents in fresh VMs or sandboxes | Actions jobs, agent sandboxes, vendor agent runners | Apache-2.0 | Early; Linux hand-back only as a time window; Actions not yet daily in its makers' CI |
| Hosted runners (GitHub-hosted, Sengi and others) | Someone else's machines, paid by use | Nothing | Not self-hosted | Recurring cost; in exchange there is no hardware to maintain |

These combine. A team can run OpenCode or OpenHands as the agent and point its command execution at Potluck sandboxes, and keep GitHub's runner for some jobs while moving others to fresh VMs.

## How Potluck plugs into agents and CI

Potluck exposes the pool in four ways.

**Sandboxes from the command line.** `potluck sandbox create` prints an id, `potluck sandbox exec <id> -- uname -a` runs a command inside it, and `potluck sandbox rm <id>` removes it. Any script or agent that can run a shell command can use these. More in [run coding agents in a sandbox on your own machines](/blog/run-coding-agents-in-a-sandbox-on-your-own-machines) and on the [agent sandbox](/agent-sandbox) page.

**An MCP server for agents.** `potluck --pool friends mcp` starts an MCP server that exposes sandbox tools to an agent, so an agent that speaks MCP can create a box, run commands in it and throw it away. See [give your AI agent a sandbox over MCP](/blog/give-your-ai-agent-a-sandbox-over-mcp).

**Vendor agent runners.** `potluck agent-runner` runs a coding-agent vendor's self-hosted runners in fresh sandboxes, so a hosted agent product that supports self-hosted runners executes on your hardware instead of a long-lived host.

**GitHub Actions.** A runner broker sends Actions jobs to fresh VMs or sandboxes. As of today a real Actions job has been run through it, by hand, on a Mac with Tart and on a Linux box on another machine using OCI with gVisor. It is not yet running daily in our own CI, so treat it as something to try, not something to move your release pipeline onto. Details on the [GitHub Actions runner](/github-actions-runner) page and in [self-hosted GitHub Actions runner on your Mac](/blog/self-hosted-github-actions-runner-on-your-mac).

## What stays on your machines, and what still leaves

Self-hosting execution keeps the repository clone, build outputs and the credentials the job uses on hardware you own. It does not, by itself, keep everything local:

- **The model request.** Where prompts and the file contents an agent reads are sent is set by the agent's own configuration. With a hosted model they go to that provider. OpenHands, OpenCode and Cline each document local model servers such as Ollama, LM Studio or vLLM if prompts must stay in your network.
- **The host can read the guest.** In Potluck, the machine that runs a VM can read everything in it, including secrets. The machine that runs a pool can read the commands and output passing through it. Join only a pool whose owner you trust, and keep production secrets out of pools that include machines you do not control.
- **Network reach.** Whatever a box can reach, an agent in it can reach. `potluck isolation test <size>` checks that a box cannot reach the host, its LAN or its tailnet. On the OCI layer the default is no network at all, which also means a job that needs to download packages needs network you choose to give it.

There are no promises here: use it at your own risk, and verify the boundaries you care about on your own setup.

## What each machine needs

- **Mac:** Tart on an Apple silicon Mac. Apple's licence allows up to two macOS VMs on a Mac you own, and Potluck caps macOS guests at two per Mac. Lending a macOS guest to your team or invited friends is fine; selling Mac time to the public is not, and neither is pooling iOS Simulator time. This is not legal advice. See the [Tart](/tart) page.
- **Linux:** Docker or Podman with Kata or gVisor.
- **Sharing:** `--share` sets the fraction of cores and RAM shared, and Potluck's default is 0.5, half the machine.
- **Taking a machine back:** a Mac sandbox ends when its owner touches the keyboard or pointer. When an owner takes a machine back, running VMs end and new work is refused. On Linux, hand-back works only as a time window today.

Machines reach each other over Tailscale, or over a tunnel you configure. For a longer walkthrough of a mixed pool, see [pool spare Macs and Linux boxes for builds and agents](/blog/pool-spare-macs-and-linux-boxes-for-builds-and-agents).

## How to start with Potluck

Install on macOS or Linux (no Go needed) and start the daemon:

```
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
potluck daemon --detach
```

On the machine that runs the pool:

```
potluck create friends
potluck invite ana
potluck invite carl --host
```

The first `potluck create` prints a Tailscale login URL. `potluck invite ana` makes a code for a friend who runs sandboxes; `potluck invite carl --host` makes a code for a friend who shares a machine.

A friend who wants sandboxes runs `potluck join '<code>'`. A friend sharing a machine first runs `potluck doctor` to see what it can share and `potluck host --dry-run` to print the plan, then `potluck host '<code>'`. That checks the machine, pulls the image, runs a quick conformance check per size and joins the pool only when a size passes. `potluck host stop` stops sharing.

Then try a box:

```
potluck sandbox create
potluck sandbox exec <id> -- uname -a
potluck sandbox rm <id>
```

## Try it early

Potluck is early. Its makers want people with a spare Mac or Linux box to try it and say what breaks: a machine that fails the conformance check, a runner job that hangs, an agent that cannot reach its sandbox. Start a thread at https://github.com/sengirunners/potluck/discussions with what you ran and what happened. If you want to contribute code, the good first issues are at https://github.com/sengirunners/potluck/issues. The source is at https://github.com/sengirunners/potluck.

## Questions people ask

### What open-source tools run coding agents and CI jobs on machines I already own?

For the agent, OpenHands, OpenCode, Cline's open-source surfaces and Coder Agents all run on your own hardware. For execution, GitHub's self-hosted runner runs Actions jobs on a host you own, and Potluck runs Actions jobs and coding agents in fresh VMs or sandboxes across a pool of your Macs and Linux boxes.

### Is Potluck a coding agent?

No. Potluck gives agents and CI jobs a place to run: fresh sandboxes from the command line, an MCP server with sandbox tools, and `potluck agent-runner` for a vendor's self-hosted runners. You bring the agent.

### Can I run an open-source coding agent fully locally?

The agent and its commands can run entirely on your machines. The model request leaves unless you point the agent at a local model server, which OpenHands, OpenCode and Cline each document; check the agent's telemetry and update settings too.

### Does Potluck run GitHub Actions jobs today?

A real Actions job has been run through its runner broker by hand, on a Mac with Tart and on a Linux box with OCI and gVisor. It is not yet running daily in its makers' own CI, so it is ready to try, not to depend on.

### What does Potluck cost?

Potluck is free and open source under Apache-2.0, and it runs without Sengi or a Sengi account. Your cost is the machines you already own.

### Can I lend my Mac to friends for builds?

Yes, to your team or invited friends, with at most two macOS guests per Mac as Apple's licence allows. Selling Mac time to the public or pooling iOS Simulator time is not allowed; this is not legal advice.

### Is my code safe on someone else's machine?

The machine that runs a VM can read everything in it, including secrets, and the pool's machine can read commands and output. Join only pools whose owner you trust, and keep secrets you cannot afford to expose out of shared pools.

### Do I need Kubernetes?

No. Potluck is not a cluster scheduler; it runs on individual Macs and Linux boxes with Tart, or Docker or Podman with Kata or gVisor.
