---
slug: run-claude-code-docker-sandbox-devcontainer
kind: new
engine: claude
title: "Claude Code Docker sandbox and devcontainer setup (2026)"
page_title: "Claude Code Docker Sandbox and Devcontainer Setup | Potluck"
page_description: "Four ways to run Claude Code in a sandbox in 2026: built-in sandbox, devcontainer, Docker Sandboxes, your own image, plus spare team machines."
target_searches:
  - "Claude Code Docker sandbox devcontainer setup"
  - "Claude Code run locally"
  - "Claude Code sandbox run locally Claude Code Anthropic run Claude locally"
  - "claude code sandbox create command 'claude sandbox' 'claude --sandbox' 'claude sandbox run'"
  - "claude code settings.local.json sandbox.filesystem example 'sandbox.filesystem' 'sandbox.network' 'settings.local.json' claude"
data_sources:
  - "Brave term"
  - "AI search x1"
drafted: 2026-11-23
model: claude-opus-5-5
date: 2026-11-24
updated: 2026-11-24
---

**Short answer:** As of November 2026 there are four common ways to run Claude Code locally in a sandbox. The first is its built-in sandbox, turned on with the `/sandbox` command or the `sandbox` block in `.claude/settings.local.json`, which uses Seatbelt on macOS and bubblewrap on Linux. The second is Anthropic's reference devcontainer, a Docker container plus a firewall. The third is Docker Sandboxes, where `sbx run claude ~/my-project` starts one microVM per sandbox. The fourth is your own Docker image run as a non-root user. The Claude Code material these searches lead to describes no `claude sandbox` subcommand. The Claude Code program runs on your machine, but the default models run at Anthropic, so you need an API key or a Claude subscription. Anthropic says its sandbox cuts permission prompts by 84%, as quoted in a write-up by the consultancy Sourcelabs. If you want the commands to run on a spare Mac or Linux box instead of your laptop, Potluck is a different route. It is free, open source and early, and it gives an agent fresh sandboxes on machines your team already owns, with at most two macOS guests per Mac.

## The options compared

| Option | How you start it | Isolation boundary | Network control | Where it costs more or needs more work |
|---|---|---|---|---|
| Claude Code built-in sandbox | `/sandbox` in a session, or `sandbox` in settings | OS level: Seatbelt (macOS), bubblewrap (Linux) | Allow lists in settings | The sandboxed shell skips your profile, so tools may be missing from PATH. Settings live on each machine |
| Anthropic reference devcontainer | Reopen the repo in the container from your editor | Docker container plus a firewall script | Firewall limits outbound hosts | Needs Docker and an editor that understands devcontainers. You maintain the image |
| Docker Sandboxes (`sbx`) | `sbx run claude ~/my-project` | microVM with its own Docker daemon, filesystem and network | Policy presets such as Balanced and Locked Down | Needs the sbx CLI and a Docker sign-in. Does not read your `~/.claude` |
| Your own Docker image | `docker run`, then `docker exec` | Container | Whatever you configure | You handle credentials, the non-root user, caches and cleanup |
| Potluck | `potluck sandbox create`, or `potluck --pool friends mcp` | Fresh VM (Tart on a Mac) or sandbox (Kata or gVisor on Linux) on a team machine | No network at all by default on the OCI layer | Early. Needs a spare machine and Tailscale or a tunnel. The host machine can read what runs in it |

Potluck is made by Sengi, Inc., which also sells hosted GitHub Actions runners. Potluck is a separate free project and needs no Sengi account.

## Is there a `claude sandbox` command?

People search for `claude sandbox`, `claude --sandbox` and `claude sandbox run`. The Claude Code material these searches lead to describes switching sandboxing on in two ways: the `/sandbox` command inside an interactive session, or the `sandbox` block in a settings file. It does not describe a separate `claude sandbox` subcommand.

The command that does start Claude Code in a sandbox from one line is Docker's: `sbx run claude`. That is the Docker Sandboxes CLI, not Claude Code.

## Claude Code's built-in sandbox and settings.local.json

Claude Code reads settings from three places:

- `~/.claude/settings.json` is your user file and applies to every project.
- `.claude/settings.json` is the project file you commit.
- `.claude/settings.local.json` is a project file you keep out of git, which suits per-machine sandbox paths.

The smallest useful local file turns the sandbox on:

```json
{
  "sandbox": {
    "enabled": true
  }
}
```

The filesystem and network rules people search for as `sandbox.filesystem` and `sandbox.network` go inside that same `sandbox` block. In them you list extra write paths, such as build caches like `~/.m2` and `~/.npm`, and the hosts the shell may reach, such as your package registry. Key names have shifted between Claude Code releases, so copy them from the settings reference that matches your installed version rather than from an old blog post.

Two lessons from the Sourcelabs write-up, whose authors ran Claude Code headless in a pipeline:

- **PATH.** Inside the Seatbelt sandbox the shell did not load `.zshrc` or `.bashrc`, so `node` and `mvn` were not found. They had to resolve tool paths on the host and pass them in.
- **Headless settings.** In their setup, project-level sandbox settings did not apply to `claude -p` runs, so they fell back to the user file in `~/.claude`.

The built-in sandbox works well for interactive sessions on your own laptop, where you can answer a prompt when a rule blocks something. For unattended runs it needs more setup up front.

## Anthropic's reference devcontainer

Anthropic publishes a reference devcontainer that runs Claude Code inside a Docker container, with a firewall layer limiting where it can connect. You open the repository in an editor that supports devcontainers and reopen it in the container. Claude Code then runs inside, against your mounted project.

This is the setup Anthropic points to when you want `--dangerously-skip-permissions`: the container is the boundary, so skipping prompts is less risky.

The cost is that you own the image. Your toolchain, caches and credentials all have to be set up in it. The Sourcelabs team chose to disable Claude Code's own sandbox inside their container, with `{ "sandbox": { "enabled": false } }`, because Seatbelt and bubblewrap rules can clash with a containerised environment. Anthropic's reference keeps it on and adds the firewall.

## Docker Sandboxes: `sbx run claude`

Docker Sandboxes is the shortest path from nothing to Claude Code in an isolated box, according to Docker's documentation:

1. Install the `sbx` CLI and sign in to Docker.
2. Store an API key with `sbx secret set anthropic`, or skip it and use `/login` inside Claude Code for a subscription.
3. Run `sbx run --name my-sandbox claude` from your project directory. On the first run, pick a network preset. Balanced allows common development services and asks about the rest.
4. Check status with `sbx ls`, stop with `sbx stop my-sandbox`, and remove with `sbx rm my-sandbox`.

What Docker's docs say it does:

- Each sandbox is a microVM with its own Docker daemon. The agent can build images and start containers without touching your host.
- The default start command is `claude --dangerously-skip-permissions`.
- Your project directory is mounted read-write. `--clone` gives the agent a private clone instead, which helps when several agents work on one repository.
- Hosts can be allowed one by one with `sbx policy allow network`.

One catch: the sandbox does not pick up user-level configuration such as `~/.claude`, only project-level files in the working directory.

## Your own Docker image

If you run Claude Code in a pipeline, a plain image is easy to reason about. This one follows the Sourcelabs example:

```dockerfile
FROM node:22-slim
RUN apt-get update && apt-get install -y git jq
RUN npm install -g @anthropic-ai/claude-code
RUN useradd -m -s /bin/bash claude
USER claude
```

The non-root user matters. Sourcelabs found that Claude Code refuses `--dangerously-skip-permissions` when run as root.

They also changed how they used containers. At first every step started a fresh container, which re-downloaded dependencies each time. They switched to one named container per ticket and ran each step in it with `docker exec`.

You also have to get credentials in. That means an `ANTHROPIC_API_KEY` environment variable, or the OAuth credentials file on Linux.

## Running the sandbox on another machine with Potluck

Every option above runs on the machine in front of you. Potluck asks a different question: what if the commands Claude Code runs happen on a spare Mac mini or Linux box in the office, in a fresh VM or sandbox each time, while Claude Code stays on your laptop?

Potluck exposes sandbox tools to an agent over MCP with `potluck --pool friends mcp`. You register that command as an MCP server in Claude Code. The agent can then create a sandbox, run commands in it and remove it, on whichever pool machine has room. There is more on this in [give your AI agent a sandbox over MCP](/blog/give-your-ai-agent-a-sandbox-over-mcp).

On the Linux OCI layer the default is no network at all. That suits running untrusted build and test commands, but it means you would not run Claude Code itself inside such a box by default, since it has to reach Anthropic. Keep Claude Code where your credentials already are and send the commands out.

Also keep the trust model in mind. The machine that runs a VM can read everything in it, including secrets. The machine that runs the pool can read commands and output passing through it.

## How to start with Potluck

Install on macOS or Linux (no Go needed), then start the daemon:

```sh
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
potluck daemon --detach
```

**Create a pool and invite people.** On the machine that runs the pool, run `potluck create friends`. The first time, it prints a Tailscale login URL. Then:

- `potluck invite ana` makes a code for someone who will run sandboxes.
- `potluck invite carl --host` makes a code for someone who will share a machine.

They run `potluck join '<code>'` or `potluck host '<code>'`.

**Share a machine.** Sharing needs Tart on an Apple silicon Mac, or Docker or Podman with Kata or gVisor on Linux. Check first with `potluck doctor` and `potluck host --dry-run`. `--share` sets the fraction of cores and RAM shared; the default is 0.5, per the Potluck docs. `potluck isolation test <size>` checks that a box cannot reach the host, its LAN or its tailnet. `potluck host stop` stops sharing.

**Try a sandbox by hand:**

```sh
potluck sandbox create
potluck sandbox exec <id> -- uname -a
potluck sandbox rm <id>
```

See [the agent sandbox page](/agent-sandbox) and [running coding agents in a sandbox on your own machines](/blog/run-coding-agents-in-a-sandbox-on-your-own-machines) for longer walkthroughs.

## What makes Potluck different

- **It uses machines you already own.** Claude Code's commands run on the Macs and Linux boxes your team already has, not on your laptop and not on rented machines.
- **A borrowed Mac goes back to its owner.** A Mac sandbox ends the moment its owner touches the keyboard or pointer. On Linux, hand-back works only as a time window today.
- **One pool for agents and CI.** The same pool serves coding-agent sandboxes and GitHub Actions jobs, each in a fresh VM or sandbox. A real Actions job has been run through it by hand on a Mac and on Linux, but it is not yet in daily use in Potluck's own CI.
- **Agents get sandbox tools over MCP.** Claude Code calls sandbox tools directly instead of shelling into your host.
- **Free and open source.** Potluck is Apache-2.0 and runs without a Sengi account. Teammates and invited friends can lend machines to the same pool.

## Try it early

Potluck is early. Its makers want people with a spare Mac or Linux box to try it with Claude Code and say what breaks. Start a thread at https://github.com/sengirunners/potluck/discussions. If you want to contribute, the good first issues are at https://github.com/sengirunners/potluck/issues.

## Questions people ask

### Can I run Claude Code fully locally?

The Claude Code program runs locally, but with the default Anthropic models the model runs at Anthropic and needs an API key or a Claude subscription. Docker's documentation covers pointing Claude Code at local or hosted models instead.

### How do I turn on the Claude Code sandbox?

Type `/sandbox` in an interactive session, or set `"sandbox": { "enabled": true }` in `.claude/settings.local.json` or another settings file. It uses Seatbelt on macOS and bubblewrap on Linux.

### Should I use the devcontainer or Docker Sandboxes?

The devcontainer gives you a container you fully control and can commit to the repo. Docker Sandboxes gives you a microVM with its own Docker daemon and network policy from one command, but needs the sbx CLI and a Docker sign-in.

### Why does Claude Code fail as root in Docker?

Claude Code refuses `--dangerously-skip-permissions` when run as root, as the Sourcelabs write-up reports. Create a non-root user in your Dockerfile and switch to it.

### Can Potluck run Claude Code's commands on a spare Mac?

Potluck gives Claude Code sandbox tools over MCP, so its commands run in fresh VMs on a spare Mac (Tart, Apple silicon) or Linux box. Apple's licence allows up to two macOS VMs on a Mac you own, and Potluck caps macOS guests at two per Mac.

### Is a Potluck sandbox safe for my API key?

The machine that runs a VM can read everything in it, including secrets, so only put keys on machines whose owners you trust. Potluck makes no promises; use it at your own risk.
