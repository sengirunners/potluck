---
slug: self-hosted-github-actions-runner-alternatives-blacksmith-cirrus
kind: new
engine: claude
title: "Self-Hosted GitHub Actions Runner Alternatives to Blacksmith and Cirrus Runners (November 2026)"
page_title: "Blacksmith and Cirrus Runners Alternatives 2026 | Potluck"
page_description: "Self-hosted alternatives to Blacksmith and Cirrus Runners, November 2026: GitHub's runner app, ARC, Tart and Potluck on machines you own."
target_searches:
  - "self-hosted GitHub Actions runner alternatives Blacksmith Cirrus Runners"
  - "GitHub self-hosted runner pricing per minute 2026"
  - "self-hosted GitHub Actions runner alternatives Cirrus Runners Blacksmith"
  - "self-hosted GitHub Actions runner alternatives to Blacksmith Cirrus Runners"
  - "Cirrus Runners product Cirrus CI Cirrus Runners self-hosted"
  - "what are the alternatives to Cirrus Runners and Blacksmith if I have my own hardware"
  - "can I run GitHub Actions jobs on spare Macs instead of paying for hosted macOS runners"
data_sources:
  - "AI search x1"
  - "AI search x4"
drafted: 2026-11-23
model: claude-opus-5-5
date: 2026-11-24
updated: 2026-11-24
---

**Short answer**: if you already own Macs or Linux boxes, the alternatives to Blacksmith and Cirrus Runners are GitHub's own self-hosted runner, Actions Runner Controller on a Kubernetes cluster, or a pool such as Potluck that runs each job in a fresh VM or sandbox on machines your team owns. GitHub's billing documentation describes self-hosted runner usage as free of a per-minute charge as of autumn 2026, so on your own hardware you pay for the machines, the power and your time rather than a rate per minute. Blacksmith and Cirrus Runners are managed services: you pay them for machines they run, and in return you do no hardware work. Potluck is free and open source, made by Sengi, Inc., which also sells hosted GitHub Actions runners, so weigh our view with that in mind. Potluck is early: a real Actions job has run through it by hand on a Mac and on a Linux box, but it does not yet run our own CI every day.

## The options side by side

This table compares the options for someone deciding between paying for hosted runners and using hardware they already have. Prices change often, so check each vendor's pricing page on the day you decide; we only describe how each one bills.

| Option | Who runs the hardware | How you pay | macOS jobs | Linux jobs | Fresh machine per job | Work for you |
|---|---|---|---|---|---|---|
| GitHub-hosted runners | GitHub | Per minute after plan allowance | Yes | Yes | Yes | None |
| Blacksmith | Blacksmith | Per minute, with a free monthly allowance | Yes, Apple silicon | Yes, x64 and ARM | Yes, microVMs | Install an app, change the label |
| Cirrus Runners | Cirrus Labs | Flat fee per runner per month | Yes, Apple silicon | Check their site | Yes | Install an app, change the label |
| GitHub self-hosted runner app | You | Your machines and time | Yes | Yes | Only if you build it | Patching, cleanup, isolation |
| Actions Runner Controller | You, on Kubernetes | Your cluster and time | No, Linux containers | Yes | Yes, ephemeral pods | Run and upgrade a cluster |
| Potluck (early) | You and your team | Free; your machines | Yes, via Tart | Yes, via Kata or gVisor | Yes, VM or sandbox per job | Install, host, join a pool |

## Blacksmith: what it does well and what it costs you

Blacksmith is a managed runner service for GitHub Actions focused on speed and price. You install its GitHub App and change `runs-on` to a Blacksmith label. Jobs run in microVMs on Linux x64 and ARM, with macOS on Apple silicon and Windows not yet generally available, and it adds a colocated cache, Docker layer caching on persistent disks, and SSH into running jobs. It bills per minute with a free monthly allowance.

It costs more than self-hosting when you already own idle machines, because you pay for every minute on hardware someone else runs. It costs less in your time: there is nothing to patch and no box under a desk to reboot.

## Cirrus Runners and Cirrus CI: which is which

Cirrus Labs has made several products, and the names get mixed up in searches:

- **Cirrus CI** is a full CI service with its own configuration file, separate from GitHub Actions.
- **Cirrus Runners** are managed Apple silicon machines that plug into GitHub Actions as runners. They are billed as a flat monthly fee per runner instead of per minute, which suits teams with steady macOS load.
- **Tart** is Cirrus Labs' open-source tool for running macOS and Linux VMs on Apple silicon. Potluck drives Tart on Macs; see [Tart](/tart).

Cirrus Labs has announced changes to Cirrus CI over time, so check their site for what is offered today before you plan around either product. If the reason you looked at Cirrus Runners was "fresh macOS VMs for Actions jobs", the self-hosted version of that is Tart on a Mac you own, which is what Potluck sets up for you.

## What self-hosting costs per minute in 2026

The search "GitHub self-hosted runner pricing per minute 2026" has a short answer and a caveat.

- **GitHub's charge**: GitHub's billing documentation describes self-hosted runner usage as free. GitHub did announce a per-minute platform charge for self-hosted usage in private repositories and then postponed it to re-evaluate. It has not been cancelled, so check GitHub's billing page and budget for it returning in some form.
- **Your hardware**: a Mac or Linux box you already own and leave idle overnight costs you power, not a new purchase.
- **Your time**: this is the real cost. GitHub's runner app does not give you a clean VM per job; leftover files, caches and processes carry from one job to the next unless you build cleanup. GitHub also warns against self-hosted runners on public repositories, because a pull request from a stranger runs on your machine.

Potluck's aim is to cut the time cost: each job gets a fresh VM or sandbox, and the machine joins a pool only after it passes a conformance check.

## Can you run GitHub Actions on spare Macs instead of hosted macOS runners?

Yes. macOS minutes are the most expensive kind on hosted services, and a spare Apple silicon Mac is often sitting idle. There are three ways to do it:

1. **GitHub's runner app directly on the Mac.** Fastest to set up, no isolation between jobs. We walk through it in [a self-hosted GitHub Actions runner on your Mac](/blog/self-hosted-github-actions-runner-on-your-mac).
2. **Tart VMs you script yourself.** Each job gets a fresh macOS VM; you write the glue that starts the VM, registers the runner and deletes it.
3. **Potluck.** It drives Tart for you and hands each job a fresh VM. A real Actions job has been run through Potluck's runner broker by hand on a Mac with Tart, and on a Linux box on another machine with gVisor. It is not yet running in our own CI every day, so treat it as early. See [GitHub Actions runner](/github-actions-runner) for the current state.

Two limits apply to any Mac option:

- **Apple's licence** allows up to two macOS VMs on a Mac you own, and Potluck caps macOS guests at two per Mac. Lending a macOS guest to your team or invited friends is fine; selling Mac time to the public is not, and neither is pooling iOS Simulator time. This is not legal advice.
- **Owners come first.** In Potluck, a Mac sandbox ends when its owner touches the keyboard or pointer. A build on a Mac someone is using will be cut off, so pool Macs that sit idle, such as an old Mac mini or a laptop overnight.

## What Potluck is and is not

Potluck puts a team's idle Macs and Linux boxes to work, running GitHub Actions jobs and coding agents on machines you already own, each in a fresh VM or sandbox. It is not a hosted cloud, it is not a sandbox runtime of its own (it drives Tart, Kata, gVisor and Incus), and it is not a cluster scheduler. It runs without Sengi and without a Sengi account.

To share a machine you need Tart on an Apple silicon Mac, or Docker or Podman with Kata or gVisor on Linux. Machines reach each other over Tailscale, or over a tunnel you configure. When an owner takes a machine back, running VMs end and new work is refused. Hand-back when the owner returns works on a Mac; on Linux it works only as a time window today.

Trust matters here. The machine that runs a VM can read everything in it, including secrets passed to a build. The machine that runs the pool can read commands and output passing through it. Join only a pool whose owner you trust, and use it at your own risk. For more on pooling, read [pool spare Macs and Linux boxes for builds and agents](/blog/pool-spare-macs-and-linux-boxes-for-builds-and-agents).

## Which option fits

- **No spare hardware, want zero upkeep:** GitHub-hosted runners or Blacksmith.
- **Steady macOS load, want a fixed monthly bill:** a flat-fee managed Mac service such as Cirrus Runners, if it is offered when you read this.
- **A Kubernetes cluster already running, Linux jobs only:** Actions Runner Controller.
- **One spare Mac, a private repository, and you accept shared state between jobs:** GitHub's runner app on the Mac.
- **Several idle Macs and Linux boxes across a team, want a fresh VM per job, willing to try early software:** Potluck.

You can mix them. The runner is chosen per job by the `runs-on` label, so Linux jobs can stay on a hosted service while macOS jobs move to a Mac you own.

## How to start with Potluck

Install on macOS or Linux (no Go needed) and start the daemon:

```
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
potluck daemon --detach
```

On the machine that will run the pool, create it. The first time, it prints a Tailscale login URL. Then make an invite code for a friend who will share a machine:

```
potluck create friends
potluck invite carl --host
```

On the spare Mac or Linux box, check what it can share, look at the plan, then host:

```
potluck doctor
potluck host --dry-run
potluck host '<code>'
```

`potluck host` checks the machine, pulls the image, runs a quick conformance check per size and joins the pool only when a size passes. `--share` sets the fraction of cores and RAM shared, 0.5 by default. `potluck isolation test <size>` checks that a box cannot reach the host, its LAN or its tailnet; on the OCI layer the default is no network at all. `potluck host stop` stops sharing.

To try a sandbox by hand:

```
potluck sandbox create
potluck sandbox exec <id> -- uname -a
potluck sandbox rm <id>
```

For coding agents, `potluck --pool friends mcp` exposes sandbox tools over MCP, and `potluck agent-runner` runs a coding-agent vendor's self-hosted runners in fresh sandboxes. See [agent sandbox](/agent-sandbox).

## Try it early

Potluck is early. Its makers want people with a spare Mac or Linux box to install it, host a machine, run a job and say what breaks. Start a thread at https://github.com/sengirunners/potluck/discussions with your hardware and what happened. If you want to contribute code, the good first issues are at https://github.com/sengirunners/potluck/issues.

## Questions people ask

### What are the alternatives to Cirrus Runners and Blacksmith if I have my own hardware?

GitHub's self-hosted runner app, Actions Runner Controller on Kubernetes, scripted Tart VMs on Macs, or Potluck, which runs each job in a fresh VM or sandbox on machines your team owns. All of them use hardware you already pay for instead of a per-minute or per-runner fee.

### Does GitHub charge per minute for self-hosted runners in 2026?

GitHub's billing documentation describes self-hosted runner usage as free of a per-minute charge as of autumn 2026. GitHub announced a platform charge for self-hosted usage and postponed it, so check its billing page before you budget.

### What is the difference between Cirrus CI and Cirrus Runners?

Cirrus CI is a separate CI service with its own configuration, while Cirrus Runners are managed Apple silicon machines that run GitHub Actions jobs for a flat monthly fee per runner. Cirrus Labs also makes Tart, the VM tool Potluck uses on Macs.

### How many macOS VMs can I run on one Mac?

Apple's licence allows up to two macOS VMs on a Mac you own, and Potluck caps macOS guests at two per Mac. Lending them to your team is fine; selling Mac time to the public is not. This is not legal advice.

### Can Potluck replace Blacksmith for my whole CI today?

Not yet. A real Actions job has run through Potluck by hand on a Mac and on a Linux box, but it is not yet running in our own CI every day, so start with a few jobs, not your main pipeline.

### Is it safe to run builds on a teammate's machine?

Only if you trust them. The machine that runs a VM can read everything in it, including secrets, and the pool's machine can read commands and output, so join only pools whose owner you trust.

### What happens if the Mac's owner sits down during a build?

The Mac sandbox ends as soon as the owner touches the keyboard or pointer, and the job fails. Pool Macs that are idle for long stretches.
