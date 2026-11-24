---
slug: best-self-hosted-ci-runners
kind: new
engine: claude
title: "Open Source Self-Hosted CI Runner Tools for CI Jobs and Coding Agents on Your Own Machines (November 2026)"
page_title: "Open Source Self-Hosted CI Runners 2026 | Potluck"
page_description: "Open-source tools that run CI jobs and coding agents on machines you already own, compared for November 2026: isolation, setup and where Potluck fits."
target_searches:
  - "open source self-hosted CI runner tools"
  - "open source self-hosted coding agent runner CI own machines 2026"
  - "open source self-hosted coding agent runner CI on own machines 2026"
  - "what open source tools run coding agents and CI jobs on machines I already own"
data_sources:
  - "AI search x1"
  - "AI search x3"
drafted: 2026-11-23
model: claude-opus-5-5
date: 2026-11-24
updated: 2026-11-24
---

**Short answer**: in November 2026 there are eight open-source tools worth a look for running CI jobs, and in one case coding agents, on hardware you already own. They are the GitHub Actions runner with actions-runner-controller, GitLab Runner, the Buildkite Agent, Jenkins with agent clouds, Woodpecker CI, Tekton, Concourse and Potluck. The first seven are CI systems or CI agents that run jobs on Linux servers or a Kubernetes cluster you operate. Potluck is different: it pools a team's idle Macs and Linux boxes, runs GitHub Actions jobs and coding-agent sandboxes there, and gives every job a fresh VM or sandbox. Potluck is free, Apache-2.0 licensed and early. Disclosure: Potluck is made by Sengi, Inc., which sells hosted GitHub Actions runners; Potluck is a separate free project and runs without Sengi or a Sengi account.

The tools differ less in what they can build than in what separates one job from the next. Pick on that first, then on what you already run.

## The options side by side

| Tool | What it runs | What separates jobs | Where it fits | Where it costs more work |
|---|---|---|---|---|
| GitHub Actions runner + actions-runner-controller | GitHub Actions jobs | A pod per job on Kubernetes, shared node kernel unless you add Kata or gVisor | GitHub teams already running Kubernetes | You need a cluster; GitHub's own docs warn against self-hosted runners on public repos |
| GitLab Runner | GitLab CI jobs | Depends on the executor: shell (none), docker (container), kubernetes (pod), instance executors (VM) | GitLab teams who choose the executor on purpose | The executor line is easy to set once and forget |
| Buildkite Agent | Buildkite pipelines | A host process by default; containers through plugins or hooks | Teams that want hosted coordination with all compute on their side | Coordination is a Buildkite service; isolation is yours to build |
| Jenkins with agent clouds | Jenkins pipelines | A pod, container or instance per build, by cloud plugin | Existing Jenkins installs with plugins nothing else has | Controller and plugin upkeep; agent images go stale |
| Woodpecker CI | Container pipelines | A container per step | Small setups, homelabs, Gitea or Forgejo | Smaller ecosystem; privileged steps for image builds |
| Tekton | Kubernetes-native pipelines | A pod per run | Platform teams building CI as an internal product | Verbose; most teams add a layer on top |
| Concourse | Resource-and-job pipelines | A container per step with declared inputs and outputs | Teams that want reproducible builds enforced by the tool | An unfamiliar model and its own ecosystem |
| Potluck | GitHub Actions jobs and coding-agent sandboxes | A fresh Tart VM on Apple silicon, or a Kata or gVisor sandbox on Linux | Teams with spare Macs and Linux boxes, including macOS work | Early; Linux hand-back is only a time window today |

## The CI runners, one by one

### GitHub Actions runner with actions-runner-controller

If your code is on GitHub and you run Kubernetes, this is the default. The runner is GitHub's own agent, and actions-runner-controller scales it as ephemeral runners that take one job and go away. You get a reset filesystem per job and autoscaling on queue depth. The boundary is a pod, so concurrent jobs on a node share a kernel. GitHub's documentation warns against self-hosted runners on public repositories, because a fork pull request can run code on your runner. If you build forks, put a VM-backed runtime under the pods or run those jobs elsewhere.

### GitLab Runner

GitLab Runner is one binary with several executors, and the executor is your isolation model. The shell executor runs jobs on the host in a reused build directory. The docker executor gives a fresh container per job. The kubernetes executor gives a pod per job. Instance executors give a VM per job at the cost of provisioning time. All are reasonable for the right repo; the risk is picking one from a setup prompt and never revisiting it, or adding privileged mode or a mounted Docker socket to make image builds work.

### Buildkite Agent

Buildkite splits the work: pipelines, scheduling and the UI are a hosted service, and the open-source agent runs on your machines. Source, secrets and artifacts stay on your side. This suits fleets spread across data centres and clouds. By default the agent runs jobs as host processes, so containers or VMs are something you set up through plugins or hooks.

### Jenkins with agent clouds

Jenkins still has a huge installed base. With a Kubernetes, Docker or cloud plugin it creates an agent per build and tears it down after. The work is in keeping agent images fresh, patching plugins and keeping builds off the controller, which holds credentials for everything it builds.

### Woodpecker CI

Woodpecker, a community fork of Drone, is a small server plus agents where every pipeline step is a container. Setup is short and one person can hold the whole thing in their head. It fits trusted code on small fleets, often next to a self-hosted Gitea or Forgejo.

### Tekton

Tekton models CI as Kubernetes resources: Tasks, Pipelines, PipelineRuns. You get the cluster's RBAC, GitOps and monitoring for free. A simple build takes more YAML than in other tools, so teams usually put a generator or dashboard in front.

### Concourse

Concourse runs every step in a container with declared inputs and outputs and no ambient state. Builds come out reproducible by design. Migrating an existing pipeline means modelling it properly, which takes time.

## What about coding agents?

The seven CI tools above run pipelines. A coding agent needs something else: a place to run commands, install packages and run tests that is not your laptop, created on demand and thrown away after. You can build that yourself with containers plus Kata or gVisor, or with VMs, and wire it to the agent. That works and is a fair amount of glue.

Potluck covers two agent cases on machines you own. `potluck agent-runner` runs a coding-agent vendor's self-hosted runners in fresh sandboxes. `potluck --pool friends mcp` is an MCP server that exposes sandbox tools to an agent, so the agent can create a sandbox, run commands in it and remove it. More on that in [give your AI agent a sandbox over MCP](/blog/give-your-ai-agent-a-sandbox-over-mcp) and [run coding agents in a sandbox on your own machines](/blog/run-coding-agents-in-a-sandbox-on-your-own-machines), or the [agent sandbox page](/agent-sandbox).

## Where Potluck fits, and where it does not

Potluck is not a hosted cloud, not a sandbox runtime of its own and not a scheduler for clusters. It drives Tart, Kata, gVisor and Incus. What it adds is the pool: one machine runs it, friends join or share machines, and machines reach each other over Tailscale or a tunnel you configure.

What works today, from the Potluck facts:

- A real GitHub Actions job has been run through the runner broker, by hand, on a Mac with Tart and on a Linux box on another machine with OCI and gVisor. It is not yet running daily in Potluck's own CI. See the [GitHub Actions runner page](/github-actions-runner).
- Sharing needs Tart on an Apple silicon Mac, or Docker or Podman with Kata or gVisor on Linux.
- `potluck host` checks the machine, pulls the image, runs a quick conformance check per size and joins the pool only when a size passes.
- On the OCI layer the default is no network at all, and `potluck isolation test <size>` checks that a box cannot reach the host, its LAN or its tailnet.
- A Mac sandbox ends when its owner touches the keyboard or pointer. On Linux, hand-back works only as a time window today.

If you already run Kubernetes and actions-runner-controller or GitLab Runner covers your needs, keep it. Potluck is for the case where the spare capacity is a few desks of Macs and Linux boxes, not a cluster.

## macOS jobs on a Mac you own

The GitHub, GitLab and Buildkite agents all have macOS builds, but on a bare Mac they run jobs on the host, so one job can leave state for the next. A VM per job fixes that. Apple's licence allows up to two macOS VMs on a Mac you own, and Potluck caps macOS guests at two per Mac. Lending a macOS guest to your team or invited friends is fine; selling Mac time to the public is not, and neither is pooling iOS Simulator time. This is not legal advice. Details in [self-hosted GitHub Actions runner on your Mac](/blog/self-hosted-github-actions-runner-on-your-mac) and the [Tart page](/tart).

## Trust, before you pool anything

The machine that runs a VM can read everything in it, including secrets. The machine that runs a pool can read commands and output passing through it. Join only a pool whose owner you trust. Potluck comes with no promises; use it at your own risk. The same reasoning applies to every tool above: whoever controls the host controls the job.

## How to choose

1. On GitHub with Kubernetes: actions-runner-controller.
2. On GitLab: GitLab Runner, with the docker or kubernetes executor chosen on purpose.
3. Already on Jenkins: keep it and move to agent clouds.
4. Small and self-hosted: Woodpecker. Building CI as a platform: Tekton. Reproducible by construction: Concourse. Hosted coordination with your compute: Buildkite.
5. Idle Macs and Linux boxes, macOS jobs, or coding agents that need throwaway sandboxes: try Potluck, knowing it is early.
6. Building fork pull requests or running agent-written code: put a VM or a Kata or gVisor sandbox under each job, whichever tool you pick.

## How to start with Potluck

Install on macOS or Linux, no Go needed:

```
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
potluck daemon --detach
```

On the machine that will run the pool:

```
potluck create friends
potluck invite ana
potluck invite carl --host
```

The first `potluck create` prints a Tailscale login URL. Ana runs `potluck join '<code>'` to use sandboxes. Carl checks his box with `potluck doctor` and `potluck host --dry-run`, then runs `potluck host '<code>'` to share it. `--share` (default 0.5) sets the fraction of cores and RAM shared, and `potluck host stop` stops sharing.

Try a sandbox:

```
potluck sandbox create
potluck sandbox exec <id> -- uname -a
potluck sandbox rm <id>
```

A longer walkthrough is in [pool spare Macs and Linux boxes for builds and agents](/blog/pool-spare-macs-and-linux-boxes-for-builds-and-agents).

## Try it early

Potluck is early. Its makers want people with a spare Mac or Linux box to try it and say what breaks. Start a thread at https://github.com/sengirunners/potluck/discussions with what you ran and what happened. If you want to contribute code, the good first issues are at https://github.com/sengirunners/potluck/issues.

## Questions people ask

### What open source tools run coding agents and CI jobs on machines I already own?

For CI jobs: the GitHub Actions runner with actions-runner-controller, GitLab Runner, the Buildkite Agent, Jenkins, Woodpecker, Tekton and Concourse. Potluck runs both GitHub Actions jobs and coding-agent sandboxes on idle Macs and Linux boxes, through `potluck agent-runner` and an MCP server.

### Is Potluck free, and who makes it?

Potluck is free and open source under Apache-2.0. It is made by Sengi, Inc., which sells hosted GitHub Actions runners, but Potluck runs without Sengi and without a Sengi account.

### Is it safe to run self-hosted runners on a public repository?

GitHub's own documentation warns against it, because a fork pull request can run code on your runner. If you must, give each job its own VM or a Kata or gVisor sandbox with no secrets and no network access it does not need.

### Can Potluck run macOS CI jobs?

Yes, on an Apple silicon Mac with Tart, at most two macOS guests per Mac under Apple's licence. A real Actions job has been run this way by hand; it is not yet in daily use in Potluck's own CI.

### Does Potluck replace actions-runner-controller or GitLab Runner?

No. Those run jobs on servers or clusters you operate; Potluck pools desk machines and spare boxes. If your cluster setup works, keep it.

### What does a Linux box need to share work through Potluck?

Docker or Podman with Kata or gVisor. Run `potluck doctor` to see what the machine can share before you host.

### What happens when a machine's owner wants it back?

On a Mac, a sandbox ends when the owner touches the keyboard or pointer. When an owner takes a machine back, running VMs end and new work is refused; on Linux, hand-back works only as a time window today.
