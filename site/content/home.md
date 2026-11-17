# Potluck: run coding agents and builds on the computers you already own

Builds and agents make your laptop crawl. Potluck pools your team's spare machines and runs coding agents and builds on them, each in a fresh VM or sandbox. When someone needs their machine back, it's theirs.

Install on macOS or Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
```

Potluck is free and open source (Apache-2.0). Releases are signed: the installer checks the signature when it can (it needs OpenSSL 3) and says so when it can't. To lend a machine you need an Apple-silicon Mac with Tart and Softnet, or a Linux box with Docker and Kata or gVisor; `potluck doctor` says what is missing. Code: https://github.com/sengirunners/potluck

## How it works

One machine runs the pool. Everyone else brings theirs. Work goes to whichever machine is free, and each job gets a fresh VM or sandbox that is thrown away after.

1. **Lay out the blanket.** On one machine, start a pool for your team (`potluck create friends`) and make invite codes (`potluck invite sam --host`). Potluck has no accounts of its own. Your machines find each other over Tailscale, or over a tunnel you set up with `--via tunnel`.
2. **Friends bring their machines.** Bring every computer you can spare: the old gaming PC, the Mac mini, the rented server, a cloud VM with hours left over. Put each one in one pool or several. A machine joins only if its checks pass (`potluck doctor`, then `potluck host <code>`).
3. **Leo wants his Mac back.** A Mac works for the team only while its owner is away: when the owner touches the keyboard or trackpad, the job on the Mac ends, and nothing new starts until it has been idle for five minutes (`potluck host --reclaim-idle-s` changes that; 0 shares all the time). To get a machine back for good, `potluck host set drain` winds it down and `potluck host set closed` stops it now.

## What you can do with it

- **Coding agents: give your agent a computer of its own.** An agent's commands run in a throwaway sandbox on a pool machine, not on your laptop: `potluck sandbox create`, `potluck sandbox exec <id> -- make test`, `potluck sandbox rm <id>`. Or hand the agent the sandbox tools over MCP with `{"mcpServers":{"potluck":{"command":"potluck","args":["mcp"]}}}`. To run Claude Code's self-hosted runner in fresh sandboxes: `potluck agent-runner claude --secret-file <key-file>`.
- **GitHub Actions: move a macOS job onto your own Mac.** `potluck ci pr acme/app` prepares a change to one line of your workflow (`runs-on: macos-15` becomes your Mac, with GitHub's runner kept for forks); then you open the pull request it prepares. Pull requests from forks keep using GitHub's runner.

At GitHub's listed rate for its standard macOS runner ($0.062 a minute as of November 2026), 5,000 macOS build minutes a month on a private repository, after your plan's included minutes, cost $310. On your own Mac with Potluck they cost nothing but electricity. Public repositories run on GitHub's runners for free.

## House rules

- **Work stays in a box.** Every job runs in a throwaway VM or sandbox, not directly on your machine, and is deleted after.
- **Keys go only where you send them.** A job gets a secret only when you grant it, only on a friend's machine, and as a file: never on the command line or in its environment.
- **Friends and strangers.** Mark each machine as a friend's or a stranger's. A friend's machine takes your jobs and the keys you grant. A stranger's runs only the sandboxes you send it, and never gets a key.
- **Share the Mac you already own.** Lend your Mac's spare time to your team and friends. Apple's licence forbids selling Mac time, not lending a macOS VM to your team (that is the makers' reading of the licence; read it yourself before you lend). Linux VMs on a Mac aren't affected.

## Who makes it

Jackson and Clay at Sengi make Potluck. Write to ops@sengi.run, or use the form on the page. Potluck Dish, for a team running a brigade of coding agents, is coming soon and is a separate product that runs on Potluck. Sengi also runs hosted GitHub Actions runners for $1 per 1,000 minutes, with 3,000 free minutes a month: https://sengi.run
