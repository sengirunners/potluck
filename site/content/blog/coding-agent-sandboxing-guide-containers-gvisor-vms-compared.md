---
slug: coding-agent-sandboxing-guide-containers-gvisor-vms-compared
kind: new
engine: claude
title: "Coding Agent Sandboxing Guide: Containers vs gVisor vs microVMs (November 2026)"
page_title: "Coding Agent Sandboxing: gVisor vs microVMs 2026 | Potluck"
page_description: "Containers vs gVisor vs microVMs for coding agents, November 2026: what each boundary stops, what it costs, and how to run one on machines you own."
target_searches:
  - "coding agent sandboxing guide containers gVisor microVMs"
data_sources:
  - "Brave term"
drafted: 2026-11-23
model: claude-opus-5-5
date: 2026-11-24
updated: 2026-11-24
---

**Short answer**: a plain container (Docker or Podman with the default runc runtime) shares the host kernel, so it is a packaging boundary, not a sandbox for code an agent writes. gVisor puts a user-space kernel between the agent and the host, and a microVM or full VM (Firecracker, Kata, Tart) gives the agent its own kernel, which is the strongest of the three. Then pick where the boundary runs: hosted platforms such as E2B, Modal and Daytona rent it to you, while self-hosting puts gVisor or a VM on hardware you already pay for. Potluck, a free, open-source (Apache-2.0) and early project, takes the self-hosted route. It runs each agent task in a fresh Tart VM on an Apple silicon Mac, or in a Kata or gVisor sandbox on Linux, on a team's idle machines. Per the Potluck docs, it caps macOS guests at two per Mac under Apple's licence.

## The isolation options at a glance

| Boundary | Kernel the agent talks to | What it stops | What it costs you |
|---|---|---|---|
| Process sandbox (Seatbelt, Bubblewrap, Landlock, seccomp) | The host kernel, with rules | File and syscall access outside allowed paths | Rules are easy to get wrong; agents have been seen probing for gaps |
| Container (Docker or Podman, runc) | The host kernel, shared | Casual mistakes, dependency mess | A kernel bug or a bad mount reaches the host |
| gVisor | A user-space kernel that re-implements syscalls | Direct calls into the host kernel | Some software hits syscalls gVisor does not emulate the same way |
| microVM (Firecracker, Kata Containers) | Its own guest kernel on hardware virtualization | Kernel exploits reaching the host | Needs KVM; you manage images and networking yourself |
| Full VM (Tart on a Mac, Incus on Linux) | Its own full OS | Same as a microVM, and you get macOS | Slower to start, more memory per guest |

The order runs weakest to strongest. Every step up costs some start time and memory, and buys a smaller surface the agent can attack.

## Why a coding agent needs more than a container

A coding agent writes a command and runs it before you read it. It installs packages, runs test suites, starts servers and deletes files. Most of the time that is fine. The cases that hurt are the ordinary ones: a cleanup step with a bad path, an install script piped from an unknown host, a test that forks until the machine stalls, or a dependency that reads environment variables and sends them somewhere.

A container handles the dependency mess well. It does not handle a hostile or broken process well, because every syscall still lands in the host kernel. That is why the hosted sandbox vendors and the cloud providers all reach for gVisor or a VM when the code is untrusted. Treat agent-written code as untrusted even when the agent is yours.

Three things matter as much as the kernel boundary:

- **Network.** Default to no egress. Most real damage needs a way out, so a box that cannot reach the internet, your LAN or your tailnet is far safer than one that can.
- **Secrets.** A sandbox does not scrub the API keys you pass in. Give the agent only the tokens the task needs.
- **Lifetime.** A fresh sandbox per task means a bad run cannot leave something behind for the next one.

## Containers vs gVisor vs microVMs: how to choose

Ask how much you trust the code and what it needs to run.

- **Your own scripts that passed review:** a container is enough.
- **Agent-written code on Linux, mostly language tooling:** gVisor is a good default. It runs ordinary OCI images and fits into Docker or Podman as a runtime. Test your toolchain first, since a few programs behave differently under it.
- **Agent-written code that installs arbitrary packages or binaries, or anything you would call hostile:** use a VM boundary, Kata on Linux or a microVM platform.
- **Anything that needs macOS or Xcode:** a full macOS VM, which on Apple silicon means a tool like Tart. No container or gVisor option runs macOS.

## Hosted platforms vs your own machines

Once you know the boundary, decide who runs it. Potluck is made by Sengi, Inc., which sells hosted GitHub Actions runners; Potluck itself is a separate free project, so weigh this table with that in mind.

| Option | Isolation | Where it runs | Good at | Costs more or needs more work |
|---|---|---|---|---|
| E2B | Firecracker microVMs | Hosted; open source, self-hostable | Clean SDKs, fast start for short tasks | Usage billing adds up for long sessions |
| Modal | gVisor | Hosted only | GPUs and Python workloads | No self-hosting; images defined in its SDK |
| Daytona | Docker by default, Kata optional | Hosted or self-hosted | Persistent dev environments | You must opt into Kata for a VM boundary |
| Google Agent Sandbox | gVisor or Kata on Kubernetes | Your cluster | Warm pools, no new vendor | You run the Kubernetes cluster |
| DIY Firecracker or gVisor | Your choice | Your servers | Full control | You build images, networking, scheduling |
| Potluck | Tart VMs on Mac; Kata or gVisor on Linux | Macs and Linux boxes you already own | Fresh sandbox per task on idle hardware, macOS guests | Early; machines are shared desktops, not a fleet |

Hosted platforms win when you need many sandboxes at once, GPUs or no hardware to look after. Self-hosting wins when you already own the machines, need macOS, or do not want agent code and repo contents on someone else's servers. Potluck sits in the self-hosted column but does not ask you to build a fleet: it drives Tart, Kata, gVisor and Incus on the computers your team already has.

## What Potluck checks before a machine takes work

`potluck host` checks the machine, pulls the image, runs a quick conformance check per sandbox size and joins the pool only when a size passes. `potluck doctor` shows what a machine can share, and `potluck host --dry-run` prints the plan without changing anything. The `--share` flag sets the fraction of cores and RAM shared; the Potluck docs give the default as 0.5.

For network isolation, `potluck isolation test <size>` checks that a box cannot reach the host, its LAN or its tailnet. On the OCI layer (Docker or Podman with Kata or gVisor) the default is no network at all.

Be clear about the trust model. The machine that runs a VM can read everything in it, including secrets, and the machine that runs a pool can read commands and output passing through it. Join only a pool whose owner you trust. There are no promises; use it at your own risk.

## How to start with Potluck

Install on macOS or Linux (no Go needed) and start the daemon:

```
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
potluck daemon --detach
```

Create a pool on the machine that will run it. The first time, it prints a Tailscale login URL:

```
potluck create friends
potluck invite ana
potluck invite carl --host
```

Ana runs `potluck join '<code>'` to use sandboxes. Carl runs `potluck host '<code>'` to share a machine, and `potluck host stop` when he wants it back. Sharing needs Tart on an Apple silicon Mac, or Docker or Podman with Kata or gVisor on Linux.

Run a sandbox by hand:

```
potluck sandbox create
potluck sandbox exec <id> -- uname -a
potluck sandbox rm <id>
```

To hand sandboxes to an agent, run `potluck --pool friends mcp`, which is an MCP server exposing sandbox tools. More on that in [give your AI agent a sandbox over MCP](/blog/give-your-ai-agent-a-sandbox-over-mcp) and [run coding agents in a sandbox on your own machines](/blog/run-coding-agents-in-a-sandbox-on-your-own-machines). If your coding-agent vendor offers self-hosted runners, `potluck agent-runner` runs them in fresh sandboxes. See also the [agent sandbox](/agent-sandbox) page.

## What makes Potluck different

- **It uses the Macs and Linux boxes your team already owns** instead of rented machines, so a gVisor, Kata or Tart boundary for agent code runs on hardware you have already paid for.
- **A Mac goes back to its owner the moment they touch the keyboard or pointer.** The sandbox ends, running VMs end and new work is refused, so lending a desktop does not mean losing it. On Linux, hand-back works only as a time window today.
- **One pool serves coding-agent sandboxes and GitHub Actions jobs,** each in a fresh VM or sandbox. A real Actions job has run through the runner broker by hand on a Mac (Tart) and on a Linux box (OCI with gVisor), though not yet daily in our own CI. See [GitHub Actions runner](/github-actions-runner).
- **Agents get sandbox tools over MCP,** and teammates and invited friends can lend machines to the same pool.
- **It is free and open source (Apache-2.0) and runs without a Sengi account.** It is not a hosted cloud, not a sandbox runtime of its own and not a cluster scheduler.

## Try it early

Potluck is early. Its makers want people with a spare Mac or Linux box to try it on real agent work and say what breaks. Start a thread at https://github.com/sengirunners/potluck/discussions. If you want to contribute code, the good first issues are at https://github.com/sengirunners/potluck/issues.

## Questions people ask

### Is Docker safe enough for a coding agent?

Not on its own with the default runc runtime, because the container shares the host kernel. Add gVisor or Kata as the runtime, or use a VM, and turn off network access unless the task needs it.

### What is the difference between gVisor and a microVM?

gVisor intercepts the agent's syscalls with a user-space kernel, so the agent never calls the host kernel directly. A microVM such as Firecracker or Kata gives the agent its own guest kernel on hardware virtualization, which is a stronger boundary but needs KVM and more setup.

### Can I sandbox a coding agent on macOS?

Yes, with a macOS or Linux VM on an Apple silicon Mac using Tart, which is what Potluck drives. Apple's licence allows up to two macOS VMs on a Mac you own, and Potluck caps macOS guests at two per Mac; see [Tart](/tart).

### Does a sandbox protect my API keys?

No. Anything you pass into the sandbox is readable by the code inside it, and with Potluck the machine running the VM can read it too, so pass only the keys the task needs.

### Can I lend Mac time to people outside my team?

Lending a macOS guest to your team or invited friends is fine under Apple's licence; selling Mac time to the public is not, and neither is pooling iOS Simulator time. This is not legal advice.

### How much does Potluck cost?

Potluck is free and open source under Apache-2.0. You supply the machines and their power.

### Is Potluck ready to replace a hosted sandbox platform?

Not for every team. It is early, it runs on shared desktops rather than a fleet, and Linux hand-back is only a time window today, so try it on non-critical agent work first.
