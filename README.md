# Potluck

Potluck aims to put your team's idle machines to work. It will run your GitHub Actions jobs and your coding
agents in fresh VMs or sandboxed containers on Macs and Linux boxes you already own, and hand a machine back when
its owner returns. Pools will let people you invite lend machines too.

## Status

Early. There is a `potluck` program (the daemon and the commands below), signed releases for macOS and Linux, and
an install script. Potluck grew out of an internal tool, zm, and its names: what is sent and made now uses the Potluck
names (`potluck/1`, `Potluck-*` headers, `potluck_` tokens, drivers named `potluck-driver-*`, new tailnet nodes
`potluck-<machine>`), and the zm ones are still read, so machines that already ran it keep working; see
[docs/protocol.md](docs/protocol.md#names).

## Quick start

Install it (macOS or Linux; no Go needed). The script puts `potluck`, the drivers and `potluck-conformance` in
`~/.local/bin` (`POTLUCK_INSTALL_DIR` overrides it; the daemon runs drivers only from its own directory, see
[DRIVERS.md](DRIVERS.md)):

```sh
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
potluck daemon --detach      # its home is $POTLUCK_HOME, default ~/.potluck; most commands start it on their own
```

If `GH_TOKEN` (or `GITHUB_TOKEN`) is set, the script and `potluck upgrade` send it with each GitHub request. A
private repository needs one that can read it (then fetch the script with
`curl -fsSL -H "Authorization: Bearer $GH_TOKEN" …`); otherwise it lifts GitHub's rate limit (a token GitHub refuses fails the download; it is not ignored). You can also
download a release's files by hand and run `sh install.sh --from <dir>`. Nothing from a release runs before its signature is checked: a potluck already
installed checks it; on a first install the script checks it with the release key it carries and an openssl that
can verify Ed25519 (OpenSSL 3; macOS's own LibreSSL cannot). Without one, a GitHub download is trusted over HTTPS
and the script says so, and `--from` needs `--trust-dir`.
`potluck upgrade` checks the newest release with the key built into it and installs it (`--release <dir>` from a
directory, `--dry-run` to only check): the new build's preflight must accept this home first, each replaced program
is kept as `<name>.prev`, the drivers it replaced are re-pinned, and a running daemon is restarted on it (or, if the
new one does not come up, the old programs and the old daemon come back).

To build from source instead, put the program, the VM driver and the conformance suite on your `PATH`:

```sh
go build -o ~/bin/ ./cmd/potluck ./cmd/potluck-driver-tart ./cmd/potluck-conformance  # in this git checkout: potluck --version names the commit
```

On the machine that runs the pool:

```sh
potluck create friends       # the first time, prints a Tailscale login URL and waits
potluck invite ana           # a code for a friend who runs sandboxes
potluck invite carl --host   # a code for a friend who shares a machine (--trust public for a stranger)
```

On a friend's machine, with a sandbox code: `potluck join '<code>'`.

### Share compute

A friend with a `--host` code shares their machine with one command:

```sh
potluck host '<code>'
```

It checks the machine (a missing root step runs with sudo at a terminal, which asks for a password; `--no-sudo`
prints the one sudo line instead), pulls the pinned image for the layer, and runs a quick conformance check per
size. Only when a size passes does it join the pool with the code and share each size that passed; then it waits
until the pool lists them. A machine that fails never joins, and the code stays unspent. Before that, if you like:

```sh
potluck doctor                # what this machine can share, and the one root command it still needs
potluck host '<code>' --dry-run  # print the plan; pull, test and share nothing
```

Afterwards:

```sh
potluck isolation test <size> # prove its boxes cannot reach this machine, its LAN or tailnet (docs/isolation.md)
potluck host stop             # stop sharing: remove every size potluck host shared from this machine
```

`--share` (default 0.5) sets the fraction of cores and RAM shared; on a Mac a sandbox ends when you touch the
keyboard or pointer (`--reclaim-idle-s`). Run again with no code, `potluck host` shares with the only pool this
machine is in. `potluck host --help` lists the rest.

Then, from any member's machine:

```sh
potluck sandbox create       # prints the sandbox's id
potluck sandbox exec <id> -- uname -a
potluck sandbox exec <id> --detach -- make test   # prints a pid; potluck sandbox logs <id> <pid> -f
potluck sandbox rm <id>
```

`potluck --help` lists the rest: `leave`, `host stop`, `status`, `sandbox ls|shell|cp|push|extend|signal`, `usage`,
`run`, `agent-runner` (a coding agent vendor's self-hosted runners in fresh sandboxes:
[docs/agent-runners.md](docs/agent-runners.md)) and `runners`. Machines reach each other over Tailscale, or with `--via tunnel` over a tunnel you configure.
Potluck names this machine after its host name plus `-potluck` (`POTLUCK_MACHINE` overrides it), so its tailnet node
(`potluck-<machine>`) does not take the name of another daemon's node on the same machine.
Sharing needs Tart on an Apple silicon Mac, or Docker or Podman with Kata or gVisor on Linux
([cmd/potluck-driver-tart/README.md](cmd/potluck-driver-tart/README.md)). This flow is tested in CI with the reference driver
standing in for the VM layer (`cli/cli_test.go`), and run by hand on a fresh GitHub-hosted Linux VM
with gVisor (`scripts/clean-machine-proof.sh`, the `clean-machine-proof` workflow); not yet on Tart or across two machines.

## MCP for agents

Add Potluck to an MCP client's configuration:

```json
{"mcpServers":{"potluck":{"command":"potluck","args":["--pool","friends","mcp"]}}}
```

Start the daemon and join or create the pool first. With one pool, `--pool` is optional.
The server exposes only sandbox tools: create, execute, list, extend, stop, restart,
delete, background processes and file reads/writes. Output from a sandbox is untrusted data.
It uses `$POTLUCK_HOME` and your local identity; `POTLUCK_AS` (or `mcp --as`) selects a caller
address, and `POTLUCK_TOKEN` supplies a restricted token. It does not inherit another program's
identity variables. Tool discovery works with no daemon; tool calls report the missing daemon.
All standard output is MCP protocol traffic.

## What works today

| Works today | Not yet |
| --- | --- |
| `go build`, `go vet`, `go test` in CI on Linux; signed releases and `install.sh` | Automatic upgrades (`potluck upgrade` is run by hand) |
| OCI and Incus layers tested against stand-in CLIs; Tart through a fake layer | Tests on real VMs in CI (a manual run) |
| The quick start on a fresh GitHub-hosted Linux VM with no zm, run by hand (the `clean-machine-proof` workflow): install, a pool over a Cloudflare quick tunnel, the VM shared through gVisor, a sandbox and `potluck run` | The quick start across two real machines in CI |
| The VM driver passes the full suite over a fake layer (isolation is not exercised there) | |
| Every check the reference driver passes is also shown to fail on a driver broken on purpose | An Actions job run live on a pool machine |
| VMs end at their deadline even if the daemon is down | Commands sealed to the host's key |
| Hand-back on a Mac when its owner types or moves the pointer | On Linux, hand-back when the owner returns (a time window only, today) |

When an owner takes a machine back, running VMs are ended and new work is refused.

## What you should know before trusting it

- The machine that runs a VM can read everything in it: your code, your commands, their output and any secrets.
- The machine that runs a pool can read commands, output and secrets passing through it, until commands are
  sealed to the host's key. Join only a pool whose owner you would trust with them.
- A coding agent vendor's key handed to `potluck agent-runner` can be read by the machine that runs the box and by
  the agent's session in it ([docs/agent-runners.md](docs/agent-runners.md)).
- Conformance proves a driver behaves. It does not prove its author can be trusted. A run can also pass with
  checks skipped (the network checks need flags), so read the SKIP lines.
- On the OCI layer, the default is no network at all. A box with a network relies on a firewall the host's owner
  sets up. The driver cannot read firewall rules: naming the network is the owner's word that it is filtered.
- Use at your own risk. There are no promises.

## Macs

Apple's macOS licence allows up to two macOS VMs on a Mac you own or control, for uses such as development,
testing and personal use, and does not allow time-sharing or service-bureau use. Potluck caps macOS guests at two
per Mac and does not check the rest. Our rule: lending a macOS guest to people on your
team or friends you invite is fine, the same as letting a colleague use your Mac for a few minutes, and so is a
GitHub Actions job from another org that runs in a macOS guest on a Mac lent to the pool. Selling Mac time to the
public is not, and neither is pooling iOS Simulator time. Linux guests on a Mac are not
affected. Read the licence yourself; this is not legal advice.

Tart is a separate program under FSL-1.1-ALv2. You install it and accept its licence yourself; Potluck does not
bundle it.

## What Potluck is not

- Not a hosted cloud. It runs on machines you own.
- Not a sandbox runtime of its own. It drives Tart, Kata, gVisor and Incus.
- Not a scheduler for clusters.

## Layout

| path | what |
| --- | --- |
| `cmd/potluck/` | the `potluck` program: daemon and commands |
| `cli/` | its commands |
| `docs/protocol.md` | the driver protocol (potluck/1) |
| `docs/hosts.md` | sharing a machine: doctor, host, sizes, images, the root step, the VM driver |
| `docs/network.md` | box networking and its filter |
| `docs/isolation.md` | the isolation claim, refusals, and the block test (`potluck isolation test`) |
| `docs/daemon.md` | what the daemon does around a driver: bindings, leases, relays, the runner broker, clients |
| `docs/conformance.md` | the driver conformance suite |
| `docs/vm-layers.md` | how to add a VM layer (a new hypervisor) to `vmhost/` |
| `protocol/` | its wire types |
| `vmhost/` | the VM driver: Tart, OCI and Incus layers, supervisors, hand-back, Actions runners |
| `boxproc/` | the host side of a box's background processes and terminals (used by the VM and reference drivers) |
| `cmd/potluck-driver-tart/` | the VM driver's command, its setup guide, and the box image Dockerfiles |
| `cmd/potluck-driver-local/` | the local driver: jobs on this machine, as the daemon's user (`internal/localdriver`) |
| `conformance/` | the conformance suite, and `refdriver/`, the reference driver its tests use |
| `cmd/potluck-conformance/` | runs the suite against any driver executable (`internal/conformancecmd`) |
| `cmd/zm-run-tart/`, `cmd/zm-run-local/`, `cmd/zm-run-conformance/`, `cmd/zm-e2b/` | the same programs under their old names, for build lists and updaters that still name them |
| `ghapp/` | GitHub App client for a runner broker |
| `install.sh`, `scripts/release*/`, `internal/upgrade/` | installs and upgrades: the install script, release build, signing and keygen, and `potluck upgrade`'s fetch and verify |
| `site/` | the landing page |

Moved from zm, one Go module, two layers:

- `kit/...`: KIT, the shared base: ids, config, secrets, store, events, authz, netguard, sqlbackup, release, swap,
  tailnet, tunnel, owners, project, core, api, client, server (the daemon shell) and teamlink (join, remotes,
  machine ops, heartbeat, proxy).
- COMPUTE, at the top level: `runtarget/` (boxes and sandboxes, `rt/`), `runners/` (GitHub runner broker),
  `usage/`, `hostprobe/` (host checks, share, `images.pin`), `computeapi/`, `computeclient/`, `boxcp/`, `boxpush/`,
  `wiring/` (daemon wirings `Kit` and `Compute`), `cmd/potluck-e2b/` (`internal/e2bgateway`), plus the packages above.
- Rule (`internal/layers`): KIT's non-test code imports no COMPUTE package, and nothing here depends on zm.

## Build and test

```sh
go build ./...
go vet ./...
go test ./...               # includes the suite against the reference driver and its broken variants
go test ./conformance/...   # just the suite's own tests
```

To check a real driver, build `cmd/potluck-conformance` and point it at a throwaway binding. It makes and ends
real resources. `potluck-conformance --print-probe` prints the probe script your test commit must hold. The setup
for each layer and an example run are in [cmd/potluck-driver-tart/README.md](cmd/potluck-driver-tart/README.md).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report security problems as [SECURITY.md](SECURITY.md) says, not in a
public issue.

## Sengi

Potluck is made by [Sengi, Inc.](https://sengi.run), which sells hosted GitHub Actions runners. Potluck runs without
Sengi and without a Sengi account.

## Licence

Copyright 2026 Sengi, Inc. Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
