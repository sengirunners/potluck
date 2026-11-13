# potluck-driver-tart

Built from `cmd/potluck-driver-tart` (and under its old name `zm-run-tart` from `cmd/zm-run-tart`); the code is in `vmhost`.

A run target ([driver protocol](../../docs/protocol.md); `kind = "run"`, `isolation = "ephemeral"`, `credentials = false`) that runs each job in a
throwaway [Tart](https://github.com/openai/tart) VM on this Mac.

Per run: an APFS clone of a base image, booted with Softnet, with no window, clipboard, audio or `--dir`, so
the guest sees nothing of the host's. Softnet keeps the guest off private and link-local ranges. It would let the
guest reach this Mac through its gateway, so the driver blocks the gateway except DNS (`--net-softnet-block=@host`), and
outbound mail to anywhere (`out proto tcp port 25`, `465`, `587`), so a guest cannot send spam from your IP; Softnet lets
no IPv6 out. It holds a clean checkout of exactly `commit`. The script runs through
the Tart guest agent and its exit code passes through. `acquire` answers once the script has started (about 6 s
for an Ubuntu guest). A detached supervisor per run ends it at `expires_at` (exit 124), on release, on reclaim,
or when the script exits, then deletes the clone, whether or not zm is up. If a supervisor dies, the next call
to the driver ends its VM. `expires_at` counts from the script's start, not the clone's; the lifetime cap counts
from `acquire`.

## Setup (once, by the owner)

```sh
# Tart from its release (Homebrew's cirruslabs tap is stale); Softnet must be setuid root:
sudo install -o root -g wheel -m 4755 softnet /usr/local/bin/softnet
tart clone ghcr.io/cirruslabs/ubuntu:latest zmrt-base-ubuntu        # Linux: ~5 GB, uncapped
tart clone ghcr.io/cirruslabs/macos-tahoe-base:latest zmrt-base-macos # macOS: at most 2 running per Mac
```

The base image needs `git` and the Tart guest agent (Cirrus images have both). zm drives a guest only through that agent: before a VM serves
anything, the driver stops its SSH and macOS Screen Sharing (Cirrus images keep both on with admin/admin, and Softnet
takes inbound connections) as root through `sudo -n`, and ends the VM while port 22 or 5900 still answers.

## Config (the binding's driver keys)

| key | default | meaning |
| --- | --- | --- |
| `image` | required | the local Tart VM every run clones; its OS picks the slot pool |
| `remote` | required | the repo's remote. A URL is fetched inside the guest (`--depth 1` of `commit`). A local path, or `fetch = "host"`, is fetched on the host and copied in as a bundle, so private repos need no credential in the guest. A URL with credentials and guest fetch is refused. |
| `fetch` | `auto` | `auto`, `guest` or `host` |
| `scripts_dir` | repo root | where script names resolve. An executable file runs directly, any other runs with `sh`. |
| `network` | `softnet` | `nat` is for development only: the guest reaches the host and the LAN, so the binding describes `isolation = "none"` (on every layer: Softnet off on Tart, the fence unchecked on Incus, any `oci_network` on OCI), and zm refuses it as a box |
| `linux_slots` / `macos_slots` | 2 / 2 | concurrent runs. macOS is capped at 2 per host, counting every running macOS VM. |
| `cpu`, `memory` | the image's | per-VM CPUs and MB |
| `ttl_max_s` | 14400 | lifetime cap since acquire |
| `boot_timeout_s` | 90 | clone, boot and checkout must finish within this |
| `reclaim_idle_s` | off | the owner counts as active while HID idle time (keyboard, pointer) is under this |
| `window` | always | `HH:MM[:SS]-HH:MM[:SS]` local time, may wrap midnight: work only inside it, every day |
| `windows` | always | `window` with weekdays: entries split by `;`, each `[days ]HH:MM-HH:MM`, days `mon`, `mon-fri` or `sat,sun` (none: every day); a window past midnight belongs to the day it opens. Set `window` or `windows`, not both. Example: `mon-fri 18:00-08:00; sat-sun 00:00-00:00` |
| `drain_deadline_s` | 1800 | how long running work has once a drain begins (leaving the window, or `host set drain`) before it is reclaimed, at most a week; `0` reclaims at once. A binding with `window` that leaves this unset now gives running work this long after the window closes, where it was reclaimed at once before |
| `host_owner_uid` | the driver's uid | who must own the host override file and its directory (or root); set it when the driver runs as a run user other than the host's owner |
| `state_dir` | `~/.potluck-driver-tart` (`~/.zm-run-tart` where that already exists: VM names hash this path, so it is never moved) | records, logs and the host fetch cache |
| `persist_max` | 4 | a box binding's kept disks ([protocol: Kept disks](../../docs/protocol.md#kept-disks-persist-stop-start)), stopped or not, in this state directory; `0` turns persistence off. With it (and `vm` `tart` or `oci`) describe lists capability `persist`, verbs `stop` and `start`, and `disks` |
| `persist_min_free_gb` | 10 | a persist acquire or a start is refused while the layer's disk directory (Docker's root dir or `$TART_HOME`) has less free space than this; `0`: no floor |
| `volume_max_gb` | 20 | (`potluck host --volume-max-gb`) a box binding's named volumes ([protocol: Named volumes](../../docs/protocol.md#named-volumes)) per owner, in GiB: a new volume past it deletes the owner's least recently used volume no box holds, else it is refused; `0` turns volumes off. With it (and `vm` `oci`) describe lists capability `volumes`. A volume is a docker named volume `<state prefix>v<16 hex>`; a `cow` mount is an overlay volume of the box's own (rootful Docker) |
| `volume_max_mb` | 0 | a finer per-owner cap than `volume_max_gb`, when set |
| `volume_check_s` | 60 | how often a supervisor measures a box's `rw` volumes and `cow` copies; a box past the cap (or the host past `persist_min_free_gb`) ends as `reclaimed`, why `volume quota` |
| `volume_project_max_gb` | 0 (4x the owner's cap) | one project's volumes and copies on this binding (acquire's `volume_project`) |
| `volume_binding_max_gb` | 0 (8x the owner's cap) | all volumes and copies on this binding |
| `volume_keep_s` | 2592000 (30 days) | a volume no box named for this long is deleted by the sweep (at least 60) |
| `shelf_max_gb` | 0 | (`potluck host --shelf-gb`) `vm=oci`: the package shelf's size cap on the `potluck-cache` volume (A cache, below); `0` fills nothing. Filled by `potluck host shelf fill` and, with `shelf_fill=boxes`, from boxes' wants; the oldest npm blobs and Go module versions are evicted past it, and fills stop at it ([docs/shelf.md](../../docs/shelf.md)) |
| `shelf_keep_s` | 2592000 (30 days) | a shelf blob or Go module version no fill named in this long is evicted (at least 60) |
| `shelf_fill` | `seed` | (`potluck host --shelf-fill`) `off`, `seed` (only what the owner seeds) or `boxes` (also what boxes ask for) |
| `shelf_fill_mbit` | 16 | the host's download rate for shelf fills, in Mbit/s (at least 1) |
| `shelf_fill_delay_s` | 300 | how long a live box's requests wait before the host fetches them (or until the box ends) |
| `warm` | 0 | a box binding keeps this many boxes booted and idle; an acquire of the default image (no persist, volumes or jit_config) takes one at once. Warm boxes hold the binding's slots (a cold acquire ends one for its slot) but no `admission_url` lease until taken |
| `persist_keep_s` | 2592000 (30 days) | how long a stopped box's disk is kept before the driver deletes it (at least 60). On `vm=oci` a kept box is the same container, its HOME also on a volume `<vm>-home`; under runsc (Docker) it gets the annotation `dev.gvisor.flag.overlay2=none` so its root's changes survive a stop too (runsc's default overlay drops them; a runsc whose override allowlist lacks overlay2 fails the kept box's boot, and the error names its version). Stopped boxes are live handles: rebinding or removing the binding is refused until they are released (`zm sandbox ls` lists them, `zm sandbox rm` deletes each disk) |
| `tart` | `tart` on PATH | the tart binary |
| `vm` | `tart` | the VM layer: `tart` (a Mac) or `oci` (a Linux box, below). |
| `oci_runtime` | required with `vm=oci` | a runtime that is a boundary: Kata (`io.containerd.kata.v2`, `kata-fc`, ...) or gVisor (`runsc`, or `runsc-root` for `root`/`docker`). `runc`, `crun` and `youki` are refused. |
| `oci_cli` | `docker` | the container CLI: verified local Docker; Podman and nerdctl are unavailable for isolated boxes |
| `root` | off | `on`, tart: a box binding on Softnet keeps its macOS image's passwordless `sudo` (Cirrus Labs' images give `admin` it): root inside a throwaway VM, as on GitHub's own macOS runners. Nothing in the VM changes; describe lists capability `root` for a macOS image only, and a Linux image on such a binding is refused (its root would hold `NET_ADMIN` and `NET_RAW`). Refused with `network=nat`, on a run binding, and with `docker`. Without it, conformance's root group fails an image whose sudo works. Root and Docker below. `on`, oci: the box's user has working `sudo` (no password); describe lists capability `root`. Drops `no-new-privileges` and `MKNOD` (and `NET_RAW` with no network) from the create and nothing else; needs a runtime that honours setuid in the box: `runsc-root` (docker only, checked against docker's runtimes before every box) or Kata, not plain `runsc`. Root and Docker below. |
| `docker` | off | `on` (oci only; implies `root`): rootless Docker in the box, started by PID 1 (`zm-dockerd`), `DOCKER_HOST` set for every exec; describe lists capability `docker`. Root and Docker below. |
| `oci_network` | `none` | `none`: no network, checkouts come in from the host (`fetch=auto` becomes host). Else a network you made for zm with egress filtered (below); the shared ones (`bridge`, `host`, `default`) are refused for isolated runs. A box binding lists capability `net-off` and takes `acquire`'s `net`: `off` makes that box with network `none` (and a host-fetched checkout) whatever the binding's network, `on` puts it on the binding's network (see `runner_dir`). Every box's acquire answer says `net`: `off` for a box made with network `none`, else `on`. zm sends `on` only for a trusted host's box, `off` for a public host's or when the renter asks (`zm sandbox create --net off`). |
| `oci_dns` | `1.1.1.1,8.8.8.8` on a named network | the box's resolvers, comma-separated public IPs (at most 3; IPv6 only in `2000::/3`; private, CGNAT, loopback, link-local, multicast, documentation, Teredo, 6to4 and NAT64 are refused: the filter drops them, they can tunnel to a private address, or they are the host). Passed as `--dns` and, for docker and podman, as a read-only `/etc/resolv.conf` zm writes per box (`state_dir/oci-resolv/<vm>.conf`, removed with the box): on a user-defined network their own resolv.conf names Docker's embedded `127.0.0.11` or Podman's aardvark-dns on the gateway, which gVisor, Kata and the filter cannot reach. Refused with `oci_network=none` (and with a `container:`/`ns:` network, which keeps that namespace's DNS). DNS notes below. |
| `incus_profile` | required with `vm=incus` | the profile every run's VM uses: the root disk and one NIC on your zm bridge with `security.port_isolation=true` and `security.acls` set (checked before boot) |
| `incus_project` | `zm` | the Incus project made for zm (never `default`); its limits cap your share |
| `incus_remote` | the CLI's default | a remote reached with a client certificate restricted to that project; a unix-socket remote (incus-admin) is refused |
| `incus_conf` | the CLI's own | `INCUS_CONF` for the incus CLI: the directory holding that certificate and remote |
| `incus` | `incus` on PATH | the incus binary |
| `admission_url` | off | the host's admission controller: zm asks it for room before every VM (contract below); needs `cpu` and `memory` |
| `admission_token_file` | none | a 0600 file whose contents go as `Authorization: Bearer` on every admission call |
| `admission_ttl_s` | 120 (at least `boot_timeout_s` + 30) | the lease TTL; the supervisor renews at a third of it |

### Boxes (`kind = "box"`)

A box is a VM kept up for its lease, which its holder drives with `exec` ([protocol: Boxes](../../docs/protocol.md#boxes): a Potluck sandbox).

- `acquire` answers once the guest answers `exec`. With `ref`/`commit`, the box holds a clean checkout of exactly
  that commit, which is `exec`'s default `cwd`. Without them it starts empty, in `HOME`.
- `exec` runs `argv` (never a shell string) under a Perl wrapper in the guest. The wrapper gives the command its
  own process group and enforces `timeout_s` with exit 124. When the command exits, the wrapper kills whatever
  it left behind, so nothing holds the output open.
- `stdout` and `stderr` keep their first 1 MiB each. The image needs `perl` (Ubuntu's `perl-base`, macOS's
  `/usr/bin/perl`), plus `git` for a checkout, and `nc` for the network checks `zm host share` runs (Debian and
  Ubuntu: `netcat-openbsd`; `ip` from `iproute2` for the VM-to-VM check). A box image without `nc` fails them.
- `exec`'s `secrets` (capability `secrets`): framed ahead of the command's stdin, written by the guest wrapper as
  0600 files in a fresh 0700 dir under `/dev/shm`, named by `POTLUCK_SECRETS` and `ZM_SECRETS` for that command
  only, and removed when it ends or times out. Never in argv, the environment or the VM's config, and never on a
  disk: the wrapper checks the guest's `/proc/self/mounts` shows the dir on a tmpfs or ramfs and `/proc/swaps` lists
  no swap but zram (a tmpfs page can be swapped out), and the call is refused (`forbidden`) where they do not. There
  is no `TMPDIR` fallback. A guest image meant for secrets needs no swap file, or zram only.
- The `secrets` verb (capability `box-secrets`) keeps secrets for the box's life in `/dev/shm/potluck-secrets`,
  under the same rules: each value replaced by a write beside it and a rename, every later exec, spawn and
  terminal without secrets of its own sees the dir in `POTLUCK_SECRETS` (only when it is a directory, not a link, of
  the command's user), and it goes with the guest's memory when
  the box ends or a kept disk stops.
- Both are Linux guests' only. A macOS guest has no memory filesystem the driver can prove (no `/dev/shm`, no
  mount table; a RAM disk would need `hdiutil` and `diskutil` run in the guest, mounted where every user can see
  it): a binding whose image is macOS lists neither capability, and both are refused there.
- A box is `ready` until it is released, expired (no exit), reclaimed or lost (its VM stopped). Every box is a
  fresh clone, deleted at the end.

| key | meaning |
| --- | --- |
| `size` | the binding's one size name; `describe.size` is `{name, os, cpu, memory_mb}` |
| `images` | comma list of OCI references pinned by digest (`ref@sha256:<64 hex>`), the first the default; `acquire`'s `image` picks one, and its answer carries that `image_digest`. Pull them first (`tart pull`). |
| `cpu`, `memory` | required for a box |
| `runner_dir` | the GitHub runner's directory in the image (relative to the guest user's home unless absolute). Set, the binding lists `github-jit` and takes `acquire`'s `jit_config`: the box's first command is that runner, its config handed over on stdin only. The authority registers and deregisters the runner. With `vm=oci` it needs an `oci_network` other than `none` (a runner must reach GitHub), which runner boxes get (an acquire with a `jit_config`, guarded or not: `jit_guard` is the caller's own), as does a box acquired with `net` `on` (zm sends it for a trusted host's box: a sandbox or a job reaches the internet through the same filter); a box acquired with `net` `off`, or with `net` unsaid (an older zm), has no network and a host-fetched checkout. `zm host share` sets it from the image's pin or `--runner-dir`. |

### GitHub runner version

Before a box's `jit_config` runner starts, the supervisor reads its version (`bin/Runner.Listener --version`).
A just-in-time runner never updates itself, and GitHub registers a deprecated one and then refuses it work, so
below the pinned minimum (`runnerMinVersion` in `vmhost/runner_refresh.go`, now v2.338.2) the pinned release
replaces the image's `bin/` and `externals/` (its `.env`, `.path` and `_work` stay). The tarball (osx-arm64,
linux-arm64 or linux-x64) comes from a host copy under `<state_dir>/cache/runner/`, downloaded once while the VM
boots and kept only if its sha256 is the pinned one, and is checked against that sha256 again in the guest
before anything is extracted; without a host copy the guest downloads it (`curl`, https only) under the same
check. A refresh that fails ends the start: acquire answers `unavailable` with the reason, and no runner is
started.

The driver never holds a GitHub token. A binding with `runner` set is refused: GitHub runners are box bindings
with `runner_dir`, and the runner broker on the authority mints each one's `jit_config` (`docs/daemon.md`).

While the reclaim policy says no, `describe` answers `available: false` with `why` and `acquire` is
`unavailable`. Owner input inside the window (or an idle time that cannot be read) ends running VMs within about a
second as `reclaimed` (no `exit`, never billed: [protocol: Reclaim](../../docs/protocol.md#reclaim)), with `why`. Leaving the window drains instead (below). Spend is in `slot_min`, counted from acquire to the end. `describe` also reports `slots` (`os`, `total`, `free`) and
`network`.

## The host switch: open, drain, closed, auto

The owner of a shared host overrides every binding on it with one command:

```sh
potluck-driver-tart host set <open|drain|closed|auto> [--for 2h | --until 23:00] [--deadline 30m]
```

| mode | new work | running work |
| --- | --- | --- |
| `open` | starts, whatever the window or idle time says (slots, `admission_url` still apply) | runs |
| `drain` | refused | finishes; a GitHub runner still waiting for a job ends at once, and a runner box ends when its runner exits; anything still running at the deadline (`--deadline`, else `drain_deadline_s`) is reclaimed, its `why` ending in "the drain deadline passed" |
| `closed` | refused | reclaimed now |
| `auto` | the binding's policy | the policy: owner input inside the window reclaims at once, leaving the window drains |

- `--for` and `--until` (local `HH:MM`, the next one, or an RFC 3339 time) return the host to `auto` then.
- The command writes `/Users/Shared/Potluck/host-override.json` on a Mac (`/etc/potluck/host-override.json`
  elsewhere: make `/etc/potluck` yourself, owned by you, mode 0755) by a temp file and a rename, as you, mode 0644:
  a run user other than you reads it and cannot write it. No file is `auto`. The file and its directory must be
  owned by `host_owner_uid` (or root); one another user made reads as unreadable, below. `/Users/Shared` is
  writable by every user, so make `/Users/Shared/Potluck` (any `host set` does) when you set the host up.
- A drain's deadline counts from when it began: `host set drain` again changes the deadline, not the start.
- A file that cannot be read as an override (bad JSON, an unknown mode or field, a symlink, another owner, or the
  file or its directory writable by others than its owner) refuses new work; running work goes on, unless the
  binding's policy alone would end or drain it.
- A drain ends a runner until it prints that it took a job, so a job assigned in the second before that line is
  cut.
- There is no status subcommand: `describe` carries `host` (`mode`, `state` `accepting`, `draining`, `closed` or
  `holding`, `until`, and during a drain `drain_since`, `drain_deadline` and `drain_cut`, how many it cut), read
  through the daemon.

## Linux boxes (`vm=oci`)

Linux boxes lend throwaway machines with the same driver: build
it with `GOOS=linux go build ./cmd/potluck-driver-tart`. Each run is a container under a runtime that is a real boundary,
created with no mounts (on a network, but a read-only `/etc/resolv.conf`: `oci_dns`), no new privileges (but with
`root=on`, below) and a process limit, its checkout copied in, removed after.

- **Kata Containers** where `/dev/kvm` exists (bare metal, or a VM with nested virtualisation): each run gets its
  own Firecracker or Cloud Hypervisor microVM. Install Kata and register it with the container engine
  (`/etc/docker/daemon.json` `runtimes`, or containerd's `io.containerd.kata.v2`).
- **gVisor** (`runsc`) where there is no KVM: a user-space kernel, weaker than a VM, far stronger than runc.
- The image needs `sh`, `git` and, for a box, `nc` and `ip` (the network checks). `buildpack-deps:bookworm-scm`
  has no netcat, so build on it, then push and pin the result by digest:

```dockerfile
FROM buildpack-deps:bookworm-scm
RUN apt-get update && apt-get install -y --no-install-recommends netcat-openbsd iproute2 iputils-ping \
  && rm -rf /var/lib/apt/lists/*
```
- zm's own such image is [images/linux/Dockerfile](images/linux/Dockerfile) (GitHub's runner image plus those tools),
  built and pinned by `.github/workflows/box-images.yml`. `zm host share` and `zm pool host` lend the pins in
  `imagepins/images.pin` when no `--image` is given (on a Mac, Cirrus Labs' `macos-tahoe-base`).
- `reclaim_idle_s` is Mac-only (keyboard and pointer idle); on Linux use `window`.
- On docker with systemd, `zm host doctor` prints the one root line that makes Potluck's own such network, `potluck-box`,
  with its filter kept across reboots by a systemd unit (`potluck-box-net.service`, or `zm-box-net.service` on a machine set up as zm; [docs/network.md](../../docs/network.md)), and
  `zm host share` / `zm pool host` use it for boxes that serve GitHub runners. Its drops sit in the raw table's
  PREROUTING, where no other daemon's ACCEPT comes first (tailscaled's jumps at the top of FORWARD and INPUT would
  outrun DOCKER-USER rules such as those below). Elsewhere, by hand:
- A network for jobs that need the internet: make one for zm and drop its traffic to private, CGNAT (tailnet),
  link-local (metadata) and the host's own addresses, and outbound mail (tcp 25, 465, 587) to anywhere, for example
  with Docker's `DOCKER-USER` chain (the network has no IPv6 unless made with `--ipv6`; if it has, add the same
  rules with `ip6tables` for `fc00::/7`, `fe80::/10` and the mail ports):

```sh
docker network create zm-egress --subnet 172.30.0.0/24
for n in 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10 169.254.0.0/16; do
  sudo iptables -I DOCKER-USER -s 172.30.0.0/24 -d $n -j DROP; done
sudo iptables -I INPUT -s 172.30.0.0/24 -j DROP   # nothing on the host itself (DNS goes to oci_dns's public resolvers)
sudo iptables -I DOCKER-USER -s 172.30.0.0/24 -p tcp -m multiport --dports 25,465,587 -j REJECT --reject-with tcp-reset   # no outbound mail
```

then `oci_network=zm-egress`. zm cannot read firewall rules: naming the network is the owner's word that it is
filtered, mail included: `zm host share --oci-network` refuses it without `--net-mail <an outside mail server>`,
which conformance then checks from a box; a size binds only if that check passed (the server must open from this
machine). `oci_network=none`
has no network, so nothing leaves at all.

DNS on a named network (`oci_dns`) has limits; each fails safe (the box gets no DNS, or no box starts, and the
conformance DNS check fails before anything binds):

- docker and podman bind the box's resolv.conf from `state_dir` with `--mount type=bind,...,readonly`, which fails
  when the source is missing. So a remote engine (`DOCKER_HOST` or a podman connection to another machine) or a
  confined one (snap docker cannot read dotfiles in your home, such as `~/.potluck-driver-tart`) refuses every box. Use a
  local engine, or a `state_dir` it can read.
- With SELinux enforcing, the bind is not relabelled (no `:z`), so the box may not read it and resolves nothing.
- nerdctl gets `--dns` and `--dns-search .`, and writes the file itself: it still copies the host's resolver
  options, puts slirp4netns's resolver first when rootless, and binds the file writable inside the box.

### Root and Docker (`root=on`, `docker=on`)

[Protocol: Root and Docker](../../docs/protocol.md#root-and-docker). What each layer gives, measured on dev hardware on
2026-11-09 (runsc release-20261102.0, docker 29, zm's Linux image):

| layer | sudo | docker run | how |
| --- | --- | --- | --- |
| oci, gVisor, default box | no | no | `no-new-privileges`, and runsc ignores setuid bits without `--allow-suid` |
| oci, gVisor, `docker=on` on `runsc-root` | yes | yes, rootless | the create drops `no-new-privileges`, `MKNOD` and `NET_RAW`; runtime `runsc-root` = runsc `--allow-suid` |
| oci, Kata, `docker=on` | yes, expected (a guest kernel) | rootless bridge mode; runtime acceptance pending (`/dev/net/tun`, user namespaces, netfilter); see `docs/docker-services.md` | as gVisor, on the Kata runtime |
| tart, macOS | the image's: Cirrus Labs images give `admin` passwordless sudo (measured 2026-11-11 on `macos-tahoe-xcode`); a binding must say `root=on` to lend it | no Docker in a macOS guest | full VM; `root=on` declares it, `docker` refused |
| tart, Linux guest | the image's | the image's (a full VM kernel: rootful docker works if installed) | full VM; `root`/`docker` keys refused, the image decides |

On tart, `root=on` is a declaration: the driver already needs the image's sudo to close SSH and
Screen Sharing before a VM serves, so it cannot take sudo away; without `root=on` such a box fails conformance
instead of lending root unannounced. Root in a macOS guest may do anything inside the VM, so its fence is outside
it: the VM itself, and Softnet on the host (it enforces the VM's MAC address and filters what leaves). A macOS
guest has no Linux bounding set, so conformance's root group skips those three checks for it and instead runs the
net probe under `sudo`: its control must see a port open in the box, and no `--net-forbid` or `--net-mail` address
that opens from the host may open from root's shell; with no such address the check fails. `potluck host --root`
(and `zm host share --root` / `zm pool host --root`) binds it.

`zm host share --docker` sets it up, and a re-run without the flag keeps it (`--docker=false` turns it off). On
docker with gVisor it registers `runsc-root` once: it copies `/etc/docker/daemon.json` to
`daemon.json.zm-bak-<date and time>` first (gVisor's own `daemon.json~` is rewritten by every `runsc install`, so
after zm installed gVisor it no longer holds your file), runs `<runsc> install --runtime=runsc-root --clobber
--download-sidecars=NEVER -- --allow-suid`, and reloads docker (`systemctl reload docker`, else SIGHUP to
`/var/run/docker.pid`). That is one new key; the `runsc` entry, every default box and any container of your own
are unchanged. It binds each size with `oci_runtime=runsc-root docker=on`, and conformance group `root` must
pass first on every image it lends. By hand, the same daemon.json entry is:

```json
"runsc-root": {"path": "/usr/local/bin/runsc", "runtimeArgs": ["--allow-suid"]}
```

The path must be the one docker's `runsc` entry has (both resolved: PATH, symlinks) and the args exactly that:
doctor and the driver (before every box with root, and every start of a kept one) read docker's runtimes and refuse any other `runsc-root` (runc, another runsc, extra flags
such as `--net-raw` or `--network=host`).

Why it is safe: the box keeps Docker's default capabilities less `MKNOD` and `NET_RAW`,
and gets no `--cap-add`, device or host mount. sudo reaches only the container's bounding set, and under gVisor
those capabilities are the Sentry's, not the host kernel's (runsc holds none on the host), so `--allow-suid` only
lets a process inside take the box's own capabilities; conformance checks root's `CapBnd` has no `NET_ADMIN`, `NET_RAW` or `MKNOD`. Docker runs rootless (rootlesskit's user namespace, slirp4netns), so its
containers' traffic is the box user's own sockets through the box's filter. Not given, because each lets a box
act on its neighbours on the shared bridge: `NET_ADMIN` (a rootful dockerd needs it; with it root re-addressed
`eth0` to a neighbour's address and MAC and answered for it), and runsc `--net-raw` /
`--allow-packet-socket-write` (gVisor's documented Docker-in-gVisor bridge setup).

A kept box (`persist`) keeps the root and docker it was made with, since `start` reuses its container: a start
after the binding's `root` or `docker` changed is refused (`conflict`; the disk stays kept until `zm sandbox rm`
or its time). The limits:

- inside a box, `docker run` and `docker build` have no network unless given `--network host` (the internet,
  through the box's filter); no bridge between containers, and `-p` publishes nothing outside the box;
- storage is `vfs` under `HOME` (overlay inside gVisor needs a memory upper), so big images unpack slowly; a kept
  box keeps its images;
- `runsc-root` is docker's: podman and nerdctl get no root on gVisor.

## Linux boxes with Incus (`vm=incus`)

Each run is a KVM virtual machine under Incus. zm never uses incus-admin: it reaches Incus with its own client
certificate, restricted to one project, so it cannot see or touch any other project's instances. Before each boot
it reads the VM's expanded config and refuses a host path, any device but the root disk and NICs, any `raw.*` key,
and (for an isolated run) a NIC without port isolation and an ACL, or whose ACLs (read through that certificate) drop or
reject no outbound mail: tcp 25, 465 and 587 to anywhere. An incusd restart is not read as the VM going;
a VM whose supervisor goes is deleted, retried through such a restart.

What the owner makes once (as root; no Potluck command prints these yet):

- a project for zm with limits for your share (`limits.instances`, `limits.cpu`, `limits.memory`), made
  `restricted=true` with `restricted.devices.nic=managed` and `restricted.devices.disk=managed`, so Incus itself
  refuses host paths and unmanaged NICs (zm also checks each VM before and after it starts);
- a bridge for zm, and an ACL on it whose egress rejects private IPv4 (RFC 1918), CGNAT/tailnet `100.64.0.0/10`,
  link-local and metadata `169.254.0.0/16`, the host's own addresses, IPv6 ULA `fc00::/7`, link-local `fe80::/10` and
  your LAN's global IPv6 prefix, rejects outbound mail (`incus network acl rule add <acl> egress action=reject
  protocol=tcp destination_port=25,465,587`; zm refuses to boot without it), and allows the rest (DNS included);
- a profile in that project: a root disk, and `eth0` on the bridge with `security.port_isolation=true` and
  `security.acls=<that ACL>`;
- a client certificate trusted with `incus config trust add-certificate <crt> --restricted --projects <project>`, and
  a remote for it (`incus remote add <name> https://<this host>:8443`) in a directory you name as `incus_conf`;
- a virtual-machine image (with `git`, `sh` and incus-agent) in the project, named as `image`.

## Sharing a host with other work (`admission_url`)

A host that also runs other work (a CI pool, its owner's jobs) can make zm ask before every VM. The run's
supervisor does every call, so the lease holds while zm is down, as the TTL does. JSON over HTTP:

| call | answer |
| --- | --- |
| `POST <url>/leases` `{"vcpu", "memory_mib", "holder", "ttl_s"}` | `201` (`200` for a repeat of the same `holder`) `{"lease", "expires_at"}`; `409` `{"reason", "retry_s"}`: no room now, and the acquire answers `unavailable` with both |
| `POST <url>/leases/{lease}/renew` `{"ttl_s"}` | `200` `{"expires_at"}`; `404` or `410`: the room is wanted back, and zm ends the VM as `reclaimed` (never billed) |
| `DELETE <url>/leases/{lease}` | `204`, or `404` when it is gone already |

`holder` is `zm:<target>/<run>`, unique per run. A dead supervisor stops renewing, so its room is free within one
TTL; a lease whose renewals fail until it lapses ends the VM too, so zm never runs on room the controller may have
handed on.

`zm host share` and `zm pool host` bind these keys on every size: `--admission-url URL`, `--admission-token-file PATH`
(a 0600 file, bound as an absolute path) and `--admission-ttl-s N` (120..3600, outlasting `boot_timeout_s` by 30 s),
checked as above before anything runs. Conformance runs with them too: its boxes are real VMs on the same room, so
they take leases like a renter's, and a controller with no room fails the run (nothing binds; try again later). A
re-run without `--admission-url` keeps the keys an earlier share bound on `<prefix>-<size>` (each flag given replaces
its own key); `--admission-url URL` takes the flags alone; `--admission-url none` removes them. Every run reads the
bindings it replaces, in the scope it binds in; when they cannot be read (the daemon, the list, or a view without their
config), share refuses unless `--admission-url none`, rather than drop the keys or leave a keyless one live. A run with no room says so (`the admission controller had no room now; try again later`).
When a run that adds the keys (the first switch-on too) leaves a size unbound, share unbinds that size's earlier
binding in the same scope when it lacks them, whose boxes the controller would not count (or says it is still live WITHOUT admission when it cannot). `zm host doctor`
prints one `admission:` line per controller, naming the bindings that ask it.

## A guarded uplink: a download cap and a cache (`vm=oci`)

A GitHub Actions job downloads hundreds of MB in its first minute at line rate (a toolchain, a restored
dependency cache, modules). On a machine whose owner guards the uplink (an admission controller that takes room
back when the host's inbound rate passes a limit), that ends the very box that runs the job. Two owner-side
settings, both off by default; neither needs anything of the image or the job:

- **A cap**: `potluck host --net-mbit N` caps what all boxes on Potluck's network `potluck-box` download together at
  N Mbit/s. It is a root step like the network's own (`--no-sudo` prints it): two `tc` lines
  in `potluck-box-net.service`, a token bucket (`tbf`) on the bridge `br-potluckbox` with a fair queue (`fq_codel`) under
  it. The bridge's transmit side is every packet routed to a box, so the cap holds whatever the box runs
  (runsc, Kata: the bridge is outside the guest) and whatever the job does; a box cannot opt out, as it could
  from a proxy. All boxes share the one rate, because an uplink guard counts the machine, not a box. Uploads are
  not capped, nor is traffic that never crosses the bridge (image pulls, a checkout the host fetches). `potluck
  doctor` prints the cap; a re-run without the flag keeps it; `--net-mbit 0` removes it. Pick N below your
  guard's limit less what the machine's other work downloads. It needs what the network needs (rootful Docker,
  systemd) and the kernel's `sch_tbf` and `sch_fq_codel`; on a kernel without them the unit fails, no box starts on
  the network, and `--net-mbit 0` undoes it. For a network of your own (`--oci-network`), add the
  same two lines for its bridge yourself:

  ```sh
  sudo tc qdisc replace dev <bridge> root handle 1: tbf rate 60mbit burst 256kb latency 50ms
  sudo tc qdisc replace dev <bridge> parent 1:1 handle 10: fq_codel
  ```

  [scripts/box-net-cap-check.sh](../../scripts/box-net-cap-check.sh) downloads a file in a container on the
  network and prints its rate beside the uplink's.
- **A cache**: when a volume named `potluck-cache` exists in the container CLI, every box gets it read-only at
  `/opt/potluck-cache` (no renter can name that volume; delete it and boxes get no mount). A GitHub runner box
  is started using two directories of it, when they are there: `tools/<tool>/<version>/<arch>` (with
  `<arch>.complete` beside it, as GitHub's hosted tool cache is laid out) is linked per version into the
  runner's tool directory, so `actions/setup-go` and its kin find a cached version and download nothing, and
  install one the cache lacks in the box as ever. Every box, runner or not, is created with
  `GOPROXY=file:///opt/potluck-cache/goproxy,https://proxy.golang.org,direct` unless its image sets `GOPROXY`
  (a run's own `GOPROXY` wins; `GOPRIVATE` modules go direct as always), so Go reads a module from the cache's
  `goproxy/` tree first. [scripts/box-cache-fill.sh](../../scripts/box-cache-fill.sh) adds toolchains
  (`--go 1.27.4`); the host fills `goproxy/` itself, only with modules from proxy.golang.org checked against
  sum.golang.org, from `potluck host shelf fill --go-sum go.sum` or from boxes' wants
  ([docs/shelf.md](../../docs/shelf.md)). What a job restores with `actions/cache` (setup-go's `cache: true`
  among them) still comes over the network, at the cap. Only a tool marked complete is used, so a fill may run
  while boxes do.
  While the volume exists, describe's `shelf` gives its path in the box, its bytes and its file count (never a
  name), measured at most every 10 minutes.
- **A package shelf** (`vm=oci`): the same volume also holds public npm tarballs, which a box links into its npm
  cache with `/opt/potluck-cache/bin/potluck-deps link .` so `npm ci` downloads nothing it finds there. The owner
  turns it on with `potluck host --shelf-gb N [--shelf-fill seed|boxes|off]` (it makes the volume and binds
  `shelf_max_gb`, and `shelf_fill` when given or already bound, on every oci size, which share the one volume), fills it with
  `potluck host shelf fill --npm-lock FILE`, and reads it with `potluck host shelf status`; those two run
  here as `potluck-driver-tart host shelf fill|status`, which read the binding's config on stdin. The keys are in
  the options table (`shelf_max_gb`, `shelf_keep_s`, `shelf_fill`, `shelf_fill_mbit`, `shelf_fill_delay_s`) and
  [docs/shelf.md](../../docs/shelf.md) covers the rest: a box reads the shelf and cannot write it, only the
  host writes it, and a box can tell that someone on the host recently used a given public package.

## Check

```sh
potluck-conformance potluck-driver-tart --remote --ref main --commit <older pushed commit with probe.sh> \
  --repo <remote> --script probe.sh --config image=zmrt-base-ubuntu --config remote=<remote> \
  --reclaim-config 'window={window}' --reclaim-config drain_deadline_s=0 --reclaim-in 90s
```

`--remote` is right: the guest shares no filesystem with the host, so the heartbeat checks are SKIPs.

Kata requires an owner resource profile and verified local engine; see [OCI host budgets](../../docs/oci-host-budget.md). Legacy Podman/nerdctl DNS and disk adapters do not constitute isolated-box readiness and are refused before acquisition.

Bounded native profiles advertise no outer named volumes, warm boxes or persistent restart. Outer measurement requires an independently admitted helper, not built yet; refused historical measurements remain unknown. Guest-internal Docker volumes are unaffected.
