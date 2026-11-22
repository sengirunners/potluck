# Layout of the source

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
| `cmd/zm-run-tart/`, `cmd/zm-run-local/`, `cmd/zm-run-conformance/`, `cmd/zm-e2b/` | the same programs under their old names ([coming-from-zm.md](coming-from-zm.md)) |
| `ghapp/` | GitHub App client for a runner broker |
| `install.sh`, `scripts/release*/`, `internal/upgrade/` | installs and upgrades: the install script, release build, signing and keygen, and `potluck upgrade`'s fetch and verify |
| `site/` | the landing page |
| `agentrunners/` | job definitions and adapters for vendor coding-agent runners |
| `boxport/` | host-side TCP forwarding for box port leases |
| `depscli/` | CLI flags and project file discovery for dependency layers |
| `grants/` | policy for who may put specific secret types into boxes |
| `hostswap/` | host swap checks and reporting for box placement |
| `issuers/` | secret issuers used to authorize credentials for jobs |
| `jobrule/` | rules for GitHub Actions jobs that may hold secrets |
| `macos/` | macOS app source and build resources |
| `mcp/` | MCP tools for working with boxes through the daemon |
| `menubar/` | Go bridge between the macOS menu bar and the daemon |
| `placement/` | filters and ranks candidate box bindings |
| `portstream/` | adapts authorized port operations to net.Conn |
| `internal/forkguard/` | selects safe runner labels for fork pull requests |
| `internal/workflowlint/` | checks workflow configuration for fork-safe CI |
| `internal/tarsafe/` | validates archive paths and rejects sparse entries |

One Go module, two layers:

- `kit/...`: KIT, the shared base: ids, config, secrets, store, events, authz, netguard, sqlbackup, release, swap,
  tailnet, tunnel, owners, project, core, api, client, server (the daemon shell) and teamlink (join, remotes,
  machine ops, heartbeat, proxy).
- COMPUTE, at the top level: `runtarget/` (boxes and sandboxes, `rt/`), `runners/` (GitHub runner broker),
  `usage/`, `hostprobe/` (host checks, share), `imagepins/` (`images.pin`), `computeapi/`, `computeclient/`, `boxcp/`, `boxpush/`,
  `wiring/` (daemon wirings `Kit` and `Compute`), `cmd/potluck-e2b/` (`internal/e2bgateway`), plus the packages above.
- Rule (`internal/layers`): KIT's non-test code imports no COMPUTE package, and nothing here depends on zm.
