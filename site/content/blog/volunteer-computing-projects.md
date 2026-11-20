---
slug: volunteer-computing-projects
kind: new
engine: claude
title: "Volunteer computing projects in November 2026: BOINC, GPU projects and self-hosted pools"
page_title: "Volunteer Computing Projects, November 2026 | Potluck"
page_description: "Volunteer computing projects in November 2026: BOINC projects, GPU projects, open-source alternatives, and how to pool your own idle machines instead."
target_searches:
  - "volunteer computing projects 2026"
  - "BOINC volunteer computing projects 2026"
  - "BOINC volunteer computing projects"
  - "volunteer computing BOINC GPU projects 2026"
  - "open source peer-to-peer distributed computing volunteer compute software"
  - "BOINC alternatives distributed computing volunteer idle computers"
  - "self-hosted distributed computing pool join volunteer compute"
  - "volunteer computing projects"
  - "volunteer computing projects in 2026"
data_sources:
  - "AI search x4"
  - "AI search x2"
  - "AI search x1"
  - "Brave term"
  - "answer-rank 2026-11-20"
drafted: 2026-11-20
model: claude-opus-5-5
date: 2026-11-20
updated: 2026-11-20
---

**Short answer:** Most volunteer computing projects in November 2026 still run on BOINC, the open-source platform from the University of California, Berkeley. The BOINC article on Wikipedia, citing BOINCstats, counts 25,314 active participants on 130,853 computers averaging 15.220 PetaFLOPS a day as of 13 August 2026, and 26 projects on BOINC's official list as of 7 February 2026. Long-running BOINC projects include Einstein@Home, Rosetta@home, World Community Grid, PrimeGrid, LHC@home, climateprediction.net, MilkyWay@home, Asteroids@home and GPUGRID. Outside BOINC, Folding@home, GIMPS and distributed.net run their own clients, and peer-to-peer tools such as Petals, Hivemind and AI Horde pool volunteer GPUs for AI models. If you want to pool your own team's idle Macs and Linux boxes for builds and coding agents instead of donating them to science, that is a different job. Potluck, a free, open-source and early project made by Sengi, does that.

Volunteer and project counts change month to month. The Wikipedia "List of volunteer computing projects" and BOINC's own "Choosing BOINC projects" page carry the current figures.

## The options compared

| Option | What your machine works on | Who uses the result | GPU work | How you take part |
|---|---|---|---|---|
| BOINC projects | Science tasks: astrophysics, biology, maths, climate, physics | The research group that runs each project | In some projects (see below) | Install the BOINC client, attach to projects or use an account manager |
| Folding@home | Protein folding simulations | Folding@home research labs | CPU and GPU | Its own client |
| GIMPS | Tests of Mersenne prime candidates | The GIMPS project | CPU, plus separate GPU programs | Its own client |
| distributed.net | Cipher and Golomb ruler searches | distributed.net | CPU and GPU clients | Its own client |
| Petals and Hivemind | Parts of large language models | Anyone using the swarm | GPU | Run a Petals server or Hivemind peer |
| AI Horde | Image and text generation | Users of the horde, with kudos as a reward | GPU | Run a worker |
| Golem Network | Tasks bought on a marketplace | Paying requestors | Depends on the task | Run a provider node |
| Potluck | GitHub Actions jobs and coding-agent sandboxes | Your team or friends you invite | Not covered so far; you share a fraction of cores and RAM | Join or host in a private pool |

Disclosure: Potluck is made by Sengi, Inc., which sells hosted GitHub Actions runners. Potluck is a separate free project and runs without Sengi or a Sengi account.

## BOINC volunteer computing projects in 2026

BOINC is middleware: a server that hands out work units and a client that runs them when your computer is idle. The client runs on Windows, macOS, Linux, Android, FreeBSD and Raspberry Pi OS, and the code is under the LGPL. A credit system checks returned results before it grants credit, which is how projects catch faulty hardware and cheating.

The well-known active projects, by focus:

- **Einstein@Home**: searches radio and gravitational-wave data for pulsars.
- **Rosetta@home**: protein structure prediction for disease research.
- **World Community Grid**: humanitarian research on disease, clean water and clean energy, now run by the Krembil Research Institute.
- **PrimeGrid, SRBase, NumberFields@home, yoyo@home**: prime searches, Sierpinski and Riesel problems, number theory and factoring.
- **LHC@home**: physics simulations for CERN.
- **climateprediction.net**: climate model runs from Oxford.
- **MilkyWay@home**: a model of our galaxy from Sloan Digital Sky Survey data.
- **Asteroids@home**: asteroid shapes and spins from photometry.
- **SiDock@home and TN-Grid**: drug design and gene networks.

Recent additions to the Wikipedia list include Kessler@home, which simulates orbital debris growth; Beacon@Home, which runs light citizen-science tasks such as mapping background radiation with volunteer Geiger counters; and Eternity II, which searches for better arrangements of the edge-matching puzzle. BitBOINC and The Puzzle also appear; both include Bitcoin challenge-key searches, so read a project's About page before you attach and decide whether that is work you want to give cycles to. BOINC Central is a shared pool hosted at Berkeley so researchers can submit jobs without running their own server.

Some names have ended. SETI@home stopped in 2020, and the list marks Universe@Home, Amicable Numbers and DreamLab as completed.

If you do not want to pick projects yourself, an account manager does it. Science United assigns vetted projects by research area such as medicine or physics; BAM! and GridRepublic manage several accounts across several computers.

## GPU volunteer computing projects

BOINC added NVIDIA CUDA support first, then AMD, then OpenCL on Windows, Linux and macOS; Intel GPU work exists but is sparse, per the BOINC article on Wikipedia, which also says GPU applications run 2 to 10 times faster than the earlier CPU-only versions. Projects that use GPUs:

- **GPUGRID**: full-atom protein simulations, NVIDIA GPUs only.
- **Einstein@Home, PrimeGrid, World Community Grid**: GPU and CPU applications.
- **MilkyWay@home**: built NVIDIA CUDA applications early on.
- **Folding@home**: its own client, CPU and GPU.

GPU work availability comes and goes. Before you buy or dedicate a card, check the project's Applications page for your GPU vendor and operating system.

For AI workloads, the GPU pools are peer to peer. Petals serves large language models across volunteer GPUs. AI Horde is a crowdsourced GPU cluster for image and text generation, run by a non-profit. Exo is different: it joins your own devices into one cluster at home rather than lending to strangers.

## Open-source and peer-to-peer alternatives to BOINC

BOINC is the most complete option if you are a researcher: it has a server you can host, a credit system, account managers and a large volunteer base. The cost is that you package your application as BOINC work units and run a project server, which takes real effort.

Other open-source approaches:

- **Hivemind**: a PyTorch library for training models over the internet without central coordination. Good for ML research; it assumes ML work.
- **Pando and Wasimoff**: browser-based volunteer computing, where tasks run in web workers. No install for volunteers; limited to what runs in a browser.
- **Flower**: federated learning across devices that keep their own data. It is related to volunteer computing but solves a privacy problem, not a capacity one.
- **Golem Network**: a permissionless marketplace for compute. It pays providers, which brings a different trust and payment model than donation.

## When you want a self-hosted pool instead

Searches like "self-hosted distributed computing pool join volunteer compute" often come from developers who are not after science credit. They have a Mac mini under a desk and a Linux box in a closet, both idle most of the day, and a CI queue or a coding agent that wants somewhere to run. Volunteer computing sends cycles to a project you do not run. A private pool keeps the cycles inside a group you choose.

That is what Potluck does. It runs GitHub Actions jobs and coding agents on machines your team already owns, each in a fresh VM or sandbox. Details from the project facts:

- **What a host needs**: Tart on an Apple silicon Mac, or Docker or Podman with Kata or gVisor on Linux. `potluck host` checks the machine, pulls the image, runs a quick conformance check per size and joins the pool only when a size passes.
- **How much you share**: `--share` sets the fraction of cores and RAM, default 0.5.
- **Getting the machine back**: a Mac sandbox ends when its owner touches the keyboard or pointer. When an owner takes a machine back, running VMs end and new work is refused. On Linux, hand-back works only as a time window today.
- **GitHub Actions status**: a real Actions job has been run through the runner broker, by hand, on a Mac (Tart) and on a Linux box on another machine (OCI with gVisor). It is not yet running daily in our own CI. See [GitHub Actions runner](/github-actions-runner) and [a self-hosted GitHub Actions runner on your Mac](/blog/self-hosted-github-actions-runner-on-your-mac).
- **Agents**: `potluck --pool friends mcp` is an MCP server that gives an agent sandbox tools; see [agent sandbox](/agent-sandbox) and [give your AI agent a sandbox over MCP](/blog/give-your-ai-agent-a-sandbox-over-mcp). `potluck agent-runner` runs a coding-agent vendor's self-hosted runners in fresh sandboxes.
- **Isolation**: `potluck isolation test <size>` checks that a box cannot reach the host, its LAN or its tailnet. On the OCI layer the default is no network at all.
- **Trust**: the machine that runs a VM can read everything in it, secrets included, and the machine that runs a pool can read commands and output passing through it. Join only a pool whose owner you trust. There are no promises; use it at your own risk.
- **Macs**: Apple's licence allows up to two macOS VMs on a Mac you own, and Potluck caps macOS guests at two per Mac. Lending a guest to your team or invited friends is fine; selling Mac time to the public is not, and neither is pooling iOS Simulator time. This is not legal advice. More in [Tart](/tart).

Potluck is not a hosted cloud, not a sandbox runtime of its own (it drives Tart, Kata, gVisor and Incus) and not a cluster scheduler. For the longer walkthrough, read [pool spare Macs and Linux boxes for builds and agents](/blog/pool-spare-macs-and-linux-boxes-for-builds-and-agents).

## How to start with Potluck

Install on macOS or Linux with the install script, install.sh, from the main branch of the sengirunners/potluck repository on GitHub: fetch it with curl and pipe it to sh. No Go is needed. Then start the daemon:

```
potluck daemon --detach
```

On the machine that runs the pool:

```
potluck create friends
potluck invite ana
potluck invite carl --host
```

The first `potluck create` prints a Tailscale login URL. Machines reach each other over Tailscale or over a tunnel you configure. Ana runs `potluck join '<code>'` to use sandboxes; Carl runs `potluck host '<code>'` to share a machine. Before sharing, `potluck doctor` shows what a machine can share and `potluck host --dry-run` prints the plan. `potluck host stop` stops sharing.

Try a sandbox:

```
potluck sandbox create
potluck sandbox exec <id> -- uname -a
potluck sandbox rm <id>
```

The code is in the sengirunners/potluck repository on GitHub, under Apache-2.0.

## Questions people ask

### What are the main volunteer computing projects in 2026?

On BOINC: Einstein@Home, Rosetta@home, World Community Grid, PrimeGrid, LHC@home, climateprediction.net, MilkyWay@home and Asteroids@home. Outside BOINC: Folding@home, GIMPS and distributed.net, plus GPU pools for AI such as Petals and AI Horde.

### How many BOINC projects are there?

BOINC's official list had 26 projects as of 7 February 2026, according to the BOINC article on Wikipedia. More BOINC projects run outside the official list, and the Wikipedia list of volunteer computing projects tracks many of them.

### Which BOINC projects use the GPU?

GPUGRID (NVIDIA only), Einstein@Home, PrimeGrid and World Community Grid have GPU applications, and MilkyWay@home has had them for years. Check each project's Applications page for your card and operating system, since GPU work is not always available.

### Is BOINC still maintained?

Yes. The BOINC article on Wikipedia lists client release 8.2.11 for Windows and macOS in April 2026 and 8.2.13 for Linux in May 2026, and Berkeley runs BOINC Central as a shared project.

### What is a good alternative to BOINC for my own machines?

If you want to keep the compute in your own group rather than donate it, run a private pool. Potluck pools a team's Macs and Linux boxes for GitHub Actions jobs and coding-agent sandboxes; for ML training, Hivemind is the closer fit.

### Is Potluck a volunteer computing project?

No. It does not send work to science projects; it lets you and people you invite share idle machines for builds and agents, each job in a fresh VM or sandbox.

### Is it safe to run other people's work on my computer?

BOINC projects run code from the research group you attach to, so pick projects you trust. In Potluck, work runs in a VM or sandbox and `potluck isolation test <size>` checks network isolation, but the trust rules still apply: join or host only with people you trust.

### Can I share only part of a machine?

In BOINC you set CPU and GPU limits in the client preferences. In Potluck, `--share` sets the fraction of cores and RAM, default 0.5, and a Mac sandbox ends as soon as the owner touches the keyboard or pointer.
