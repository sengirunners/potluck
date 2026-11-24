# Tartelet vs Cilicon: ephemeral macOS GitHub Actions runners on Tart, autoscaling and Kubernetes (November 2026)

Published 2026-11-24. https://potluck.sh/blog/tart-based-github-actions-runners-compared-tartelet-cilicon

**Short answer:** Every Tart-based GitHub Actions runner tops out at two macOS VMs per Mac. Apple's licence allows two macOS VMs on a Mac you own, and the Tartelet, Cilicon and macos-ephemeral-runners READMEs all name the same limit in Apple's Virtualization framework. Tartelet is a macOS app that keeps up to two ephemeral runners going on one Mac. Cilicon is a macOS app that runs ephemeral VMs for GitHub Actions, GitLab Runner, Buildkite Agent or a script. KnobanLabs' macos-ephemeral-runners adds scale-from-zero autoscaling through GitHub's runner scale set API and runs Ubuntu ARM64 jobs beside macOS ones. None of the four tools here uses Kubernetes. To run more than two macOS jobs at once you add Macs, and Potluck, an early, free, open-source project, pools a team's spare Macs and Linux boxes for that.

Disclosure: Potluck is made by Sengi, which sells hosted GitHub Actions runners. Potluck is a separate free project and runs without Sengi.

## The four options at a glance

The details below come from each project's README as of November 2026, and from the Potluck facts for Potluck.

| | Tartelet | Cilicon | macos-ephemeral-runners | Potluck |
|---|---|---|---|---|
| What it is | macOS app | macOS app | Go controller run as a LaunchAgent | Daemon and CLI that pools machines |
| CI systems | GitHub Actions | GitHub Actions, GitLab Runner, Buildkite Agent, scripts | GitHub Actions | GitHub Actions through a runner broker, coding-agent runners, sandboxes |
| macOS VMs at once per Mac | Up to 2 | 1; running 2 is listed under future ideas | Capped at 2 | Capped at 2 |
| Linux jobs | No | No | Ubuntu ARM64 VMs on the same Mac | Linux boxes with Docker or Podman plus Kata or gVisor |
| Scaling model | Fixed slots, VM recreated after each job | One VM, recreated each cycle | Runner scale sets, scales from zero, optional warm VMs | Across every machine in a pool |
| Network isolation | Not covered in README | Not covered in README | Softnet isolated mode by default | Isolation check command; no network by default on the OCI layer |
| Status | Performance numbers dated January/February 2023 | Version 2.0; warns about macOS 15.0 to 15.3 | Marked experimental | Early; Actions job run by hand, not yet daily |

## Tartelet: two runners on one Mac, little to configure

Tartelet, from Shape in Denmark, is a macOS app built on Tart. Its README describes the cycle: Tart clones a VM, the VM boots, a setup script downloads the newest GitHub runner and registers it with your organization, the runner takes one job and removes itself, and Tartelet shuts down and deletes the VM before starting over.

What it does well: it is the shortest path from one Mac to two parallel, clean runners. The Tartelet README reports, on a 2020 Mac mini M1 with 16 GB of memory, jobs running 3 to 4 times faster than on GitHub's runners, about 12% slower per job when two VMs run in parallel, and 25 to 30 seconds to destroy and recreate a VM. It also notes that recreation overhead mostly disappears because the next VM boots while no job is waiting. Those numbers were last updated in January/February 2023, so treat them as a rough guide on current hardware.

Where it costs more: it is GitHub Actions only, it registers at the organization level, it runs on one Mac, and scaling past two jobs means another Mac with another copy of the app.

## Cilicon: one config file, several CI systems

Cilicon, from Trade Republic, is the project Tartelet credits as its inspiration. Version 2.0 dropped the login-item script inside the VM: Cilicon now connects over SSH and runs commands directly. It reads a cilicon.yml in the host user's home directory, pulls Tart-format images over OCI, and supports GitHub Actions, GitLab Runner, Buildkite Agent and arbitrary scripts as provisioners.

What it does well: if your team runs more than one CI system, Cilicon covers them with one tool. Its GitHub provisioner has a liveness probe that restarts the VM when a health command fails (by default every 30 seconds after a 60-second delay, per its README), and an SSH retry setting that defaults to 10 attempts.

Where it costs more: the README recommends a dedicated, freshly restored Mac with automatic login, automatic updates off and screen lock off. It lists running two VMs in parallel as a future idea, so plan on one job at a time per Mac. It warns of an SSH issue on macOS 15.0 to 15.3 and asks for 15.4 or later. GitHub use needs a GitHub App with self-hosted runner read and write permission at the organization level.

## macos-ephemeral-runners: autoscaling with scale sets

This is the closest match to the search for an autoscaling, ephemeral macOS runner. KnobanLabs' controller registers one GitHub runner scale set per pool, long-polls GitHub for waiting jobs, scales from zero, and gives each VM a just-in-time registration valid for one job. Its README gives boot times of about 8 seconds for Linux and about 30 seconds for macOS.

What it does well: macOS and Ubuntu ARM64 jobs share one Mac's memory and vCPU budget, one Mac can serve several organizations, and pools default to Tart's Softnet isolated network, which keeps VMs off the Mac and your local network. Its capacity table, with 7 GB per macOS job, 8 GB per Ubuntu job and 8 GB held back for the host, shows a 24 GB, 8-core Mac fitting two macOS jobs, and a 64 GB, 16-core Mac fitting two macOS plus five Ubuntu jobs.

Where it costs more: the README marks it experimental, notes that the scale set API is in public preview, and asks for Tart 2.30.0 or later, Homebrew, Go, about 93 GB of images, and a logged-in user session. It is for private repositories only. Scaling is still bounded by one Mac at a time per deployment.

## What about Kubernetes?

None of the four tools runs on Kubernetes, and the macOS side cannot move into a Linux cluster: every one of them runs macOS guests through Tart on Apple silicon Macs. Autoscaling macOS runners therefore comes in two layers. Inside one Mac, a controller such as macos-ephemeral-runners scales from zero up to two macOS guests plus whatever Linux VMs fit. Across Macs, you add machines. If your Linux runners already live in a cluster, keep them there; the question is how to get more Macs into rotation without a rack of dedicated hardware.

## Where Potluck fits

Potluck is not a scheduler for clusters and not a sandbox runtime of its own. It drives Tart on Apple silicon Macs and Docker or Podman with Kata or gVisor on Linux, and puts idle machines your team already owns into a pool. Each job gets a fresh VM or sandbox, and macOS guests are capped at two per Mac to match Apple's licence.

What is true today: a real GitHub Actions job has been run through Potluck's runner broker, by hand, on a Mac with Tart and on a Linux box on another machine with OCI and gVisor. It does not yet run daily in its makers' own CI. A Mac sandbox ends when its owner touches the keyboard or pointer, and when an owner takes a machine back, running VMs end and new work is refused. On Linux, hand-back works only as a time window today.

Trust matters more here than with a single dedicated Mac: the machine that runs a VM can read everything in it, including secrets, and the machine that runs a pool can read commands and output passing through it. Join only a pool whose owner you trust. Lending a macOS guest to your team or invited friends is fine; selling Mac time to the public is not, and neither is pooling iOS Simulator time. This is not legal advice.

If you want one Mac with two clean runners, Tartelet or Cilicon is less to set up. If you want autoscaling on one Mac, macos-ephemeral-runners is built for it. If you have several part-time Macs and Linux boxes on desks, Potluck is aimed at that. For more background, see [Potluck on Tart](/tart), [GitHub Actions runners with Potluck](/github-actions-runner) and [a self-hosted GitHub Actions runner on your Mac](/blog/self-hosted-github-actions-runner-on-your-mac).

## How to start with Potluck

Install on macOS or Linux (no Go needed) and start the daemon:

`curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh`

`potluck daemon --detach`

On the machine that runs the pool, create it and invite a teammate who will share a Mac. The first time, `create` prints a Tailscale login URL:

`potluck create friends`

`potluck invite carl --host`

On Carl's Mac, check what it can share and preview the plan, then share it:

`potluck doctor`

`potluck host --dry-run`

`potluck host '<code>'`

`potluck host` pulls the image, runs a quick conformance check per size, and joins only when a size passes. `--share` (default 0.5) sets the fraction of cores and RAM shared, and `potluck host stop` stops sharing. Run `potluck isolation test <size>` to check that a box cannot reach the host, its LAN or its tailnet. To try a sandbox by hand: `potluck sandbox create`, then `potluck sandbox exec <id> -- uname -a`, then `potluck sandbox rm <id>`. The longer walkthrough is in [pool spare Macs and Linux boxes for builds and agents](/blog/pool-spare-macs-and-linux-boxes-for-builds-and-agents).

## Try it early

Potluck is early. Its makers want people with a spare Mac or Linux box to try it and say what breaks. Start a thread at https://github.com/sengirunners/potluck/discussions with what you ran and what happened. If you want to contribute, the good first issues are at https://github.com/sengirunners/potluck/issues.

## Questions people ask

### How many macOS runners can one Mac run with Tart?

Two. Apple's licence allows up to two macOS VMs on a Mac you own, and the Tartelet, Cilicon and macos-ephemeral-runners READMEs all cite the same limit in Apple's Virtualization framework.

### Is Tartelet or Cilicon better for GitHub Actions?

Tartelet runs up to two GitHub Actions runners per Mac with little configuration. Cilicon supports GitHub Actions, GitLab Runner, Buildkite Agent and scripts from one config file, but its README lists running two VMs in parallel as a future idea.

### Can I autoscale macOS GitHub Actions runners on Kubernetes?

Not with these tools; all of them run macOS guests through Tart on Apple silicon Macs, outside Kubernetes. macos-ephemeral-runners scales from zero on one Mac through GitHub's runner scale set API, and beyond two macOS jobs you add Macs.

### Can Tart runners run Linux jobs too?

macos-ephemeral-runners runs Ubuntu ARM64 VMs on the same Mac as its macOS VMs. Potluck runs Linux sandboxes on Linux boxes with Docker or Podman plus Kata or gVisor.

### Are these runners safe for public repositories?

The macos-ephemeral-runners README says never to let public repositories use its runners, because anyone can open a pull request from a fork. The same caution applies to any self-hosted runner on a machine you care about.

### Does Potluck run GitHub Actions in production?

No. A real Actions job has been run through its runner broker by hand on a Mac and on a Linux box, but it is not yet daily in its makers' own CI.

### Does Potluck cost anything or need a Sengi account?

Potluck is free and open source under Apache-2.0, and it runs without Sengi and without a Sengi account. Sengi, its maker, sells hosted GitHub Actions runners as a separate product.
