# macOS Runners Cheaper Than GitHub Hosted, Plus Linux Runners With gVisor or Kata (November 2026)

Published 2026-11-27. https://potluck.sh/blog/macos-runners-cheaper-than-github-hosted

**Short answer**: the cheapest macOS runner is usually an Apple silicon Mac your team already owns. GitHub bills its hosted macOS runners by the minute on private repositories, while a self-hosted runner runs on hardware you have already paid for, so you pay for power, upkeep and your own time instead. For Linux, the safe pattern is a fresh sandbox per job on Docker or Podman with gVisor or Kata, so one job cannot leave files, processes or secrets behind for the next. Potluck, free and open-source (Apache-2.0) software made by Sengi, does both on machines you already own: macOS jobs in Tart VMs, capped at two per Mac to match Apple's licence, and Linux jobs in OCI sandboxes under gVisor or Kata. Potluck is early. A real GitHub Actions job has gone through its runner broker by hand on a Mac and on a Linux box with gVisor, but it does not yet run daily in our own CI.

Disclosure: Sengi, which makes Potluck, sells hosted GitHub Actions runners. Potluck is a separate free project that runs without Sengi and without a Sengi account. Check GitHub's pricing page for current per-minute rates; this post does not quote them.

## The options compared

| Option | What you pay for | Clean machine per job | Setup work | Where it costs more |
|---|---|---|---|---|
| GitHub-hosted macOS runner | Per-minute billing on private repositories | Yes, a fresh VM | None | Bill grows with every build minute |
| Official runner installed directly on a Mac | The Mac, power, your time | No, jobs share the host | Low | State and secrets build up between jobs |
| Tart-based runner tools (Tartelet, Cilicon and similar) | The Mac, power, your time | Yes, a fresh VM | Medium | You maintain images and the Macs |
| Actions Runner Controller on Kubernetes | Cluster nodes | Yes, a fresh pod | High | Needs a cluster; Linux only |
| Hosted runner vendors (Sengi and others) | A vendor's per-minute or plan pricing | Depends on vendor | None | Ongoing bill, someone else's hardware |
| Potluck | Machines you already own | Yes, a fresh VM or sandbox | Medium | Early; Actions broker tested by hand, not yet daily |

GitHub-hosted runners are the easiest choice: nothing to buy, a clean VM every time, and GitHub keeps the images current. Hosted vendors offer the same convenience with different hardware and pricing. The self-hosted rows trade that convenience for a lower running cost. For more detail on each, see [best self-hosted CI runners](/blog/best-self-hosted-ci-runners), [Tart-based runners compared](/blog/tart-based-github-actions-runners-compared-tartelet-cilicon) and [Actions Runner Controller](/blog/actions-runner-controller-github-actions-runner-controller-arc).

## Why a Mac you own is cheaper than a hosted macOS runner

A hosted macOS runner charges for every minute a job runs. A Mac on a desk is already paid for, and most team Macs sit idle at night, at weekends and during meetings. The extra cost of running builds on it is mostly electricity and the time someone spends keeping it working.

That time is real, and it is where self-hosting costs more:

- **Images.** You keep macOS and Xcode versions current yourself. GitHub does this for you on hosted runners.
- **Clean state.** The official runner installed straight on a Mac reuses the same user account for every job. Caches, keychains and leftover files carry over. A fresh VM per job fixes this, which is what Tart-based tools and Potluck do. See [self-hosted macOS runners with a fresh VM every job](/blog/self-hosted-macos-runners-fresh-vm-every-job).
- **Availability.** A laptop leaves the office. A desktop owner wants their machine back. Your builds have to cope.
- **Hardware failure.** When a disk dies, nobody else replaces it.

There is also a licence limit. Apple's macOS licence allows up to two macOS VMs on a Mac you own, so Potluck caps macOS guests at two per Mac. Lending those guests to your team or to invited friends fits the licence; selling Mac time to the public does not, and neither does pooling iOS Simulator time. This is not legal advice.

One more cost to watch: in December 2025 GitHub announced a per-minute platform charge for self-hosted runner usage on private repositories, then postponed it within a day, as Winbuzzer reported at the time. Check GitHub's billing documentation for the current state before you budget.

## How to run self-hosted GitHub Actions runners on a Linux box with gVisor or Kata

A plain Docker container shares the host's kernel. If a job escapes the container, or a malicious pull request runs code you did not expect, it is running on your box. gVisor puts a user-space kernel between the job and the host kernel. Kata runs each container inside a lightweight VM. Both plug into Docker or Podman as a container runtime, so the workflow stays the same.

The pattern, with or without Potluck:

1. Install Docker or Podman on the Linux box.
2. Install gVisor or Kata and register it as a runtime, following each project's own documentation.
3. Run each job in a new container under that runtime, and register the GitHub runner in ephemeral mode so it takes one job and exits.
4. Delete the container after the job, and start the next job from the clean image.
5. Cut the sandbox off from the host, your LAN and anything else you would not hand to a stranger.

Step 5 is the one people skip. With Potluck, `potluck isolation test <size>` checks that a box cannot reach the host, its LAN or its tailnet, and on the OCI layer the default is no network at all. Before a Linux box joins a pool, `potluck host` checks the machine, pulls the image and runs a quick conformance check for each sandbox size; the box joins only when a size passes. `potluck doctor` shows what the machine can share, and `potluck host --dry-run` prints the plan.

One Linux limit today: Potluck hands a Linux box back to its owner only as a time window. On a Mac, a sandbox ends when the owner touches the keyboard or pointer.

For how gVisor, Kata, containers and full VMs compare as isolation, read [the coding agent sandboxing guide](/blog/coding-agent-sandboxing-guide-containers-gvisor-vms-compared).

## What Potluck does today, and what it does not

What exists:

- A `potluck` program (the daemon and the commands), signed releases for macOS and Linux, and an install script.
- Pools: one machine runs the pool, friends join to run sandboxes, and other friends share their machines. Machines reach each other over Tailscale, or over a tunnel you configure.
- Sharing on an Apple silicon Mac with Tart, or on Linux with Docker or Podman plus Kata or gVisor.
- A runner broker for GitHub Actions. A real Actions job has been run through it by hand on a Mac (Tart) and on a Linux box on another machine (OCI with gVisor).
- `potluck agent-runner`, which runs a coding-agent vendor's self-hosted runners in fresh sandboxes.

What it is not yet: the Actions path is not running daily in our own CI. Potluck is also not a hosted cloud, not a sandbox runtime of its own (it drives Tart, Kata, gVisor and Incus) and not a scheduler for clusters. If you need a cluster scheduler, Actions Runner Controller is built for that. More on the runner side at [GitHub Actions runner](/github-actions-runner) and [Tart](/tart).

## Trust: who can see your jobs

Self-hosting moves trust onto people and machines you pick. With Potluck:

- The machine that runs a VM can read everything in it, including secrets.
- The machine that runs a pool can read commands and output passing through it.
- Join only a pool whose owner you trust.

Use it at your own risk; there are no promises. For a team pooling its own desks, this usually matches how trust already works. For public repositories that accept outside pull requests, think hard about which secrets a job can see.

## How to start with Potluck

Install on macOS or Linux with the one-line install script from the sengirunners/potluck repository on GitHub (the file is install.sh; download it with curl and pipe it to sh, no Go needed). Then start the daemon:

```
potluck daemon --detach
```

On the machine that will run the pool:

```
potluck create friends
```

The first time, it prints a Tailscale login URL. Then make invite codes:

```
potluck invite ana
potluck invite carl --host
```

`ana` gets a code to run sandboxes; `carl` gets a code to share a machine. On Carl's Mac or Linux box:

```
potluck doctor
potluck host --dry-run
potluck host '<code>'
```

The `--share` flag sets the fraction of cores and RAM shared; the default is 0.5. `potluck host stop` stops sharing. Ana joins with `potluck join '<code>'` and tries a sandbox:

```
potluck sandbox create
potluck sandbox exec <id> -- uname -a
potluck sandbox rm <id>
```

To give a coding agent the same sandboxes, `potluck --pool friends mcp` runs an MCP server with sandbox tools. The code lives in the sengirunners/potluck repository on GitHub. For a longer walkthrough, see [pool spare Macs and Linux boxes for builds and agents](/blog/pool-spare-macs-and-linux-boxes-for-builds-and-agents).

## Questions people ask

### Are self-hosted macOS runners cheaper than GitHub-hosted ones?

Usually, if you already own the Macs and they sit idle. You stop paying per minute and start paying in upkeep time: images, Xcode versions and hardware.

### How many macOS VMs can I run on one Mac?

Apple's licence allows up to two macOS VMs on a Mac you own, and Potluck caps macOS guests at two per Mac. This is not legal advice.

### Can I sell spare Mac build time with Potluck?

No. Lending macOS guests to your team or invited friends is fine; selling Mac time to the public is not, and neither is pooling iOS Simulator time.

### Should I use gVisor or Kata for Linux runners?

Both work with Docker or Podman, and Potluck can share a Linux box with either. gVisor adds a user-space kernel in front of the host kernel, while Kata runs each container in a lightweight VM; run `potluck doctor` to see what your box can share.

### Is Potluck ready to replace my GitHub-hosted runners?

Not yet. It is early: a real Actions job has run through the runner broker by hand on a Mac and on Linux with gVisor, but it is not yet in daily use in our own CI.

### Do I need a Sengi account to use Potluck?

No. Potluck runs without Sengi and without a Sengi account; Sengi, which makes it, sells hosted runners as a separate product.

### What happens if a Mac owner sits down mid-build?

The Mac sandbox ends when the owner touches the keyboard or pointer. When an owner takes a machine back, running VMs end and new work is refused.
