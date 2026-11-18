# Put your team's idle machines to work.

Self-hosted GitHub Actions runners and sandboxes for coding agents, each job in a fresh VM or sandbox, on the Macs and Linux boxes you already own. When someone needs their machine back, it's theirs. Free and open source (Apache-2.0).

Install on macOS or Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
```

Free and open source (Apache-2.0) · macOS and Linux. Code: https://github.com/sengirunners/potluck

## Or have your coding agent set it up

Copy this prompt into Claude Code, Codex or any coding agent, and it sets Potluck up on that machine.

```text
Set up Potluck (https://github.com/sengirunners/potluck) on this machine so I can try it alone. Read https://potluck.sh/llms-full.txt first. Then:
1. Install: curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh (if it says ~/.local/bin is not on PATH, run the line it prints).
2. Run `potluck doctor` and tell me what is missing. On a Mac that is usually Tart, Softnet and about 60 GB of free disk. Show me any sudo line it prints; do not run sudo yourself.
3. When doctor is clean: potluck create trial --via tunnel, then potluck host --reclaim-idle-s 0 (on a Mac the first run pulls about 27 GB).
4. potluck sandbox create, then potluck sandbox exec <id> -- uname -a, then potluck sandbox rm <id>.
Stop and ask me before anything that needs a password, a sign-in or deleting files. If a command fails, show me its exact output.
```

## How it works

One machine runs the pool. Everyone else brings theirs. Work goes to whichever machine is free, and each job gets a fresh VM or sandbox that is thrown away after.

1. **Lay out the blanket.** On one machine, start a pool for your team (`potluck create friends`) and make invite codes (`potluck invite sam --host`). Potluck has no accounts of its own. Your machines find each other over Tailscale, or over a tunnel you set up with `--via tunnel`.
2. **Friends bring their machines.** Bring every computer you can spare: the old gaming PC, the Mac mini, the rented server, a cloud VM with hours left over. Put each one in one pool or several. A machine joins only if its checks pass (`potluck doctor`, then `potluck host <code>`).
3. **Leo wants his Mac back.** A Mac works for the team only while its owner is away: when the owner touches the keyboard or trackpad, the job on the Mac ends, and nothing new starts until it has been idle for five minutes (`potluck host --reclaim-idle-s` changes that; 0 shares all the time). To get a machine back for good, `potluck host set drain` winds it down and `potluck host set closed` stops it now.

## What you can do with it

- **Coding agents: give your agent a computer of its own.** An agent's commands run in a throwaway sandbox on a pool machine, not on your laptop: `potluck sandbox create`, `potluck sandbox exec <id> -- make test`, `potluck sandbox rm <id>`. Or hand the agent the sandbox tools over MCP with `{"mcpServers":{"potluck":{"command":"potluck","args":["mcp"]}}}`. To run Claude Code's self-hosted runner in fresh sandboxes: `potluck agent-runner claude --secret-file <key-file> --image ghcr.io/sengirunners/potluck-box-agent`.
- **GitHub Actions: move a macOS job onto your own Mac.** `potluck ci pr acme/app` prepares a change to one line of your workflow (`runs-on: macos-15` becomes your Mac, with GitHub's runner kept for forks); then you open the pull request it prepares. Pull requests from forks keep using GitHub's runner.

At GitHub's listed rate for its standard macOS runner ($0.062 a minute as of November 2026), 5,000 macOS GitHub Actions minutes a month on a private repository, after your plan's included minutes, cost $310. On your own Mac with Potluck they cost nothing but electricity. Public repositories run on GitHub's runners for free.

## House rules

- **Work stays in a box.** Every job runs in a throwaway VM or sandbox, not directly on your machine, and is deleted after.
- **Keys go only where you send them.** A job gets a secret only when you grant it, only on a friend's machine, and as a file: never on the command line or in its environment.
- **Friends and strangers.** Mark each machine as a friend's or a stranger's. A friend's machine takes your jobs and the keys you grant. A stranger's runs only the sandboxes you send it, and never gets a key.
- **Share the Mac you already own.** Stop paying by the minute for macOS GitHub Actions jobs. Lend your Mac's spare time to your team and friends. Apple's licence forbids selling Mac time, not lending a macOS VM to your team (that is the makers' reading of the licence; read it yourself before you lend). Linux VMs on a Mac aren't affected.

## Guides

- A self-hosted GitHub Actions runner with a fresh VM for every job: https://potluck.sh/github-actions-runner
- Tart, its licence, and what Potluck adds: https://potluck.sh/tart
- A sandbox for AI coding agents on machines you own: https://potluck.sh/agent-sandbox

## Who makes it

Jackson and Clay at Sengi make Potluck. Write to ops@sengi.run, or use the form on the page. Mezze, the agent and team layer that runs on Potluck, has its own site: https://mezze.sh. It is for a team running a brigade of coding agents, and is a separate product. Sengi also runs hosted GitHub Actions runners for $1 per 1,000 minutes, with 3,000 free minutes a month: https://sengi.run
