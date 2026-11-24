# Actions Runner Controller (ARC): Self-Hosted GitHub Actions Runners on Kubernetes in 2026

Published 2026-11-24. https://potluck.sh/blog/actions-runner-controller-github-actions-runner-controller-arc

**Short answer**: Actions Runner Controller (ARC) is the Kubernetes operator from the actions/actions-runner-controller repository that runs and autoscales self-hosted GitHub Actions runners as ephemeral pods. The GitHub Docs page "Actions Runner Controller" says ARC ships as 2 Helm charts and 1 controller container image. You run 2 `helm install` commands: one for the gha-runner-scale-set-controller chart, and one gha-runner-scale-set chart per runner scale set. Then you set `runs-on` to the installation name, which the same page limits to 45 characters. Per that page, a failed runner pod is retried up to 5 times, and GitHub unassigns a job that no runner accepts within 24 hours. ARC needs a Kubernetes cluster and runs Linux container pods. If your spare capacity is Macs and Linux boxes rather than a cluster, Potluck, an early open-source project, runs jobs in a fresh VM or sandbox on machines you already own.

The rest of this page is a setup reference. Each fact names the GitHub Docs page or repository it comes from. The last part shows where Potluck fits.

## What Actions Runner Controller (ARC) is

The repository description calls actions-runner-controller a "Kubernetes controller for GitHub Actions self-hosted runners". The GitHub Docs concept page describes it as an operator that orchestrates and scales those runners.

- **Runner scale sets are the supported mode.** The repository README says the older autoscaling modes are legacy and maintained by the community only.
- **Charts are OCI only.** The concept page says the charts are published as OCI packages on GitHub Packages. There are no tarballs and no Helm repository on GitHub Pages.
- **ARC adds custom resources.** The concept page lists the API groups `actions.github.com/v1alpha1` (scale sets) and `actions.summerwind.net/v1alpha1` (legacy). The controller chart installs them.

## ARC compared with other runner options

Potluck is made by Sengi, which sells hosted GitHub Actions runners. Potluck is a separate, free project.

| Option | Where jobs run | What you operate | macOS jobs |
|---|---|---|---|
| ARC runner scale sets | Pods in your Kubernetes cluster | The cluster, 2 Helm releases, GitHub auth, logs | No, Linux container pods |
| One self-hosted runner | A machine you own | The runner app, cleanup between jobs | Yes, on a Mac |
| Hosted runners (GitHub-hosted, Cirrus Runners, Blacksmith, Sengi) | The vendor's machines | Nothing; the vendor bills you | Where the vendor offers it |
| Potluck | A fresh VM or sandbox on Macs and Linux boxes you own | A `potluck` daemon per machine, Tailscale or your own tunnel | Yes, with Tart on Apple silicon |

ARC is GitHub-supported and autoscales with workflow demand. A single runner is the simplest option, but it has no autoscaling and does not give each job a clean environment. Hosted runners need no hardware. Potluck is early: its Actions path has been run by hand, not yet daily.

## Prerequisites for ARC on Kubernetes

The GitHub Docs tutorial "Get started with Actions Runner Controller" lists:

- A Kubernetes cluster. It suggests a managed service such as AKS, or minikube or kind for a local trial.
- Helm 3.
- A way to collect and keep logs from the controller, listeners and ephemeral runners before production. The tutorial recommends this but does not require it.

## Step 1: install the gha-runner-scale-set-controller chart

This step installs the operator and the CRDs. The commands come from the quickstart:

```bash
NAMESPACE="arc-systems"
helm install arc \
  --namespace "${NAMESPACE}" \
  --create-namespace \
  oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set-controller
```

Once you move past a trial, pin a chart version with `--version`. The namespace must be able to reach the Kubernetes API server.

## Step 2: authenticate ARC to the GitHub API

The docs page "Authenticating ARC to the GitHub API" covers authentication.

- For repository or organization runners, GitHub recommends a GitHub App.
- Enterprise-level scale sets require a personal access token (classic).
- The quickstart uses a classic token with the `repo` and `admin:org` scopes to keep the trial short.

The quickstart warns against passing secrets in plain text on the command line. Create a Kubernetes secret instead:

```bash
kubectl create namespace arc-runners
kubectl create secret generic arc-github-secret \
  --namespace arc-runners \
  --from-literal=github_token='<PAT>'
```

## Step 3: deploy a runner scale set

The installation name becomes your `runs-on` value. The quickstart recommends keeping runner pods in a different namespace from the controller.

```bash
INSTALLATION_NAME="arc-runner-set"
NAMESPACE="arc-runners"
GITHUB_CONFIG_URL="<URL of your repository, organization or enterprise>"
helm install "${INSTALLATION_NAME}" \
  --namespace "${NAMESPACE}" \
  --set githubConfigUrl="${GITHUB_CONFIG_URL}" \
  --set githubConfigSecret=arc-github-secret \
  oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set
```

The concept page's resource table says that when `githubConfigSecret` is a string, ARC uses that existing secret instead of creating one.

Resource names are the installation name plus a fixed suffix, so keep the name short. For example, the listener Role reserves 26 characters for its suffix.

To route jobs by other names, set `runnerScaleSetName` or `runnerScaleSetLabels` in `values.yaml`.

## Step 4: verify the install and run a workflow

```bash
helm list -A
kubectl get pods -n arc-systems
```

You should see both releases deployed, plus a controller pod and a listener pod in `Running`. Then add a workflow:

```yaml
name: Actions Runner Controller Demo
on:
  workflow_dispatch:
jobs:
  Explore-GitHub-Actions:
    runs-on: arc-runner-set
    steps:
      - run: echo "This job uses runner scale set runners"
```

Trigger it by hand and watch a runner pod come and go with `kubectl get pods -n arc-runners -w`. If a pod stays pending, see the GitHub Docs page "Troubleshooting Actions Runner Controller errors".

## How ARC scales runners

The architecture section of the concept page describes this flow:

1. The controller fetches the runner group and creates the scale set in the GitHub Actions service.
2. A listener pod holds an HTTPS long poll and waits for a Job Available message.
3. When a job's `runs-on` matches, the listener patches the EphemeralRunnerSet replica count.
4. The EphemeralRunner controller gets a just-in-time token and creates runner pods. It retries a failed pod up to 5 times.
5. When the job finishes, the runner is deleted.

Each job gets a new pod.

## Custom runner images

The concept page lists what GitHub's minimal runner image contains: runner binaries, container hooks, and Docker for Docker-in-Docker mode.

To add tools, use setup actions such as actions/setup-node, or build your own image. A custom image must:

- place the runner under `/home/runner/` and start it with `/home/runner/run.sh`;
- in Kubernetes mode, put the container hooks under `/home/runner/k8s`.

## Where Potluck fits

ARC assumes a Kubernetes cluster and Linux pods. Many teams that ask about running jobs on spare Macs have no cluster. They have a Mac mini on a shelf and a Linux box under a desk.

Potluck is free, Apache-2.0 software that runs GitHub Actions jobs and coding agents on those machines, each in a fresh VM or sandbox.

- **Runtimes:** Tart on Apple silicon, or Docker or Podman with Kata or gVisor on Linux.
- **Not a cluster scheduler:** if you already run Kubernetes, ARC is the better fit for Linux jobs.
- **Status:** a real Actions job has been run through the runner broker by hand, on a Mac (Tart) and on a Linux box on another machine (OCI with gVisor). It does not run daily in our own CI yet.
- **Coding agents:** `potluck agent-runner` runs a coding-agent vendor's self-hosted runners in fresh sandboxes.
- **Owners come first:** a Mac sandbox ends when its owner touches the keyboard or pointer. On Linux, hand-back works only as a time window today.
- **macOS limit:** Apple's licence allows 2 macOS VMs on a Mac you own, and Potluck caps macOS guests at 2 per Mac.

More: [GitHub Actions runners on your own machines](/github-actions-runner), [a self-hosted runner on your Mac](/blog/self-hosted-github-actions-runner-on-your-mac), [pooling spare Macs and Linux boxes](/blog/pool-spare-macs-and-linux-boxes-for-builds-and-agents).

## How to start with Potluck

Install and start the daemon on macOS or Linux (no Go needed):

```bash
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
potluck daemon --detach
```

Set up a pool:

1. On the pool machine, run `potluck create friends`. The first time, it prints a Tailscale login URL.
2. Run `potluck invite ana` to make a code for a friend who runs sandboxes.
3. Run `potluck invite carl --host` to make a code for a friend who shares a machine.
4. Friends run `potluck join '<code>'` or `potluck host '<code>'`.

Before you share a machine:

- `potluck doctor` shows what the machine can share.
- `potluck host --dry-run` prints the plan.
- `--share` sets the fraction of cores and RAM shared (default 0.5).
- `potluck isolation test <size>` checks that a box cannot reach the host, its LAN or its tailnet.

Try a sandbox with `potluck sandbox create`, `potluck sandbox exec <id> -- uname -a` and `potluck sandbox rm <id>`. To stop sharing, run `potluck host stop`.

Trust works like this. The machine that runs a VM can read everything in it, including secrets. The pool machine can read commands and output passing through it. Join only a pool whose owner you trust, and use Potluck at your own risk.

## Try it early

Potluck is early. Its makers want people with a spare Mac or Linux box to try it and say what breaks. Start a thread at https://github.com/sengirunners/potluck/discussions. Contributors can find good first issues at https://github.com/sengirunners/potluck/issues.

## Questions people ask

### What is Actions Runner Controller (ARC)?

ARC is a Kubernetes operator that runs and autoscales self-hosted GitHub Actions runners as ephemeral pods. It lives in the actions/actions-runner-controller repository, and its documentation is on GitHub Docs.

### Is ARC supported by GitHub?

The runner scale set mode is supported by GitHub. The README says the legacy autoscaling modes are maintained by the community only.

### Should ARC use a GitHub App or a personal access token?

GitHub Docs recommends a GitHub App for repository and organization runners. Enterprise-level scale sets require a personal access token (classic).

### Can ARC run macOS jobs?

No. ARC runners are Linux container pods. For macOS jobs you need a Mac, a hosted macOS runner, or a Tart VM through Potluck on Apple silicon.

### Does Potluck replace ARC?

No. Potluck is not a cluster scheduler. It puts idle Macs and Linux boxes to work in fresh VMs or sandboxes.

### Do I need a Sengi account to use Potluck?

No. Potluck runs without Sengi and without a Sengi account.
