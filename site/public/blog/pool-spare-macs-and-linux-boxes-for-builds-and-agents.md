# How to pool your team's spare Macs and Linux boxes for builds and coding agents

Published 2026-11-16, updated 2026-11-19. https://potluck.sh/blog/pool-spare-macs-and-linux-boxes-for-builds-and-agents

A pool is a group of machines that a team or friends lend to each other for builds and coding agents. With Potluck, one machine runs the pool, other people join with one-time invite codes, and each job runs in a fresh VM or sandbox on whichever machine is free. A machine's owner chooses how much to share and can wind it down or stop it with one command.

## What is a pool, and what do the words mean?

Potluck uses a few words, and these four matter most here:

- **Pool.** The group of machines you and your friends lend and use. One machine runs it, with `potluck create`.
- **Host.** A machine that shares some of itself with a pool, with `potluck host`. It shares half of its cores and memory by default, which you change with `--share`.
- **Member.** A person who runs sandboxes on the pool's machines.
- **Sandbox.** A fresh VM or container on a host, which you create, run commands in and remove.

Potluck has no accounts of its own. Machines reach each other over Tailscale by default, or over a tunnel you set up with `--via tunnel`.

## Who can use whose machine?

The pool's owner decides, because every person and machine joins with a one-time invite code that the owner makes. There are two kinds of invite, and they are different roles:

| Invite | Command | What the person can do |
| --- | --- | --- |
| Member | `potluck invite sam` | Runs sandboxes on the pool's machines. |
| Host, trusted | `potluck invite carl --host` | Shares a machine. Sees nothing else of the pool. |
| Host, public | `potluck invite dee --host --trust public` | Shares a machine as a stranger's. Sees nothing else of the pool. |

A host's trust is the owner's choice at invite time, and the machine cannot change it. The difference matters:

- A **trusted** machine, a friend's, has sandboxes placed on it without being asked. It may run [GitHub Actions jobs](/github-actions-runner) and receive the secrets a member sends to a sandbox.
- A **public** machine, a stranger's, runs only sandboxes a member names with `--on`. It never runs GitHub Actions jobs and never receives a secret.

Two limits are worth knowing before you invite anyone. The machine that runs a sandbox can read everything in it, including code, commands, output and secrets. The machine that runs the pool can read the commands, output and secrets passing through it, until commands are sealed to the host's key, and over `--via tunnel` so can the tunnel's provider. Join only a pool whose owner you would trust with that.

## What does each job run in, and what can it reach?

Every job runs in a throwaway VM or isolated container, not on the host itself. Potluck's isolation document says the sandbox cannot reach the lending machine on any port (on Tart, apart from Softnet's DNS forwarder on the host), cannot reach its local network or its Tailscale network, cannot reach another sandbox running at the same time, cannot send mail, and holds none of the lender's credentials.

A lender can check this on their own machine:

```sh
potluck isolation test <machine>-c2m4
```

Sizes are matched by exact name, and `potluck host` names each one `<machine>-<size>` (`potluck isolation` lists the names this machine serves). The test boots sandboxes of that size and tries to reach this machine's addresses, the router, Tailscale's resolver and a second sandbox. It passes only if every check ran and failed to get through. On Linux, a sandbox that has a network relies on a firewall the machine's owner sets up.

## How do you start a pool, step by step?

### Step 1: Install and check the first machine

```sh
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
potluck doctor
```

A Mac needs to be Apple silicon with [Tart](/tart) and Softnet. A Linux box needs Docker with Kata or gVisor. `potluck doctor` says what is missing.

### Step 2: Create the pool and share this machine

```sh
potluck create friends
potluck host
```

The first `potluck create` with the Tailscale default prints a login link and waits for you to sign in. On a Mac, the first `potluck host` pulls a macOS image of about 27 GB and wants about 60 GB free.

### Step 3: Invite people

```sh
potluck invite ana
potluck invite carl --host
```

Each prints a one-time code that works for 24 hours by default (`--ttl` changes it).

### Step 4: Friends join and share

Ana runs `potluck join '<code>'` and can then create sandboxes. Carl runs `potluck host '<code>'` on the machine he lends. That command checks the machine, pulls the image, runs a quick conformance check per size, and joins only if a size passes. A machine that fails never joins, and the code stays unspent.

### Step 5: Run work

```sh
potluck sandbox create --ref HEAD
potluck sandbox exec <id> --timeout 600 -- make test
potluck sandbox rm <id>
potluck run -- make test
```

`potluck sandbox create` picks the smallest shared size that fits. `--ref HEAD` sends your checkout's current commit in; without it the sandbox has no code and `make test` would run in an empty home directory. `exec` kills a command after 30 seconds unless you pass `--timeout` (at most 600), so for longer work use `potluck run`, whose limit is 30 minutes by default. `potluck run` creates a sandbox, sends your checkout's commit, runs the command and removes the sandbox. GitHub Actions jobs and coding agents use the same pool: see `potluck runners` and `potluck agent-runner`. To keep one group from using up everyone's time, `potluck usage limit <area> <minutes>` sets a minutes limit for an area, such as `potluck usage limit team 600 --per week` (day, week or month; a project owner or admin sets it).

## How does a Mac owner take the machine back?

A Mac owner has three levels of control, and the second and third are single commands.

1. **Automatic.** By default a Mac's sandbox ends when the owner touches the keyboard or pointer, and none starts until the Mac has been idle for five minutes. `--reclaim-idle-s` changes the idle time, and `0` shares all the time.
2. **Wind down.** `potluck host set drain` stops new work. Running work finishes and is cut after 30 minutes by default. `--deadline 10m` changes the limit.
3. **Stop now.** `potluck host set closed` refuses new work and ends running work immediately.

```sh
potluck host set drain
potluck host set closed
potluck host set auto
```

`auto` returns to the machine's own rules. `--for 2h` and `--until 23:00` return the machine to `auto` at a set time. On a Mac there is also a menu bar item with Wind down and Stop now entries. Install it with `curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh -s -- --menubar`, which installs the app but does not start it, then open it from your `~/Applications` folder. It is not yet signed with an Apple Developer ID.

To stop sharing for good, `potluck host stop` removes every size the machine shared, and `potluck leave` leaves the pool.

## Can you lend a Mac?

Apple's macOS software license agreement (read November 2026) allows up to two macOS VMs per Mac you own or control, and Potluck caps macOS guests at two. The README's rule is that lending a macOS VM to your team or invited friends is fine and selling Mac time to the public is not. Read the licence yourself; this is not legal advice.

## FAQ

### Is it a cluster scheduler?

No. Potluck runs builds, GitHub Actions jobs and coding agents in VMs and containers on ordinary Macs and Linux boxes, and its README says it is not a scheduler for clusters.

### Can I join a pool as a friend without giving access to my files?

Yes. A job runs in a throwaway sandbox that is meant to be walled off from your machine, its network and your credentials, and `potluck isolation test <machine>-c2m4` checks that by sample on your machine. You still trust the pool's owner with what the pool's machine can read.

### Can strangers use my machine?

Only people the pool's owner invites as members can run sandboxes on it, and each invite code works once. A stranger's machine can share with the pool only through `--trust public`, which gives it no secrets and no GitHub Actions jobs.

### What if my machine goes to sleep or reboots?

On a Mac, Potluck keeps the Mac from idle sleep while a VM runs, though the display may still sleep. With FileVault on, a Mac that reboots runs nothing until its owner logs in. VMs end at their deadline even if the daemon is down.

### Is it a volunteer-computing network?

No. Potluck runs a team's own builds, GitHub Actions jobs and coding agents on machines the team or its invited friends already own. Nothing goes to people the pool's owner has not invited.

### Does it cost anything?

Potluck is open source under Apache-2.0 and has no price. Each owner pays for their own machine and power. Jobs on
a pool's machines use none of GitHub's minutes; [how GitHub bills Actions minutes](https://sengi.run/blog/github-actions-billing-explained)
explains what they would cost on GitHub's runners.

Code and docs: [github.com/sengirunners/potluck](https://github.com/sengirunners/potluck)
