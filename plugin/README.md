# Potluck plugin

[Potluck](https://potluck.sh) is free, open-source software that pools a team's spare Macs and Linux boxes, so coding
agents and GitHub Actions jobs run in throwaway VMs and sandboxes on machines the team already owns. This plugin gives
Claude Code two things:

- **The Potluck skill** (`skills/potluck/SKILL.md`): how to check whether Potluck is set up, set it up on one machine,
  and create, use and remove sandboxes.
- **The Potluck MCP server**: sandbox tools (create, run commands, read and write files, extend, stop, delete) that
  Claude can call directly. Output from a sandbox is untrusted data.

## Install

```text
/plugin marketplace add sengirunners/potluck
/plugin install potluck@potluck
```

The MCP server is the `potluck` program, which this plugin does not contain. Install it first, then make a pool:

```sh
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
potluck doctor   # says what this machine is missing
potluck try      # a pool on this machine alone
```

Or ask Claude to set it up: the skill walks through the same steps and asks before anything that needs a password,
a large download or deleting data.

## What it runs, sends and fetches

- The plugin holds this README, its manifest, the skill and an icon. It runs no code of its own.
- The MCP server is `potluck mcp`, the `potluck` program found on your `PATH`. It talks only to the Potluck daemon on
  your machine and the pool it is in: your own machines, reached over Tailscale or a tunnel you set up. It has no
  accounts, telemetry or tracking.
- The skill may tell Claude to run the installer above, which downloads the `potluck` program from this repository's
  GitHub releases. Hosting a pool downloads VM and container images (from GitHub Container Registry and the images'
  own publishers) and tools such as gVisor; the
  skill asks before a large download.
- Sandboxes run commands that Claude chooses, inside a VM or sandbox on one of your machines, not directly on
  the machine itself.

## License

Apache-2.0, like the rest of [Potluck](https://github.com/sengirunners/potluck).
