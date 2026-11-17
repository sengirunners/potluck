---
slug: self-hosted-github-actions-runner-on-your-mac
kind: new
engine: claude
title: "How to run a self-hosted GitHub Actions runner on your own Mac, with a fresh VM for each job"
page_title: "Self-hosted GitHub Actions runner on a Mac | Potluck"
page_description: "Run GitHub Actions jobs on your own Mac or Mac mini, each in a fresh throwaway VM, so one job never touches your files or the next job."
target_searches:
  - "self-hosted github actions runner mac"
  - "github actions runner on mac mini"
  - "self-hosted macos runner fresh vm per job"
data_sources:
  - "Potluck README.md, docs/daemon.md (GitHub runner broker), docs/ci-pr.md, docs/hosts.md and docs/isolation.md, read November 2026"
  - "Potluck commands as printed by cli/runners.go and cli/pool.go"
drafted: 2026-11-16
model: claude-sonnet-5-5
date: 2026-11-16
updated: 2026-11-16
summary: "A plain self-hosted runner runs every job on your Mac itself. Potluck gives each GitHub Actions job its own fresh VM on your Mac and deletes it when the job ends."
---

You can run GitHub Actions jobs on a Mac you own by registering it as a self-hosted runner, but a plain runner runs every job directly on that Mac, so jobs share its files, its keychain and each other's leftovers. Potluck runs each job in its own fresh VM on your Mac or Mac mini and deletes the VM when the job ends. It is open source (Apache-2.0), and you set it up with a handful of commands.

## Why is a plain self-hosted runner on your Mac risky?

A standard self-hosted runner is a program on your Mac that runs each job directly on it. GitHub's own guidance (GitHub Docs, "Secure use reference", read November 2026) says self-hosted runners "do not have guarantees around running in ephemeral clean virtual machines" and "should almost never be used for public repositories", because anyone can open a pull request and compromise the environment.

Three things follow from that:

- **A job can read what your Mac holds.** Your other projects, your shell history, your SSH keys and any signed-in accounts are all inside the job's reach.
- **Jobs leave things behind.** A cache, a changed setting or a background process from one job can change the next, so a build passes or fails depending on what ran before it.
- **A bad pull request can damage the Mac.** Workflow code runs with the runner's permissions.

A plain runner is therefore a poor fit for a Mac you also sit at.

## What does a fresh VM for each job change?

A fresh VM changes where the job runs. Instead of running on your Mac, the job runs inside a virtual machine that Potluck starts for that one job and deletes afterward. Nothing the job writes survives, and the next job starts from the same clean image.

Potluck's isolation claim (docs/isolation.md in the repository) is that a job's VM cannot reach the lending Mac on any port (on Tart, apart from Softnet's DNS forwarder on the host), cannot reach its local network or its Tailscale network, cannot reach another VM running at the same time, cannot send mail, and holds none of your credentials. The test that checks this, `potluck isolation test`, has not yet been recorded on a real Mac; the repository lists that run as owed.

| | Plain self-hosted runner | Potluck, a fresh VM per job |
| --- | --- | --- |
| Where a job runs | Directly on your Mac | A VM on your Mac |
| Clean state for each job | Not guaranteed (GitHub Docs, read November 2026) | Yes |
| Can reach your files and keychain | Yes | No |
| Can reach your local network | Yes | No |

## How does Potluck run one job in one VM?

Potluck has a broker that watches for queued jobs in the GitHub repositories you connect. For each queued job it does this:

1. It asks GitHub for a one-job runner registration for that repository.
2. It starts a box, which is a fresh VM on one of the pool's machines (Tart VMs on a Mac), and hands the registration to the runner inside it.
3. The runner takes that one job. A GitHub runner of this kind takes one job only, so when the job ends the runner and its VM are removed.
4. A job-started hook inside the VM checks the job against the repository's rules and stops it if the rules refuse it.

Only code from people who can push to the repository is served. For a public repository, Potluck serves only its own branches, and pull requests from forks are never served. For a private repository, forks are refused unless you turn on `allow_fork_prs` in the broker's config.

## How do you set it up, step by step?

You need an Apple-silicon Mac with Tart and Softnet installed. The first `potluck host` on a Mac pulls a macOS image of about 27 GB and wants about 60 GB free. Linux boxes with Docker and Kata or gVisor work too, for Linux jobs.

### Step 1: Install Potluck

```sh
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
```

### Step 2: Check what the Mac can share

`potluck doctor` lists what is missing and prints the one `sudo` line it still needs.

```sh
potluck doctor
```

### Step 3: Start a pool on this Mac

A pool is the group of machines that run your jobs. With one machine, the pool and the host are the same computer.

```sh
potluck create ci --via tunnel
```

### Step 4: Share the Mac with the pool

On a Mac you sit at, a job ends when you touch the keyboard or pointer, and new jobs start only after five idle minutes. A Mac mini nobody sits at can share all the time:

```sh
potluck host --reclaim-idle-s 0
```

For Xcode jobs, `potluck host --with xcode` lends the pinned Cirrus Labs Xcode image instead of the base one, and the job asks for the extra label `xcode`. That image is a 67.5 GB download and wants about 200 GB free.

### Step 5: Turn on the runner broker

Creating and linking the GitHub App needs an https address that your browser is sent back to, which you forward to the listener Potluck opens. Serving jobs does not need it: with no public address the broker asks GitHub for queued jobs every 15 seconds.

```sh
potluck runners setup ci --url https://runners.example.com --repo acme/app
```

The command prints one URL. Open it to create a private GitHub App, install the App, and sign in to say yes to linking it. The App is private and installs only on the account that owns it, so add `--org acme` to create it under an organization when the jobs belong to one.

### Step 6: Connect each repository

```sh
potluck runners allow acme/app
```

A public repository needs `--public`, which is your yes to serve its own branches.

### Step 7: Point a job at the pool

The label is the pool's name, and a Mac job names `macos`:

```yaml
jobs:
  build:
    runs-on: [self-hosted, macos, ci]
```

Or let Potluck prepare the edit: `potluck ci pr acme/app` changes one job's `runs-on` from a GitHub-hosted macOS label and prints a script that opens the pull request. The change keeps GitHub's runner for fork pull requests.

### Step 8: Push a commit and look at what the broker did

```sh
potluck runners jobs
potluck ci doctor acme/app
```

`potluck ci doctor` is read-only. It runs numbered checks and gives one next step for each failure.

To add more machines later, the pool needs a way for them to reach this one. A pool made with `--via tunnel` and no tunnel configured works on this machine alone. A second machine needs either Tailscale (run `potluck create ci` again without `--via tunnel`, which is safe on an existing pool and brings Tailscale up, then `potluck invite <name> --host`) or a tunnel you configure in `~/.potluck/config.json` (see the README's "A tunnel instead of Tailscale", then `potluck invite <name> --host --via tunnel --pool ci`). On the other machine, run `potluck host '<code>'`. Jobs go to whichever machine is free.

## What does the Mac license allow?

Apple's macOS software license agreement (read November 2026) allows up to two macOS VMs on a Mac you own or control for uses such as development and testing, and its licence does not allow time-sharing or service-bureau use. The Potluck README's rule is that lending a macOS guest to your team or invited friends is fine and selling Mac time to the public is not. Potluck caps macOS guests at two per Mac. Read the license yourself; this is not legal advice.

## What works today and what does not?

Potluck is early, and its README lists what has been tested. A GitHub Actions job has been run through the broker by hand on a Mac with Tart, and on a Linux box on another machine. Potluck's own CI does not yet run Actions jobs on pool machines every day. The `runs-on` form that `potluck ci pr` writes uses GitHub's `fromJSON` function, and Potluck's docs say that form has not yet been run live. Interfaces will change.

## FAQ

### Can I use a Mac mini as a GitHub Actions runner?

Yes, if it is Apple silicon. Install Potluck, run `potluck create` and `potluck host --reclaim-idle-s 0`, and each job runs in a fresh Tart VM on the mini.

### Does every job really get a new VM?

Yes. The broker registers a one-job runner, starts a new box for it, and removes the runner and the box when the job ends. A VM does not carry files from one job to the next.

### Can a pull request from a fork run on my Mac?

Not by default. A fork's pull request is never served for a public repository, and for a private one it is refused unless you set `allow_fork_prs` in the broker's config.

### Do I need Tailscale?

No. A pool on one machine can use `--via tunnel`. Machines in a pool with friends reach each other over Tailscale by default, or over a tunnel you set up. Setting up the GitHub App needs an https address your browser can be sent back to, but serving jobs does not.

### Can the same pool run Linux jobs?

Yes, if a Linux box with Docker and Kata or gVisor joins the pool. A job that names no operating system is placed on Linux, as on GitHub's own runners, and a job that needs a Mac names `macos` in `runs-on`.

### What does it cost?

Potluck is open source under Apache-2.0 and has no price. The Mac and its power are the costs.

Code and docs: [github.com/sengirunners/potluck](https://github.com/sengirunners/potluck)
