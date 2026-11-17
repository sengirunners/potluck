# How to run Claude Code and Codex in a sandbox on your own machines instead of a hosted sandbox service

Published 2026-11-16. https://potluck.sh/blog/run-coding-agents-in-a-sandbox-on-your-own-machines

You can run a coding agent such as Claude Code or Codex in a sandbox on machines you own, with no hosted sandbox service. Potluck starts a fresh VM or container on a Mac or Linux box in your pool (the ready-made agent image needs a Linux box), runs the agent's commands inside it, and deletes it afterward. The sandbox cannot reach the machine that hosts it, that machine's network, or other sandboxes.

## What does a sandbox for a coding agent have to isolate?

An agent runs shell commands, installs packages, edits files and fetches web pages. Some of what it does is wrong, and some of it may come from text it read, such as a web page or an issue, that tells it to do something harmful. A sandbox limits the damage. It should do six things:

- **Keep the agent off the host.** It cannot reach the machine running it, on any port, including the ports your own programs listen on (on Tart, apart from Softnet's DNS forwarder on the host).
- **Keep it off the local network.** No router, NAS, printer or other computers.
- **Keep sandboxes apart.** One agent cannot reach another agent's sandbox.
- **Hold none of your credentials.** The sandbox starts with no keys of yours, and a key goes in only when you send it.
- **Start clean and end for good.** Each sandbox starts from the same image and is deleted at its time limit or when you remove it.
- **Block mail.** Outbound mail ports are closed.

These are the claims in Potluck's isolation document. `potluck isolation test <machine>-c2m4` checks the host, local network, Tailscale network and peer-sandbox ones by sample on your own machine; `potluck host` checks mail. The repository lists the first recorded test on a real Mac as owed.

## Why run it on machines you own?

A hosted sandbox service runs your code on someone else's machines, which can be the right choice. A sandbox on your own machines is a different tradeoff, set against running the agent directly on your laptop:

| | Agent on your laptop | Potluck sandbox on your machines |
| --- | --- | --- |
| Where commands run | Your laptop, as you | A VM or container on a machine you own |
| Can the agent reach your files | Yes | No |
| Who can read the code, output and secrets | You | You and the owner of the machine running the sandbox |
| Who runs the hardware | You | You and your team |

The machine that runs a sandbox can read everything in it, including your code, your commands, their output and any secrets. The machine that runs the pool can read the commands, output and secrets passing through it, until commands are sealed to the host's key, and over `--via tunnel` so can the tunnel's provider. Potluck's README says to join only a pool whose owner you would trust with that. On your own machines, that owner is you.

## How does Potluck give an agent a fresh box?

A pool is a group of machines you and your friends lend and use. A host is a machine that shares some of itself with a pool, half of its cores and memory by default. A sandbox is a fresh VM or container on a host. Potluck drives the layer underneath: Tart VMs on an Apple-silicon Mac, and Docker with Kata or gVisor on Linux. It does not run its own sandbox technology.

When you ask for a sandbox, Potluck picks the smallest shared machine size that fits (sizes look like `c2m4`: 2 cores, 4 GB of RAM), starts a box there, and prints its id.

## How do you set it up, step by step?

### Step 1: Install and check the machine

```sh
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
potluck doctor
```

On a Mac you need Tart and Softnet. On Linux you need Docker with Kata or gVisor. `potluck doctor` says what is missing. The `agent` template in Step 3 and `potluck agent-runner` need a Linux box with Docker and Kata or gVisor in the pool; a Mac-only pool can run plain sandboxes but not those.

### Step 2: Start a pool and share the machine

```sh
potluck create team --via tunnel
potluck host
```

On a Mac you sit at, add `--reclaim-idle-s 0`. By default a Mac's sandbox ends when you touch the keyboard or pointer, which would cut an agent off mid-task.

### Step 3: Create a sandbox with the agent image

```sh
potluck sandbox create --template agent --ref HEAD
```

The `agent` template is Potluck's Linux image plus git, gh, Node 22, Python 3, ripgrep, jq, build-essential, and the Claude Code and Codex command-line tools. It needs a Linux box in the pool, since the template is built from Potluck's Linux image. `--ref HEAD` sends the current commit of your checkout in as a git bundle. Add `--dirty` to include uncommitted changes.

### Step 4: Run commands in it

```sh
potluck sandbox exec <id> --timeout 600 -- npm test
potluck sandbox shell <id>
```

`exec` takes a literal argument list and exits with the command's exit code. It kills the command after 30 seconds unless you pass `--timeout` (at most 600); `--detach` with `potluck sandbox logs -f` suits longer commands. `shell` opens a terminal in the sandbox, so you can start an agent there. `potluck sandbox cp` copies files in and out.

### Step 5: Remove it

```sh
potluck sandbox rm <id>
```

For a single command, `potluck run -- make test` does all five steps: it creates a sandbox, sends the commit, runs the command, streams the output and removes the sandbox.

## How do you let an agent create its own sandboxes?

Potluck can serve sandbox tools to an agent over MCP. Add it to an MCP client's configuration:

```json
{"mcpServers":{"potluck":{"command":"potluck","args":["--pool","team","mcp"]}}}
```

The server exposes only sandbox tools: create, execute, list, extend, stop, restart, delete, background processes, and file reads and writes. With one pool, `--pool` is optional. Output from a sandbox is treated as data, never as instructions.

## How do you run Claude Code's self-hosted runner in sandboxes?

Claude Code has a self-hosted runner mode, `claude self-hosted-runner`, described in Anthropic's Claude Code documentation under "Self-hosted environments". Potluck's agent-runner document describes the key it needs as an environment secret, which you save to a file. This path needs a Linux box with Docker and Kata or gVisor in the pool: Potluck's agent image is pinned only for Linux, and macOS guests take no secrets yet, so a Mac-only pool cannot run it. Then:

```sh
potluck agent-runner claude --secret-file ~/.config/claude/environment-secret --image ghcr.io/sengirunners/potluck-box-agent
```

Potluck starts one fresh sandbox per runner and keeps a fresh one going as each ends. `--count` runs several at once, `--timeout` sets a time limit (default one hour), `--allow-tool` pre-approves a tool for sessions, and `--deny-tool` blocks one. The default image, the first one a machine lends, does not ship `claude`, so pass `--image` with the `agent` image, which ships `claude`, git and Python 3. Check that its `claude` has the runner mode before relying on it: `potluck sandbox create --template agent`, then `potluck sandbox exec <id> -- claude self-hosted-runner --help`. Potluck installs nothing itself.

## What are the limits you should know about?

Potluck's documents are plain about them. Isolation on a real Mac is also not yet recorded, and on Linux a sandbox that has a network relies on a firewall its owner sets up.

- The vendor key is readable by the machine that runs the sandbox, and by the agent's session in it. A prompt that steers the agent can read it too.
- Potluck does not yet limit a sandbox's network to the vendor's hosts. By default a sandbox on a trusted machine reaches the internet and never the machine or its LAN (on Linux, once the root step `potluck doctor` prints has set up Potluck's filtered network). `--net off` gives no network at all.
- The Claude Code runner is tested with stand-in adapters; the repository records no run of a real Claude Code environment through it yet.
- Agent runners do not yet run on macOS boxes, because macOS guests take no secrets yet.
- The quick start has run in CI with a stand-in VM layer and by hand on a Linux VM with gVisor. It has not yet been run end to end on a real Mac with Tart. Interfaces will change.

## FAQ

### Can I run Claude Code in a Potluck sandbox?

Yes. Create a sandbox with `--template agent`, open it with `potluck sandbox shell`, and start Claude Code there. For sessions started from claude.ai on your own machines, use `potluck agent-runner claude`.

### Does it work with Codex and other agents?

The `agent` image includes the Codex command-line tool, so you can start it in a sandbox the same way. The built-in runner adapter exists only for Claude Code today. Adapters for other vendors are listed as not built.

### Do I need a second machine?

No. The pool and the host can be the same computer, with no account. A second machine is the point once you invite friends.

### What happens if the sandbox runs too long?

It ends at its time limit. Use `--ttl` when you create it, and `potluck sandbox extend <id>` to add time within the machine's cap.

### How is this different from a container on my laptop?

A container on your laptop shares your laptop's network and often your files. A Potluck sandbox runs on a host, in a VM or an isolated container, and is walled off from the host, its network and other sandboxes.

### Does it cost anything?

Potluck is open source under Apache-2.0 and has no price. You pay for your own machines and power.

Code and docs: [github.com/sengirunners/potluck](https://github.com/sengirunners/potluck)
