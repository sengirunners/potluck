---
slug: tart
title: "Tart VMs for CI and agents: what Tart does, its licence, and what Potluck adds"
page_title: "Tart VMs for CI and agents: licence, limits, and what Potluck adds | Potluck"
page_description: "Tart runs macOS and Linux VMs on Apple silicon. What it does, its FSL-1.1-ALv2 licence, Apple's two-VM limit, and what Potluck adds to use it for GitHub Actions jobs and coding agents."
target_searches:
  - "tart vm"
  - "tart macos vm"
  - "tart virtualization licence"
  - "tart ci runner"
data_sources:
  - "Tart's repository github.com/openai/tart: README and LICENSE, read 2026-11-17"
  - "Apple's software licence agreement for macOS, as shipped with macOS 27.2.1, section 2B(iii), read 2026-11-17"
  - "Potluck README (What you need, Macs), docs/isolation.md, vmhost/config.go"
date: 2026-11-17
updated: 2026-11-17
summary: "Tart runs macOS and Linux VMs on Apple-silicon Macs. It is fair-source (FSL-1.1-ALv2) and lives at github.com/openai/tart. Potluck uses it to give each GitHub Actions job or coding agent a fresh VM on your team's idle Macs."
---

## What Tart does

Tart's README describes it as a virtualization toolset to build, run and manage macOS and Linux virtual machines on
Apple silicon, built by CI engineers for automation. It uses Apple's own Virtualization framework, so VMs run at close
to native speed. VM images are pushed to and pulled from any OCI-compatible container registry, the same kind of
registry that holds Docker images, and a Packer plugin builds them. Its site is [tart.run](https://tart.run).

Tart was started at Cirrus Labs. Its repository is now [github.com/openai/tart](https://github.com/openai/tart)
(the old cirruslabs/tart address leads there), and its licence names OpenAI as the copyright holder.

## Tart's licence: FSL-1.1-ALv2

Tart's LICENSE file is the Functional Source License, version 1.1, with Apache 2.0 as its future licence
(FSL-1.1-ALv2). In short:

- You may use, copy, change and redistribute Tart for any purpose except a "Competing Use", which the licence defines
  as "making the Software available to others in a commercial product or service that: 1. substitutes for the
  Software; 2. substitutes for any other product or service we offer using the Software that exists as of the date
  we make the Software available; or 3. offers the same or substantially similar functionality as the Software".
- The licence names internal use, non-commercial education and research, and professional services to someone who
  holds a licence as permitted.
- Each version becomes available under Apache 2.0 two years after it is made available.

Running Tart inside your own team to test your own code is internal use. Whether a particular business counts as a
Competing Use is a question for the licence text and your lawyer, not this page.

## Apple's limit: two macOS VMs per Mac

Apple's software licence agreement for macOS (section 2B(iii), in the copy shipped with macOS 27.2.1) allows up to
two additional copies or instances of macOS in virtual machines on each Apple-branded computer you own or control,
for software development, testing during software development, macOS Server, or personal non-commercial use. The
same agreement does not allow those VMs to be used for service-bureau, time-sharing, terminal-sharing or relay
services. Linux VMs on a Mac are not affected.

Potluck caps macOS guests at two per Mac. Its rule for lending: a macOS VM for people on your team or friends you
invite is fine; selling Mac time to the public is not, and neither is pooling iOS Simulator time
([the README's Macs section](https://github.com/sengirunners/potluck#macs)). That is Potluck's makers' reading of
the licence.

## Potluck does not bundle Tart

Potluck's installer puts `potluck`, its drivers, `potluck-conformance` and `potluck-e2b` on your machine, and nothing
else. On a Mac, you install Tart and Softnet (the helper Tart uses to give a VM an isolated network; it is setuid
root) yourself, and accept Tart's licence yourself. `potluck doctor` says what is missing and where to get it,
including the one sudo line Softnet needs. A Mac also needs about 60 GB free: the first `potluck host` pulls a macOS
image of about 27 GB.

## What Potluck adds on top of Tart

Tart makes and runs VMs. Potluck decides which VM to make, for whom, on which machine, and when to end it:

- **A fresh VM for every job.** Each GitHub Actions job or sandbox gets its own VM, thrown away when it is done.
- **Your team's machines in one pool.** Teammates lend their Macs and Linux boxes; work goes to whichever is free.
- **The Mac goes back to its owner.** By default a lent Mac works for the pool only while its owner is away: touch
  the keyboard or trackpad and its job ends. VMs also end at their deadline, even if Potluck's daemon is down.
- **GitHub Actions jobs.** A broker takes queued jobs from the repositories you connect and runs each in a fresh VM,
  refusing runs from forks ([self-hosted GitHub Actions runner](/github-actions-runner)).
- **Sandboxes for coding agents.** `potluck sandbox create`, `potluck sandbox exec`, or the same tools over MCP.
- **Checks before a machine joins.** `potluck host` checks the machine and runs a quick conformance check for each
  size before sharing it.

On one Mac, the quick start is:

```sh
potluck doctor                          # what this machine can share, and what is missing
potluck create trial --via tunnel       # a pool on this machine alone
potluck host --reclaim-idle-s 0         # share this machine with that pool
potluck sandbox create                  # prints the sandbox's id
potluck sandbox exec <id> -- uname -a
potluck sandbox rm <id>
```

This one-machine path has been run end to end by hand many times on an Apple-silicon Mac with Tart.

Facts on this page were checked against the sources named in each section on November 17, 2026. Licences change;
read them yourself. This is not legal advice.
