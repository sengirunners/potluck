---
slug: giving-coding-agents-their-own-mac-macos-vms
kind: new
engine: claude
title: "Give Coding Agents Their Own Mac: macOS VMs on a Mac mini (2026)"
page_title: "Give Coding Agents Their Own Mac in 2026 | Potluck"
page_description: "One Mac mini can give coding agents two macOS VMs at once. How Tart, UTM, Lima and Potluck compare for safe, disposable agent sandboxes on Apple silicon."
target_searches:
  - "give coding agents their own Mac macOS VM"
  - "Tart UTM Lima macOS VM sandbox AI coding agent Mac mini"
  - "Tart macOS VM Apple Silicon sandbox AI agent"
  - "how do I give an AI coding agent a safe disposable VM on a Mac mini"
data_sources:
  - "Brave term"
  - "AI search x1"
  - "AI search x2"
drafted: 2026-11-23
model: claude-opus-5-5
date: 2026-11-24
updated: 2026-11-24
---

**Short answer**: one Apple silicon Mac mini can give coding agents two macOS VMs at a time, because Apple's licence allows up to two macOS VMs on a Mac you own. Potluck caps macOS guests at two per Mac for the same reason. The usual way to do it is Tart: clone a fresh macOS VM for each task, run the agent in it headless, and delete the VM when the task ends. UTM suits people who want the VM in a window, and Lima is built around Linux guests. Potluck, an early, free and open-source (Apache-2.0) project, drives Tart on your Macs and Kata or gVisor on Linux, so an agent gets a fresh sandbox per task through a command line or an MCP server without you writing the VM lifecycle yourself.

## The options compared

Potluck is made by Sengi, Inc., which sells hosted GitHub Actions runners. Potluck is a separate free project and runs without Sengi or a Sengi account.

| Option | What it runs on a Mac | Best at | Disposable per task | How an agent gets in | Work you do |
|---|---|---|---|---|---|
| Tart | macOS and Linux VMs on Apple silicon | Headless, scripted macOS guests from registry images | Yes, if you clone and delete per task | Your own scripts around its exec command | Write and maintain the lifecycle scripts |
| UTM | macOS, Linux and Windows guests, with a GUI | Interactive use, trying an OS in a window | Possible, mostly by hand | You shell into the guest | More manual steps for automation |
| Lima | Linux VMs, often used for containers | Linux dev environments on a Mac | Yes, for Linux guests | Shell or container tools | Not the tool for a macOS guest |
| Hosted Macs (cloud or hosted runners) | Machines you rent | No hardware to look after | Depends on the provider | Provider's API or runner | You pay for rented time |
| Potluck | Tart guests on Macs, Kata or gVisor sandboxes on Linux | Fresh sandboxes for agents and Actions jobs on machines you own | Yes, each run in a fresh VM or sandbox | `potluck sandbox` commands or the MCP server | Install, create a pool, host a machine |

## Why an agent needs its own Mac

A coding agent that builds a Mac app, clicks through its UI or installs tools wants the whole machine. On your desktop it competes with you for the keyboard, the pointer and the front window, and it can reach your files, your keychain and your shell history. A macOS VM fixes both: the agent gets a desktop of its own, and when the task ends you throw the VM away so the next task starts clean.

A developer blog post from November 2026, about running a Mac card game's UI tests in a VM, describes the pattern well: Xcode and the source stay on the host, the host builds a Debug app, the app is copied into a headless macOS guest, and the agent drives it there and reads back a screenshot and the accessibility tree. The host Mac stays usable the whole time.

## The two-VM limit on a Mac mini

The number that shapes every plan is two. Apple's licence allows up to two macOS VMs on a Mac you own, and Potluck caps macOS guests at two per Mac to match. A Mac mini therefore gives you at most two agent Macs at once. For more parallel macOS agents you need more Macs, and Linux sandboxes on other boxes can take the work that does not need macOS.

The licence also limits who uses those guests. Lending a macOS guest to your team or to invited friends is fine; selling Mac time to the public is not, and neither is pooling iOS Simulator time. This is not legal advice.

## Doing it by hand with Tart

Tart, from Cirrus Labs, is the most direct route. You install it with Homebrew, use `tart clone` to pull a macOS image from a container registry into a named VM, start it headless with `tart run`, and send commands into the guest with `tart exec`. Use the prebuilt images that include Tart's guest agent, since exec needs it; the vanilla images lack it. The images are large, so check free disk space first, and pick the macOS version your app needs as its minimum.

To make the VM disposable, clone a fresh copy from the base image for each task and delete it afterwards. That loop is what you end up scripting: create, wait for the desktop, copy the build in, run, collect output, delete. It works well, and you own every line of it. For more on Tart, see our [Tart page](/tart).

## UTM and Lima: where they fit

UTM is a good app for running macOS, Linux and Windows guests in a window on Apple silicon. It is the friendlier choice when a person sits in front of the VM. For an agent that needs a fresh guest every few minutes, you do more by hand than with Tart.

Lima is a solid way to run Linux VMs on a Mac, often as the base for containers. If your agent only needs a Linux shell, a Lima VM on the Mac mini is a fine sandbox. For a macOS guest, people reach for Tart or UTM instead.

## Making the VM safe, and what safe means

A fresh VM per task stops one task from leaving files or processes behind for the next. Beyond that:

- **Network.** Decide what the guest can reach. With Potluck, `potluck isolation test <size>` checks that a box cannot reach the host, its LAN or its tailnet. On Potluck's OCI layer on Linux, the default is no network at all.
- **Secrets.** The machine that runs a VM can read everything in it, including any secret you pass in. Give an agent only the tokens the task needs.
- **The person at the Mac.** On a desk Mac, Potluck ends a Mac sandbox when its owner touches the keyboard or pointer. A Mac mini in a cupboard has nobody touching it, so it stays available.

## Start with Potluck

Potluck is early. On macOS or Linux, with no Go needed:

```
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
potluck daemon --detach
```

On the machine that runs the pool (the first time, it prints a Tailscale login URL), create the pool and make a code for a machine to share:

```
potluck create friends
potluck invite carl --host
```

On the Mac mini you want to share, which needs Tart on Apple silicon, check it, look at the plan, then host:

```
potluck doctor
potluck host --dry-run
potluck host '<code>'
```

`potluck host` checks the machine, pulls the image, runs a quick conformance check per size and joins the pool only when a size passes. `--share` (default 0.5) sets the fraction of cores and RAM shared. `potluck host stop` stops sharing.

Then create a sandbox, run a command in it and remove it:

```
potluck sandbox create
potluck sandbox exec <id> -- uname -a
potluck sandbox rm <id>
```

To give an agent sandbox tools directly, run `potluck --pool friends mcp`, an MCP server the agent connects to. There is more in [give your AI agent a sandbox over MCP](/blog/give-your-ai-agent-a-sandbox-over-mcp) and [run coding agents in a sandbox on your own machines](/blog/run-coding-agents-in-a-sandbox-on-your-own-machines). If you use a coding-agent vendor's self-hosted runners, `potluck agent-runner` runs them in fresh sandboxes.

## What makes Potluck different

- **It uses the Mac mini you already own.** Your agent's VMs run on Macs and Linux boxes the team already has, instead of rented machines.
- **A desk Mac goes back to its owner the moment they touch the keyboard or pointer.** You can lend a work Mac to agents when it is idle without losing it when you sit down. On Linux, hand-back is only a time window today.
- **One pool, a fresh VM or sandbox per run, for agents and CI.** The same machines serve coding-agent sandboxes and GitHub Actions jobs. A real Actions job has been run through the runner broker by hand on a Mac and on a Linux box; it is not yet daily in our own CI.
- **Agents get sandbox tools over MCP,** and teammates or invited friends can lend their own machines to the same pool.
- **Free and open source (Apache-2.0), no Sengi account needed.** It drives Tart, Kata, gVisor and Incus rather than replacing them, and it is not a hosted cloud or a cluster scheduler.

## Try it early

Potluck is early, and its makers want people with a spare Mac or Linux box to try it and say what breaks. Start a thread at https://github.com/sengirunners/potluck/discussions. If you want to contribute, the good first issues are at https://github.com/sengirunners/potluck/issues. The code is at https://github.com/sengirunners/potluck. For a wider view, read [pool spare Macs and Linux boxes for builds and agents](/blog/pool-spare-macs-and-linux-boxes-for-builds-and-agents).

## Questions people ask

### How many macOS VMs can a coding agent use on one Mac mini?

Two at once. Apple's licence allows up to two macOS VMs on a Mac you own, and Potluck caps macOS guests at two per Mac.

### Is Tart or UTM better for an AI agent sandbox?

Tart, for most agent setups, because it is built for headless, scripted guests and has a guest agent for running commands. UTM is better when a person wants to use the VM in a window.

### Can I use Lima to give an agent a macOS VM?

Lima is built around Linux guests. Use it when a Linux shell is enough, and use Tart or UTM when the agent needs macOS.

### Are secrets safe inside the agent's VM?

Not from the machine running it: that machine can read everything in the VM, including secrets, and a pool's machine can read commands and output passing through it. Pass in only what the task needs and join only pools whose owner you trust.

### Can the agent's VM reach my home or office network?

It depends on your setup. With Potluck, `potluck isolation test <size>` checks that a box cannot reach the host, its LAN or its tailnet.

### Can I rent out my Mac mini's macOS VMs?

No. Lending a macOS guest to your team or invited friends is fine, but selling Mac time to the public is not allowed under Apple's licence. This is not legal advice.

### Do I need a Sengi account to use Potluck?

No. Potluck is free, open source under Apache-2.0, and runs without Sengi or a Sengi account.
