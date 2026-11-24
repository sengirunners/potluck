---
slug: claude-code-permissions-sandboxing-explained
kind: new
engine: claude
title: "Claude Code permissions and sandbox settings explained (2026)"
page_title: "Claude Code Permissions & Sandbox (2026) | Potluck"
page_description: "How Claude Code permission rules, permission modes and the Bash sandbox fit together in 2026, what each blocks, and when to isolate the whole agent."
target_searches:
  - "Claude Code permissions and sandbox settings explained"
data_sources:
  - "Brave term"
drafted: 2026-11-23
model: claude-opus-5-5
date: 2026-11-24
updated: 2026-11-24
---

**Short answer**: Claude Code has two separate controls. Permission rules (allow, ask, deny) and permission modes decide whether a tool call runs and whether you are asked first, and they apply to every tool: Read, Edit, Bash, WebFetch, MCP. The Bash sandbox is a boundary the operating system enforces (Seatbelt on macOS, bubblewrap on Linux and WSL2) around shell commands only. It is off by default. When on, commands can write only to the working directory, a per-user temp directory and directories you added; they can read most of the machine, including `~/.ssh`, unless you deny it; and their network traffic goes through a local proxy whose allowed domains list starts empty. Claude's file tools, MCP servers and hooks run outside the sandbox. For one boundary around everything the agent does, run it in a container, a VM, or a sandbox on another machine. This follows Anthropic's Claude Code documentation as of November 2026.

## The layers at a glance

| Layer | What it controls | Enforced by | Covers |
|---|---|---|---|
| Permission rules | Which tool calls run, prompt, or are blocked | Claude Code, before the tool runs | All tools |
| Permission modes | Whether you get a prompt | Claude Code (auto mode adds a classifier) | All tools |
| Bash sandbox | What a running shell command can read, write and reach | The OS kernel | Bash, PowerShell and Monitor commands and their child processes |
| Whole-process boundary | Everything the agent process does | A container, VM or remote sandbox | The agent, its file tools, MCP servers and hooks |

The University of Vermont IT knowledge base names the most common mistake: assuming the sandbox restricts everything Claude reads. It does not. A sandbox `denyRead` entry does not stop the Read tool, and `allowedDomains` does not limit WebFetch. Block a secret in both places, or the other path still reaches it.

## Permission rules: allow, ask, deny

Rules live in `permissions` in a settings file and use patterns such as `Read(...)`, `Edit(...)`, `Bash(git push *)` and `WebFetch(domain:example.com)`. Per the UVM article, they are evaluated deny first, then ask, then allow, and a deny set in managed settings cannot be re-allowed by a user. Watch the path syntax: in Read and Edit rules, `//path` is absolute and `/path` is relative to the project, while sandbox filesystem settings use ordinary paths such as `/tmp/build` and `~/.kube`.

## Permission modes

Modes decide who approves a call. Manual mode prompts; acceptEdits approves file edits; auto mode hands the decision to a classifier model; dontAsk refuses anything not already allowed; plan mode gates commands while you plan; bypassPermissions (the `--dangerously-skip-permissions` flag) skips prompts. `/sandbox` is not a mode. The sandbox's own auto-allow setting approves Bash commands because the sandbox contains them, which is different from auto mode's classifier. You can combine the two.

## What the Bash sandbox restricts by default

| Access | Default when enabled | Change it with |
|---|---|---|
| Writes | Working directory, per-user temp directory, added directories; protected paths such as `.claude` settings, `.mcp.json` and `.bashrc` stay write-denied | `filesystem.allowWrite`, `filesystem.denyWrite` |
| Reads | Most of the machine, including credential files | `filesystem.denyRead`, `filesystem.allowRead`, `credentials` |
| Network | No direct route out; a proxy checks each host against an allowlist that starts empty | `network.allowedDomains`, `network.deniedDomains`, `network.strictAllowlist` |
| Environment | Inherited from Claude Code, secrets included | `credentials.envVars`, `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB` |

Turn it on with `/sandbox` in a session and pick auto-allow or regular permissions, or set `sandbox.enabled` to true in `~/.claude/settings.json`. On Linux and WSL2, install `bubblewrap` and `socat` first. To check it works, ask Claude (not the `!` prompt, which runs unsandboxed) to run `touch ~/sandbox-probe`; it should fail.

A starting point for user settings that blocks SSH and AWS keys, strips a token, allows two registries and stops unsandboxed retries:

```json
{
  "sandbox": {
    "enabled": true,
    "allowUnsandboxedCommands": false,
    "failIfUnavailable": true,
    "network": { "allowedDomains": ["github.com", "*.npmjs.org"] },
    "credentials": {
      "files": [
        { "path": "~/.ssh", "mode": "deny" },
        { "path": "~/.aws/credentials", "mode": "deny" }
      ],
      "envVars": [ { "name": "GITHUB_TOKEN", "mode": "deny" } ]
    }
  }
}
```

There is no built-in credential deny list; only what you name is protected.

## The escape hatches

- **Unsandboxed retry**: after a failure Claude can retry with `dangerouslyDisableSandbox`. Setting `allowUnsandboxedCommands` to false (strict sandbox mode) removes it.
- **excludedCommands**: matching commands run with your full access. Docker does not work inside the sandbox, so `docker compose *` here is common; keep patterns narrow.
- **Weakening switches**: `allowAppleEvents` on macOS, `allowNestedUserNamespaces` and `enableWeakerNestedSandbox` on Linux each trade isolation for compatibility.

## Settings for a team

Administrators deliver keys through managed settings. `enabled`, `failIfUnavailable` and `allowUnsandboxedCommands: false` together make the sandbox admin-required, and Claude Code then ignores repository settings that loosen it, such as a repo's `excludedCommands` or `allowedDomains`. `allowManagedDomainsOnly` and `allowManagedReadPathsOnly` lock network and read lists to managed values. Native Windows has no sandbox, so with `failIfUnavailable` set Claude Code exits there; move those users to WSL2 or a container.

## Limits Anthropic lists

The documentation says the sandbox reduces risk but is not a complete isolation boundary. The proxy decides on hostname and does not inspect TLS by default, so a broad entry like `github.com` can be an exfiltration path and domain fronting is possible. Allowing a Unix socket such as `/var/run/docker.sock` effectively hands over the host. Broad write rules to `$PATH` directories or shell startup files can lead to code running elsewhere.

## When to put the whole agent in a box

If MCP servers, hooks and file tools need a boundary too, the documentation points to running the Claude Code process itself in a container, VM or the sandbox runtime. Your options:

| Option | Covers file tools and MCP | Runs on | What it costs you |
|---|---|---|---|
| Claude Code Bash sandbox | No | Your machine | Little setup; shell commands only |
| Dev container | Yes | Your machine | Image upkeep; shares your laptop's CPU and RAM |
| Local VM | Yes | Your machine | Strong boundary; heavy on a laptop |
| Hosted sandbox service | Yes, for what you send it | Rented machines | Easy to scale; usage bills and your code leaves your hardware |
| Potluck sandbox | Commands sent to it | A spare Mac or Linux box your team owns | Early software; you run the pool |

Disclosure: Potluck is made by Sengi, Inc., which sells hosted GitHub Actions runners. Potluck is a separate free project and runs without a Sengi account.

Potluck is not a replacement for Claude Code's settings. It is not a sandbox runtime of its own; it drives Tart on Apple silicon Macs and Kata, gVisor or Incus on Linux, and puts each sandbox on a machine that is not your laptop. On the OCI layer the default is no network at all. Trust still applies: the machine that runs a VM can read everything in it, including secrets, and the machine that runs a pool can read commands and output passing through it.

## How to start with Potluck

Install on macOS or Linux (no Go needed) and start the daemon:

```sh
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
potluck daemon --detach
```

Create a pool with `potluck create friends` (the first time it prints a Tailscale login URL). `potluck invite carl --host` makes a code for someone sharing a machine, who runs `potluck host '<code>'`; `potluck doctor` and `potluck host --dry-run` show what will be shared first. Then:

```sh
potluck sandbox create
potluck sandbox exec <id> -- uname -a
potluck sandbox rm <id>
```

`potluck isolation test <size>` checks that a box cannot reach the host, its LAN or its tailnet. To hand an agent sandbox tools, run `potluck --pool friends mcp`; see [give your AI agent a sandbox over MCP](/blog/give-your-ai-agent-a-sandbox-over-mcp) and [agent sandboxes](/agent-sandbox).

## What makes Potluck different

- When the Bash sandbox is not enough, it uses the Macs and Linux boxes your team already owns instead of rented machines.
- Each command an agent sends lands in a fresh VM or sandbox, and the same pool can also run GitHub Actions jobs (run by hand so far, not yet daily in Potluck's own CI).
- Agents get sandbox tools over MCP with `potluck --pool friends mcp`.
- A lent Mac goes back to its owner the moment they touch the keyboard or pointer; on Linux, hand-back works only as a time window today.
- It is free and open source (Apache-2.0) and runs without a Sengi account.

## Try it early

Potluck is early. Its makers want people with a spare Mac or Linux box to try it and say what breaks. Start a thread at https://github.com/sengirunners/potluck/discussions, and if you want to contribute, the good first issues are at https://github.com/sengirunners/potluck/issues.

## Questions people ask

### Is the Claude Code sandbox on by default?

No. Turn it on with `/sandbox` in a session or set `sandbox.enabled` to true in a settings file.

### Does the sandbox stop Claude from reading my .env file?

Only for shell commands. The Read tool follows permission rules, so add a Read deny rule as well as a sandbox `denyRead` or `credentials` entry.

### Does the sandbox work on Windows?

Not natively; commands run unsandboxed there. Run Claude Code inside WSL2 to get it.

### What is dangerouslyDisableSandbox?

It is the parameter Claude uses to retry a failed command outside the sandbox. Set `allowUnsandboxedCommands` to false to make Claude Code ignore it.

### Is the sandbox a full security boundary?

Anthropic's documentation says no: the proxy filters by hostname without inspecting TLS, and broad allowlists or sockets can undo it. For a harder line, isolate the whole agent process.

### Does Potluck replace Claude Code's sandbox?

No. It gives agents fresh VMs or sandboxes on machines your team owns, over MCP or the `potluck sandbox` commands. Keep your permission rules either way.

### Is Potluck free?

Yes. It is open source under Apache-2.0, and you can read the code at https://github.com/sengirunners/potluck.
