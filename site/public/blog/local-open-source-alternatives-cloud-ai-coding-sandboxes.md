# Local Alternatives to Cloud AI Coding Sandboxes (November 2026)

Published 2026-11-24. https://potluck.sh/blog/local-open-source-alternatives-cloud-ai-coding-sandboxes

**Short answer**: the local alternatives to cloud AI coding sandboxes such as E2B and Daytona are open-source tools that run the same kind of isolated box on hardware you control. The main ones are E2B's own open-source runtime, microsandbox, Hotcell, agent-sandbox for Kubernetes and agentbox, plus the runtimes underneath them: Firecracker, gVisor, Kata Containers, nsjail and bubblewrap. The software is free to use. You pay in hardware and in the hours it takes to run it. Potluck takes a different route: free, Apache-2.0 software that pools the Macs and Linux boxes a team already owns and gives each agent a fresh VM or sandbox on one of them, with GitHub Actions jobs served from the same pool. Potluck is early, and it is made by Sengi, a company that sells hosted GitHub Actions runners.

## Local alternatives at a glance

| Option | What it is | Isolation | Runs on | Where it needs more work |
|---|---|---|---|---|
| E2B open-source runtime | The runtime behind E2B's managed service | Firecracker microVMs | Cloud accounts you operate | You run the same distributed system E2B runs |
| microsandbox | Embeddable library, no long-running daemon | libkrun microVMs | Apple silicon Macs, Linux with KVM, Windows | You wire it into your own agent code |
| Hotcell | Sandbox layer with a key-swapping gateway on the host | Docker, Firecracker, Apple Virtualization | One Mac, Linux box or VM | Sized to one machine; non-HTTP credentials still have to be injected |
| agent-sandbox | Kubernetes resource and controller | gVisor or Kata via RuntimeClass | Any Kubernetes cluster | You need to run a cluster |
| agentbox | Container wrapper for one developer's agent | Docker plus an egress proxy and firewall | Your own machine | Container boundary, one developer |
| Firecracker, gVisor, Kata, nsjail, bubblewrap | Building blocks | microVM, user-space kernel, VM-backed containers, Linux namespaces | Linux | You build lifecycle, networking and cleanup yourself |
| Potluck | A pool of a team's own machines for agent sandboxes and Actions jobs | Tart on Apple silicon; Docker or Podman with Kata or gVisor on Linux | Macs and Linux boxes you own, joined over Tailscale | Early; Linux hand-back is only a time window today |

## Why teams move agent sandboxes off the cloud

Managed sandboxes are the least work. One API call and a box appears, and someone else patches the kernels. That is worth a lot, and for a team with no spare hardware it is often the right call.

The reasons people search for local alternatives are usually three:

- **Where the code runs.** Some repositories cannot leave company machines because of a client contract or policy.
- **The meter.** One agent session is cheap. Many agents running all day on per-second billing adds up, especially when Macs and Linux boxes sit idle on desks.
- **Control.** You pick the image, the network rules and when a box dies.

Running locally does not make the model provider go away. Your prompts and code still go to whichever model the agent calls, unless you run the model yourself too.

## The options, one by one

### E2B's open-source runtime

The runtime behind E2B's managed service is open source. It runs Firecracker microVMs with snapshot, pause and resume, and can be deployed into cloud accounts you operate. It is the closest thing to self-hosted E2B, and the most operational work on this list.

### microsandbox

microsandbox runs OCI images in microVMs built on libkrun, as a library with no server. It works on Apple silicon Macs, Linux with KVM and Windows, has SDKs for several languages, and supports secrets that never enter the VM. Good for builders who want sandboxing inside their own program.

### Hotcell

Hotcell runs as many sandboxes as one machine can hold, using Docker, Firecracker on Linux with KVM, or Apple Virtualization on macOS. Its strength is credentials: each sandbox gets a short-lived token, and a gateway on the host swaps it for the real key. Its own documentation says credentials that do not travel in an HTTP header still have to be injected.

### agent-sandbox for Kubernetes

If agents already run on Kubernetes, agent-sandbox adds a Sandbox resource with templates and warm pools. Isolation comes from the cluster's RuntimeClass, usually gVisor or Kata. It is a good fit for platform teams and a heavy one for a team without a cluster.

### agentbox

agentbox wraps a coding agent in a Docker container with an egress proxy, and injects secrets through the proxy so the agent never sees them. Lightweight, and aimed at a single developer.

### The building blocks

Firecracker gives a separate guest kernel through KVM. gVisor puts a user-space kernel between the container and the host. Kata gives VM-backed containers that keep OCI and Kubernetes interfaces. nsjail and bubblewrap confine a Linux process with namespaces, mounts and syscall filters. All are solid, and none is a finished agent product: you write the lifecycle, mounts, network policy and cleanup. Agent harnesses such as OpenHands sit on top and still need one of these as the boundary.

### What about Daytona?

Daytona's README says core development moved to a private codebase and the public repository no longer gets updates. You can fork the last open release, but it is not a maintained local option.

## A different approach: pool the machines you already own

Every tool above answers "how do I isolate code on this machine or cluster?" Potluck answers a different question: "we have spare Macs and Linux boxes on desks; how do we put them to work for agents and CI?"

Potluck is not a sandbox runtime of its own. It drives Tart, Kata, gVisor and Incus. It is not a hosted cloud and not a scheduler for clusters. One machine runs a pool, teammates and invited friends join it, and some of them lend machines to it. Machines reach each other over Tailscale, or over a tunnel you configure.

What that changes for the reader:

- **The Mac on someone's desk is usable.** A Mac sandbox ends when its owner touches the keyboard or pointer, and when an owner takes a machine back, running VMs end and new work is refused. On Linux, hand-back works only as a time window today.
- **One pool, two jobs.** The same pool serves coding-agent sandboxes and GitHub Actions jobs, each in a fresh VM or sandbox. A real Actions job has been run through the runner broker, by hand, on a Mac with Tart and on a Linux box with gVisor. It is not yet running daily in our own CI. `potluck agent-runner` runs a coding-agent vendor's self-hosted runners in fresh sandboxes.
- **Agents get tools directly.** `potluck --pool friends mcp` is an MCP server that exposes sandbox tools to an agent. More in [give your AI agent a sandbox over MCP](/blog/give-your-ai-agent-a-sandbox-over-mcp).

## What a local sandbox still has to get right

Local does not mean safe by default. With Potluck, `potluck isolation test <size>` checks that a box cannot reach the host, its LAN or its tailnet, and on the OCI layer the default is no network at all.

Be clear about trust. The machine that runs a VM can read everything in it, including secrets. The machine that runs a pool can read commands and output passing through it. Potluck does not swap API keys behind a gateway the way Hotcell, microsandbox or agentbox aim to. Join only a pool whose owner you trust, and use it at your own risk.

For Macs, Apple's licence allows up to two macOS VMs on a Mac you own, and Potluck caps macOS guests at two per Mac. Lending a macOS guest to your team or invited friends is fine; selling Mac time to the public is not, and neither is pooling iOS Simulator time. This is not legal advice.

## How to choose

| If you need | Start with |
|---|---|
| Self-hosted E2B at cloud scale | E2B's open-source runtime |
| A microVM library inside your own program | microsandbox |
| Real API keys never inside the sandbox, on one host | Hotcell |
| Sandboxes as Kubernetes resources | agent-sandbox |
| A quick container for one developer's agent | agentbox |
| Full control of every layer on Linux | Firecracker, gVisor, Kata, nsjail or bubblewrap |
| Agent sandboxes and Actions jobs on the team's spare Macs and Linux boxes | Potluck |

## How to start with Potluck

Install on macOS or Linux (no Go needed) and start the daemon:

```
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
potluck daemon --detach
```

On the machine that will run the pool, create it. The first time, it prints a Tailscale login URL.

```
potluck create friends
potluck invite ana
potluck invite carl --host
```

Ana runs `potluck join '<code>'` to use sandboxes. Carl runs `potluck host '<code>'` to share a machine. Before that, `potluck doctor` shows what the machine can share and `potluck host --dry-run` prints the plan. `potluck host` checks the machine, pulls the image, runs a quick conformance check per size and joins the pool only when a size passes. `--share` sets the fraction of cores and RAM shared (default 0.5, per the Potluck docs). `potluck host stop` stops sharing.

Sharing needs Tart on an Apple silicon Mac (see [Tart](/tart)), or Docker or Podman with Kata or gVisor on Linux. Then try a sandbox:

```
potluck sandbox create
potluck sandbox exec <id> -- uname -a
potluck sandbox rm <id>
```

For a longer walk-through, read [run coding agents in a sandbox on your own machines](/blog/run-coding-agents-in-a-sandbox-on-your-own-machines) and [pool spare Macs and Linux boxes for builds and agents](/blog/pool-spare-macs-and-linux-boxes-for-builds-and-agents).

## What makes Potluck different

- **It uses the machines you already own.** Instead of renting sandboxes, agents run on the team's idle Macs and Linux boxes, each in a fresh VM or sandbox.
- **A Mac goes back to its owner the moment they touch the keyboard or pointer**, so lending a desk machine does not mean giving it up.
- **One pool serves agents and CI.** Coding-agent sandboxes and GitHub Actions jobs come from the same pool, though Actions is not yet in daily use in our own CI.
- **Teammates and invited friends can lend machines to the same pool**, and agents reach it through sandbox tools over MCP.
- **It is free and open source (Apache-2.0)** and runs without Sengi and without a Sengi account.

## Try it early

Potluck is early. Its makers want people with a spare Mac or Linux box to try it and say what breaks. Start a thread at https://github.com/sengirunners/potluck/discussions. If you want to contribute code, the good first issues are at https://github.com/sengirunners/potluck/issues. The source is at https://github.com/sengirunners/potluck.

## Questions people ask

### What is the best local alternative to E2B?

For E2B's own architecture on cloud accounts you run, use E2B's open-source runtime. For one machine with keys kept out of the sandbox, look at Hotcell or microsandbox. For a pool of the team's own Macs and Linux boxes, try Potluck.

### Can I run AI coding sandboxes on a Mac?

Yes. microsandbox and Hotcell support Apple silicon, and Potluck runs macOS guests through Tart, capped at two per Mac to match Apple's licence.

### Do local sandboxes keep my code private?

They keep execution on machines you control. Code and prompts still go to the model provider the agent calls, and with Potluck the machine running a VM can read everything inside it.

### Does Potluck keep API keys out of the sandbox?

No, it does not claim to. The host machine can read secrets in a VM, so if key isolation is your main need, Hotcell, microsandbox or agentbox focus on it.

### Is Daytona still open source?

Its last open release can still be forked, but its README says development moved to a private codebase and the public code gets no further updates.

### Can Potluck run GitHub Actions jobs too?

A real Actions job has been run through its runner broker by hand on a Mac and on a Linux box, but not yet daily in our own CI. See [GitHub Actions runner](/github-actions-runner) for the current state.

### Is Potluck a hosted service?

No. It is free, open-source software you run yourself, and it needs no Sengi account.
