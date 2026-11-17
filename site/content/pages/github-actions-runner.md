---
slug: github-actions-runner
title: "Self-hosted GitHub Actions runner with a fresh VM for every job"
page_title: "Self-hosted GitHub Actions runner, a fresh VM per job | Potluck"
page_description: "Run GitHub Actions jobs on your team's idle Macs and Linux boxes, each job in a fresh VM or sandbox thrown away after it. Runs from forks are refused unless you allow them on a private repository. Free and open source."
target_searches:
  - "github self hosted runner"
  - "github actions self hosted runner"
  - "ephemeral self hosted runner"
  - "self hosted runner fresh vm per job"
  - "github actions macos runner"
data_sources:
  - "Potluck README, docs/ci-pr.md, docs/ci-doctor.md, docs/daemon.md (GitHub runner broker)"
  - "GitHub Docs, Actions billing and runner pricing, read 2026-11-17"
date: 2026-11-17
updated: 2026-11-17
summary: "Every GitHub Actions job gets a fresh VM on one of your team's idle Macs or Linux boxes, and the VM is thrown away when the job ends. Free and open source (Apache-2.0)."
---

A self-hosted runner that lives on one machine for months collects state: caches, half-installed tools, files a
job left behind, and anything a bad dependency dropped there. Potluck gives every job a fresh machine instead, on
hardware your team already owns.

## How a job runs

1. One machine in your pool runs Potluck's runner broker. It is connected to GitHub through a GitHub App you create,
   and it watches the repositories you connect. With a public address it takes GitHub's webhooks and catches up by
   asking GitHub every 5 minutes; with none, it asks GitHub for queued jobs every 15 seconds.
2. A queued job whose `runs-on` names your pool's label (for a pool called `friends`: `runs-on: friends`, or
   `friends:<size>` for a size, with or without `self-hosted`) is the pool's. A job that names no OS goes to Linux,
   as on GitHub's own runners.
3. The broker registers a just-in-time runner for that one job and asks a pool machine for a fresh box of the right
   size. GitHub's runner starts inside the box, takes the job, and the box is ended when the job is done.

A box is a Tart VM on an Apple-silicon Mac, or a container under Kata (a lightweight VM) or gVisor on Linux. Nothing
from one job is left for the next. On a Mac lent by a person, a job ends when its owner touches the keyboard or
trackpad, and nothing new starts until the Mac has been idle for five minutes; `potluck host set drain` winds any
machine down.

## Which jobs it takes, and which it refuses

- Only repositories you connect: `potluck runners allow <owner/repo>`. A public repository needs `--public`, your
  yes to serve its own branches.
- Only runs started by people who can push: `push`, `pull_request` from the repository's own branches,
  `workflow_dispatch`, `schedule`, `merge_group`, `release` and `repository_dispatch`. Never `pull_request_target`,
  `workflow_run`, `issue_comment` and the like, which act on outsiders' input.
- Never a run from a fork, unless you turn on `allow_fork_prs` for a private repository.

A refused job never runs on your machines. It stays queued on GitHub, or, if GitHub hands it a runner made for
another job, the box stops it before its first step and it fails. A fork can edit `runs-on` in its own pull request, so the
label alone would not keep it off your machines; the broker's refusal is what does. To have forks' pull requests
run on GitHub's runner instead, use the `runs-on` that `potluck ci pr` writes (below).

## Move one job onto your Mac: `potluck ci pr`

`potluck ci pr <owner/repo>` changes one job's `runs-on` in a workflow and prepares the change as a pull request
for you to open. A job that says `runs-on: macos-15` becomes:

```yaml
runs-on: ${{ fromJSON(github.event.pull_request && github.event.pull_request.head.repo.full_name != github.repository && '["macos-15"]' || '["self-hosted","macos","<pool label>"]') }}
```

As written, pull requests from forks ask for `macos-15`, GitHub's runner. Pushes, manual runs, schedules and pull
requests from the repository's own branches ask for your pool. Only that one value in the file changes
([docs/ci-pr.md](https://github.com/sengirunners/potluck/blob/main/docs/ci-pr.md)).

## Set it up

Start with a pool and a machine that shares itself with it (the [quick start](https://github.com/sengirunners/potluck#quick-start)).
Then, on the pool's machine:

```sh
potluck runners setup <pool>        # writes the broker's settings and prints the GitHub App's creation page
potluck runners allow <owner/repo>  # connect a repository (add --public for a public one)
potluck ci pr <owner/repo>          # prepare the pull request that moves one job onto the pool
potluck ci doctor <owner/repo>      # 17 read-only checks, each failure with one next step
```

Creating the GitHub App and linking its installation need a public https address that GitHub can send your browser
back to (for example a tunnel). Serving jobs afterwards does not need it: with no public address the broker asks GitHub itself, every 15 seconds. The details are in
[docs/daemon.md](https://github.com/sengirunners/potluck/blob/main/docs/daemon.md#github-runner-broker) and
[docs/ci-doctor.md](https://github.com/sengirunners/potluck/blob/main/docs/ci-doctor.md).

## What it costs

Potluck is free. GitHub does not charge for minutes on self-hosted runners; its billing docs list self-hosted runners
as free (read November 17, 2026). On a private repository, after your plan's included minutes, GitHub's standard
macOS runner costs $0.062 a minute and its 2-core Linux runner $0.006 a minute (GitHub's runner price list, read
November 17, 2026). On your own machines you pay for the electricity.

The machine that runs a job's VM can read everything in it, secrets included, so lend and use machines only among
people you trust with that.

On a Mac, Potluck uses Tart: [what Tart is and what Potluck adds](/tart).
