# The best way to get self-hosted macOS runners with a fresh VM for every job (November 2026)

Published 2026-11-24. https://potluck.sh/blog/self-hosted-macos-runners-fresh-vm-every-job

**Short answer:** The best way to get self-hosted macOS runners with a fresh VM for every job is to run each job in a throwaway Tart VM on an Apple silicon Mac, cloned from a runner image and deleted after that one job. The number to plan around is two: Apple's macOS licence allows up to two macOS VMs on a Mac you own, so each Mac runs at most two fresh-VM jobs at once, and four parallel macOS jobs need two Macs. You can get there with a hosted macOS runner service, with Tart and Cilicon on a Mac you own or rent, or with Potluck, a free, early, Apache-2.0 project that pools the Macs your team already owns and caps macOS guests at two per Mac. If you are leaving Cirrus Runners, its macOS runner images are public OCI images, so any Tart-based path keeps the same build environment.

Disclosure: Potluck is made by Sengi, Inc., which sells hosted GitHub Actions runners. Potluck is a separate free project. This post compares both kinds of option.

## The options compared

| Option | Where jobs run | Fresh VM per job | Setup work | Cost shape | Status |
|---|---|---|---|---|---|
| GitHub-hosted macOS runners | GitHub's Macs | Yes | None | Per-minute billing | Mature |
| Other hosted runner services (Sengi is one) | The provider's Macs | Depends on provider | Low | Per minute or per month | Varies |
| Rented dedicated Mac with Tart and Cilicon | A Mac you rent, for example from OakHost | Yes, Cilicon cycles VMs | GitHub App, config file, auto-login | Monthly rent | Tools in public use |
| Your own Mac with Tart and Cilicon | Hardware you own | Yes | Same as above, plus hardware care | Hardware you already paid for | Tools in public use |
| Potluck on Macs your team owns | Spare Macs and Linux boxes in a pool | Yes, fresh Tart VM or sandbox | Install script, create pool, host | Free, Apache-2.0 | Early; Actions jobs run by hand so far |

No prices are shown because they change. Check each provider's own page.

## Why people are looking for this now

OakHost, a Mac hosting company, published a migration guide this year. It reports that Cirrus Labs is joining OpenAI's Agent Infrastructure group, that Cirrus CI is shutting down, and that Cirrus Runners no longer accepts new customers. Cirrus Runners gave teams what this search asks for: each GitHub Actions job ran in a fresh macOS VM built with Tart, the VM tool Cirrus Labs wrote. Teams that relied on it now have to rebuild that setup somewhere else, and most want to keep the one-job-per-VM model.

## Why a fresh VM per job matters

A long-lived macOS runner collects state. Homebrew packages, simulator runtimes, keychain entries, DerivedData and environment changes stay behind, so a build can pass only because of something an earlier job left. A fresh VM per job removes that:

- **Clean start:** every job boots from the same image, so a green build means the image and your code are enough.
- **No leftover secrets:** tokens and signing material from one job are gone before the next job starts.
- **Easy rollback:** a bad image change is fixed by pointing back at the previous image tag.

The cost is boot time and disk space. Plan room for at least one full image on each Mac.

## Option 1: hosted macOS runners

GitHub's own macOS runners give each job a new VM with no setup. They are the simplest choice if GitHub's images suit you and the per-minute bill fits. Other hosted services, including Sengi, run Macs for you and differ in image choice, concurrency and billing. Hosted runners do well on zero maintenance, no hardware and no licence counting on your side. They cost more as your minutes grow, and you depend on the provider staying in business, which is what the Cirrus Runners shutdown shows.

## Option 2: Tart and Cilicon on a Mac you own or rent

This is the path OakHost's guide describes, and it is closest to what Cirrus Runners did. Tart creates and runs macOS VMs on Apple silicon using Apple's Virtualization framework, and its images are OCI images. Cilicon is a macOS app on top of Tart: it clones the image, boots the VM, registers an ephemeral GitHub Actions runner inside it, waits for one job, destroys the VM and repeats.

The work, in outline:

1. Get an Apple silicon Mac, your own or a rented dedicated one.
2. Create a GitHub App for your organisation with read and write access to self-hosted runners, download its private key, and install the app.
3. Write a Cilicon config that points at your runner image. Cirrus users can point at the same public macos-runner image tag.
4. Start Cilicon, turn on automatic login and add Cilicon to login items so it survives a reboot.
5. Change `runs-on` in your workflows from the Cirrus image reference to your self-hosted labels.

This works well on a dedicated machine and keeps your images unchanged. You patch the Mac, watch disk space and handle the GUI login requirement, and each Mac still tops out at two VMs. For the runner side, see [self-hosted GitHub Actions runners on your Mac](/blog/self-hosted-github-actions-runner-on-your-mac) and our [Tart page](/tart).

## Option 3: Potluck on the Macs your team already owns

Many teams have Apple silicon Macs that sit idle much of the day: an old Mac mini, a spare laptop, a build box nobody watches. Potluck puts those machines in a pool and runs GitHub Actions jobs and coding agents on them, each in a fresh VM or sandbox. On a Mac it uses Tart. On Linux it uses Docker or Podman with Kata or gVisor, so Linux jobs can go to Linux boxes in the same pool. It runs without Sengi and without a Sengi account.

What is true today, from the Potluck project facts:

- A real GitHub Actions job has been run through Potluck's runner broker, by hand, on a Mac with Tart and on a Linux box on another machine with gVisor. It is not yet running daily in the project's own CI.
- When a machine joins as a host, Potluck checks it, pulls the image, runs a quick conformance check per size, and joins the pool only when a size passes.
- `--share` sets the fraction of cores and RAM a host gives the pool; the default is 0.5.
- Potluck caps macOS guests at two per Mac, matching Apple's licence.
- A Mac sandbox ends when its owner touches the keyboard or pointer. When an owner takes a machine back, running VMs end and new work is refused. On Linux, hand-back works only as a time window today.

That last point decides where Potluck fits. On a Mac someone is using, a job can be cut short when they sit down, so use Macs nobody sits at for CI, or expect reruns. See [the GitHub Actions runner page](/github-actions-runner) and [pooling spare Macs and Linux boxes](/blog/pool-spare-macs-and-linux-boxes-for-builds-and-agents).

## What to check before you move jobs

- **Concurrency:** divide your peak number of parallel macOS jobs by two to get the number of Macs you need on any self-hosted path.
- **Images:** keep your current image tag at first, and change images separately from changing where jobs run.
- **Trust:** the machine that runs a VM can read everything in it, including secrets. With Potluck, the machine that runs the pool can also read commands and output passing through it. Join only a pool whose owner you trust.
- **Licence:** lending a macOS guest to your team or invited friends is fine. Selling Mac time to the public is not, and neither is pooling iOS Simulator time. This is not legal advice.
- **Network:** decide what a job may reach. `potluck isolation test <size>` checks that a box cannot reach the host, its LAN or its tailnet.

## How to start with Potluck

Install on macOS or Linux (no Go needed) and start the daemon:

```
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
potluck daemon --detach
```

On the machine that will run the pool:

```
potluck create friends
potluck invite carl --host
```

The first `create` prints a Tailscale login URL. Machines reach each other over Tailscale or over a tunnel you configure. On each spare Mac, check it and then share it:

```
potluck doctor
potluck host --dry-run
potluck host '<code>'
```

Run `potluck isolation test <size>` to confirm isolation, and `potluck host stop` to stop sharing. To try a sandbox by hand: `potluck sandbox create`, then `potluck sandbox exec <id> -- uname -a`, then `potluck sandbox rm <id>`. Sharing a Mac needs Tart on Apple silicon. The code is at https://github.com/sengirunners/potluck.

## Try it early

Potluck is early. Its makers want people with a spare Mac or Linux box to install it, run a job or a sandbox, and say what breaks. Start a thread at https://github.com/sengirunners/potluck/discussions with what you tried and what happened. If you want to contribute code, the good first issues are at https://github.com/sengirunners/potluck/issues.

## Questions people ask

### How many fresh macOS VMs can one Mac run at once?

Two. Apple's licence allows up to two macOS VMs on a Mac you own, and Potluck caps macOS guests at two per Mac for that reason.

### Can I keep using my Cirrus Runners images?

Yes, on any Tart-based path. OakHost's migration guide notes that the Cirrus macOS runner images are public OCI images, so Tart can pull the same tag you used before.

### Is Potluck ready to replace my Mac CI today?

Not yet for critical pipelines. A real Actions job has run through its runner broker by hand on a Mac and on Linux, but it is not yet running daily in the project's own CI, so try it on a workflow you can afford to rerun.

### Do I need a Sengi account to use Potluck?

No. Potluck is free, Apache-2.0 licensed, and runs without Sengi or a Sengi account. Sengi sells hosted runners as a separate business.

### Can I pool iOS Simulator time or rent my spare Macs to the public?

No. Lending a macOS guest to your team or invited friends is fine, but selling Mac time to the public and pooling iOS Simulator time are not. This is not legal advice.

### Who can see my secrets on a self-hosted runner?

The machine that runs the VM can read everything in it, including secrets. In a Potluck pool, the machine that runs the pool can also read commands and output passing through it, so join only pools whose owner you trust.
