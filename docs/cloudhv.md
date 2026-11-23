# Linux boxes with cloud-hypervisor (`vm=cloudhv`)

A VM layer of the VM driver (`vmhost/cloudhv.go`; layers: [vm-layers.md](vm-layers.md)). Each box is a KVM virtual
machine run by a [cloud-hypervisor](https://www.cloudhypervisor.org/) process of the driver's own user. There is no
daemon and the driver does not run as root.

What this layer adds to the others is the nap. A box that has run no command for a while is saved to disk,
encrypted, and its hypervisor process exits, so a napping box holds no memory on the host. The next command
restores it, with the same programs still running, and then runs.

## What the host needs

- Linux on x86-64 with KVM, and the driver's user in the group that owns `/dev/kvm`;
- `cloud-hypervisor` and its UEFI firmware `CLOUDHV.fd` (both from the cloud-hypervisor project's releases). Tested
  with v53.0: the disk and restore options the layer passes are that version's;
- `qemu-img` (it makes each VM's disk), `nft`, `ip` and `conntrack` (the `conntrack` package), in `/usr/sbin`,
  `/sbin`, `/usr/bin` or `/bin`: the helper looks nowhere else;
- `sudo`, with one line that lets the driver's user run the driver's network helper and nothing else (below);
- for naps, `/dev/shm` mounted as a memory filesystem (tmpfs or ramfs, sticky, as Linux distributions mount it)
  with room for the memory of the VMs that nap or wake at one moment. Without one no VM naps (The nap, below). And
  what the driver's user has there must stay: systemd's logind removes a user's files from `/dev/shm` when the
  user's last session ends (`RemoveIPC`, on by default) unless the user is a system user or lingers
  (`loginctl enable-linger`). A VM whose snapshot is removed in the middle of a wake is lost;
- for naps, a driver started with a hard core limit that is not 0 (`ulimit -Hc`), or as root. `sudo` starts its
  command with a hard limit of 0 (Ubuntu 24.04), so a driver started through `sudo -u` as a user that is not
  root naps no VM.

## The network helper and its sudoers line

Making a tap device and loading a firewall table need root. The driver asks for them by running itself as
`potluck-driver-tart --cloudhv-net ...` through `sudo -n`. The owner allows that one command, as root, in a file
under `/etc/sudoers.d` (edit it with `visudo -f`), with the driver's real path and the driver's user:

```
potluck ALL=(root) NOPASSWD: /usr/local/bin/potluck-driver-tart --cloudhv-net *
```

The helper takes two forms and refuses everything else (`cloudhvNetArgs` in `vmhost/cloudhv_net.go`):

| arguments | what it does |
| --- | --- |
| `up <tap> <uid> <host address>` | loads the fence (below), makes the tap `<tap>` owned by `<uid>` and gives it its hardware address (Network, below), deletes the kernel's connection records for the link's guest address, gives the tap the host address, turns on IPv4 forwarding, and on a host with Docker adds two accepts for the layer's taps to Docker's `DOCKER-USER` chain |
| `down <tap>` | deletes the tap |

`<tap>` must be one of the layer's own names (`pch` and six hex digits), `<host address>` the first address of a
/30 inside `10.0.0.0/8`, and the name the one that goes with the address. A VM whose `up` call fails is not
started. The helper refuses:

- a tap a running VM's hypervisor holds open, for `up` and for `down` (a `down` first waits 10 seconds for it
  to be let go of): a running VM's link is never taken over or removed;
- a link whose guest address the host's main routing table already sends somewhere else (a LAN, a VPN's route):
  give the binding another `ch_net`. Routes a VPN keeps in a table of its own are not seen; the fence then drops
  the guest's packets, so such a VM has no network and reaches nothing it should not;
- a host with no `conntrack`.

What the helper still lets its caller do: route any free /30 inside `10.0.0.0/8` to a tap of any user, delete any
of the layer's taps that no process holds open (a napping VM's) or set such a tap's hardware address to its own,
reload the fence, and delete connection records for those addresses. Allow it only for the user the driver runs as.

The helper runs as root, so the driver cannot end it. A start that is cancelled can leave a helper still making
its tap. Each VM has a lock the helper holds until it ends, and whatever stops the VM waits up to 15 seconds for
that lock before it removes the tap (measured: 108 cancelled starts, no tap left). A tap can still be left: by a
helper stuck longer than that, or by a killed hypervisor that takes more than 10 seconds to let go of it. Nothing
removes such a tap, and its link is not handed out again: delete it with `ip link del <tap>`.

Keep the driver binary owned by root and not writable by the driver's user: whoever can replace the binary can run
anything as root through this line.

## An image

An image is a raw disk, `<ch_images>/<name>.raw`, that boots under `CLOUDHV.fd` and starts `potluck-guest`
(`cmd/potluck-guest`) at boot. It needs `sh`, `perl` and `ip`, and `git` for a checkout, and one ordinary account
named `box`, which the image gives no sudo: every command runs as that account. For `root=on` it also needs
`sudo`, and for `docker=on` a `docker.service` that systemd can start, not enabled, with `box` in the group that
may use its socket (Root and Docker, below). The layer talks to the guest only through `potluck-guest`, over the
VM's vsock. No SSH server and no password or key are involved.

`scripts/cloudhv-image.sh` builds one from Ubuntu 24.04's cloud image (checked against Ubuntu's published
checksum) and Docker's static release (the version and checksum pinned in the script). The script downloads both
and boots the image once with no network card to install `potluck-guest` and Docker, so nothing is downloaded from
inside the image:

```sh
CGO_ENABLED=0 go build -o potluck-guest ./cmd/potluck-guest
scripts/cloudhv-image.sh /var/lib/potluck/images/box.raw ./potluck-guest /usr/share/cloud-hypervisor/CLOUDHV.fd
```

With `agent` as a fifth argument, after the disk's size, the image also has what a coding agent runs:

```sh
scripts/cloudhv-image.sh /var/lib/potluck/images/agent.raw ./potluck-guest \
  /usr/share/cloud-hypervisor/CLOUDHV.fd 20G agent
```

That is what the `agent` template for Docker boxes adds (`docs/hosts.md`, "Templates"). Its Ubuntu packages, its
tools and its pinned versions are read from that template's file
(`cmd/potluck-driver-tart/images/agent/Dockerfile`), so a version or a tool changed there changes in both; how
each is installed is written in both places. With them the image has Node, Go, `gh`, Python's `pip` and `venv`, a
C and C++ compiler, and the Claude Code and Codex programs. The build needs Docker on the host for it, and an
account that may use Docker. The Ubuntu packages are downloaded in a container made from Ubuntu's own root
archive of the same cloud image, where apt chooses them against what that archive has; the image has all of that
and a little more, so a few of them are packages it has already. Node's packages are installed there too, and
all of it goes to the build boot on its seed disk. The image still never has a network card. The build boot
installs the packages with apt, which removes nothing for them, and the build fails unless they fit the image,
every tool is found in it, and the agent programs answer as `box`.

Everything is installed for all users under `/usr` and `/usr/local`, owned by root. So a box without `root=on`
runs the tools and installs into its own directories (a `venv`, a project's `node_modules`, Go's module cache),
and a global install (`npm install -g`, `apt-get install`) needs `root=on`. The agent programs stay at the pinned
versions until the image is rebuilt: without `root=on`, `box` cannot write where they are, and Claude Code's
updater is turned off (`/etc/claude-code/managed-settings.json`).

A box gets this image as any other: by its name, `agent`, when the binding has more than one image, or as the
binding's first. It is not what a template picks (`--template agent`): a template names one of Potluck's own
Docker images, which this layer does not run.

A build boot that does not set the image up, with or without `agent`, fails the build: one whose setup command
fails, one the hypervisor ends, one that takes more than fifteen minutes. The build then writes no image, and
leaves what that boot's console said beside where the image would be, as `<out>.console.log`.

A build that succeeds prints the pin to use. A box binding's `images` are pinned by digest (`<name>@sha256:<hex>`), and
the digest is the image file's own sha256, which the script writes beside it in `<name>.raw.sha256`. Hashing a 20 GB
file at every acquire is too slow, so the layer checks the pin against that file, and refuses an image whose
modification time is later than the digest file's. That catches an image rebuilt or edited without a new digest. It is
not a check against someone who can write the images directory: keep that directory writable only by the owner.

The driver never writes to an image: each VM's disk is a qcow2 overlay over it, holding only what the VM changed.
A VM records which file its image was (size and modification time) and is refused, at a start and at a wake,
once that file has been rebuilt or rewritten. A rebuilt image is for new VMs; a kept box made over the old one
must be made again. A VM made by a driver older than this check has no such record and is not checked.

`potluck-guest` in the image and the driver must be built from the same source: they agree on a port and a
protocol. Rebuild the image, and pin its new digest, when the driver is upgraded.

`potluck-guest` sets the guest's address itself. The host makes each VM's vsock context id from the guest's IPv4
address, and the guest reads it back, so one image serves every VM with nothing written into it per VM. The image
must not run a DHCP client or anything that waits for the network (the script turns those off).

## Config (the binding's driver keys)

Besides the driver's own keys (`cmd/potluck-driver-tart/README.md`); `cpu` and `memory` size each VM (2 and 4096
MiB when unset).

| key | default | meaning |
| --- | --- | --- |
| `vm` | | `cloudhv` |
| `ch_firmware` | required | absolute path of `CLOUDHV.fd` |
| `ch_images` | required | absolute path of the directory holding the images |
| `ch` | `cloud-hypervisor` on PATH | the cloud-hypervisor binary |
| `ch_qemu_img` | `qemu-img` on PATH | the qemu-img binary |
| `ch_dir` | `<state_dir>/cloudhv` | where each VM's directory is made: its disk, and its saved memory, encrypted, while it naps |
| `ch_net` | `10.213.0.0/16` | the private range each VM's link is cut from, a /30 each; inside `10.0.0.0/8`. Two bindings on one host need two ranges. Given one range they can pick the same link, and the second VM to start on it is not started |
| `ch_nap_idle_s` | `0` | nap a VM that has run no command for this many seconds; `0` never naps |
| `ch_nap_slots` | `0` | how many boxes the binding holds beyond `linux_slots`, napping (below). `0`: a napping box keeps its slot. Needs `ch_nap_idle_s` |
| `root` | `off` | `on`: the box's user has sudo with no password (capabilities `root` and `root-vm`). Box bindings only |
| `docker` | `off` | `on`: Docker runs in the box and its user can `docker run` (capability `docker`). Turns `root` on |
| `ch_console` | `off` | `on` keeps the end of what the guest writes to its serial console, in `console.log` in the VM's directory (see Logs) |
| `ch_disk_mb_s` | `0` | the MiB a second each VM may read and write on its disk (see Limits). `0`: no limit |
| `ch_disk_ops_s` | `0` | the disk requests a second each VM may make. `0`: no limit |
| `ch_net_mbit_s` | `0` | the megabits a second each VM may send, and as many it may receive. `0`: no limit |

`network=nat` is refused: the layer has one network, the fenced one.

## Root and Docker (`root=on`, `docker=on`)

So an agent can install packages and run containers. Both are off unless the binding says so.

- With `root=on` the box's user has `sudo` with no password. It is the guest's real root: it holds every
  capability of the guest's kernel, can load modules, change the guest's addresses and routes, and stop or replace
  `potluck-guest`.
- With `docker=on` (which turns `root` on) the image's Docker is started and the box's user can use it. It is
  ordinary rootful Docker with bridge networking, overlay storage, `docker build` and published ports inside the
  box. A kept box keeps its images, and a napping box's containers go on at the wake.
- The image carries no sudo rule and starts no Docker. `potluck-guest` writes the sudo rule
  (`/etc/sudoers.d/potluck-box`) and starts `docker.service`, waiting until Docker is up, before the first command
  of a box whose VM was made with them: that command is the boot's own, so a box that is ready has both. It
  removes any such rule when it starts. A box made without them has neither: its user is in the `docker` group,
  but there is no Docker socket to use and it cannot start one.
- A boot fails, saying why, on an image that cannot give them: one without `sudo` or `docker.service`, or one whose
  `potluck-guest` was built before `root=on`.

Why real root is safe here, when an oci box's root must lack `NET_ADMIN` and `NET_RAW`: an oci box shares a bridge
and a kernel boundary with its neighbours, and a cloudhv box does not. The box is a VM of its own on a link of its
own, and everything that fences it is on the host, outside the guest: the tap, the fence table (Network, below)
and the hypervisor. No root inside the guest can change those. The host takes nothing the guest says as true
about anything but the box itself.

The driver says so with capability `root-vm` beside `root`, and conformance's `root` group then proves the fence
from root's own shell instead of reading root's capability bits (`docs/hosts.md`, "Conformance group `root`"):
root cannot reach a forbidden address, also after it empties the guest's own packet filter and gives its link
another address and route; cannot reach another box after naming that box a neighbour on its own link; and
cannot open a connection from any address but the box's own, another box's included. It also checks that two
boxes do not share a kernel.

A VM is made with the binding's `root` and `docker` and keeps them. A kept box (`persist`) is refused at `start`
(`conflict`) under a binding whose `root` or `docker` now differ; its disk stays kept until it is released or its
time ends. A warm box made before such a change is ended, never handed out.

## Terminals

A box binding lists the `pty` capability. A `spawn` with `pty` runs its command on a terminal that `potluck-guest`
opens in the guest. The command leads its own session there, the terminal is its controlling one, and it belongs
to the box's user, as a login's does.

The driver's client for it (the driver run as `--cloudhv-tty`) stands on the terminal the process's holder owns
on the host. The holder makes that one raw before the client starts, so only the guest's terminal echoes, edits a
line and reads a Ctrl-C or a Ctrl-D, in what `spawn`'s `stdin` types first too. The client carries the host
terminal's size to the guest's at the start and at each resize, and an `input` that resizes and types reaches the
command in that order: the new size first.

- A terminal is a `spawn` in flight for as long as its command runs. The VM does not nap under it, and under
  `ch_nap_slots` it holds a slot.
- A `spawn` with `pty` wakes a napping box as any command does. When the box is not woken, the process ends with
  exit 255 and the reason in its output.
- When the command ends, the terminal is taken from whatever it left running on it: a later write to it fails.
- A timeout, or the client going away, kills the command and its process group. A shell with job control puts
  each job in a group of its own: those are hung up on, as when any terminal closes, and one that is in the
  background or ignores the hang-up goes on with no terminal.
- `potluck-guest` holds up to 2 MiB of what was typed and the command has not read. Past that, typing waits for
  the command, and the client going away is seen only once the command reads on or ends; a timeout and a
  `signal` still end it.
- An image whose `potluck-guest` was built before terminals runs nothing for a `pty`: the process ends with exit
  127, and its output says to rebuild the image.

## The nap

A VM naps as its binding says when the VM starts. Set `ch_nap_idle_s` to 0 and a VM already running still naps
until it is started again.

With `ch_nap_idle_s` set, each VM's monitor (one host process per VM, the driver run as `--cloudhv-vm`) watches for
the VM being idle: no `exec` or `spawn` command in flight (a terminal is a `spawn`). When it has been idle that long
the monitor:

1. asks the guest to hand back what it is not using (it syncs, drops its file cache and compacts free memory), so
   the saved memory is little more than what the guest's programs hold;
2. waits 3 seconds for the freed memory to reach the host, and gives up if a command arrived meanwhile;
3. has the guest write its files to disk once more, pauses the VM and has the hypervisor save its memory and
   device state into the host's memory;
4. encrypts that into one file in the VM's directory (`snap/sealed`), and removes the copy in the host's memory;
5. asks the hypervisor to shut down, and waits for it to exit.

The hypervisor is asked to shut down, never killed. It holds disk writes the guest has finished that are not yet
in the VM's disk file, and writes them out as it shuts down. Killed after the snapshot instead, it lost them: a
file the guest wrote just before the nap read back different after the wake (measured, 2026-11-20).

The next command asks the monitor to wake the VM first. The monitor decrypts the saved state into the host's
memory, starts a hypervisor from it, resumes it, tells the guest the time (a guest's clock stops while it naps)
and has it reseed its random pool, and deletes the saved state, on disk and in memory. Then the command runs. One
nap is restored once: two guests never start from the same memory.

A guest's memory holds its secrets (they are files in its memory filesystem), and the protocol keeps those off
every disk. So a nap's saved memory reaches the disk encrypted only, and a binding that naps serves secrets and
may have a `runner_dir` as one that does not:

- The hypervisor writes its snapshot to a directory of the driver's user alone under `/dev/shm`
  (`potluck-cloudhv-<uid>/<a name made from the VM's directory>`, mode 0700), which must be a memory filesystem.
  The snapshot is there for the moments a nap or a wake needs it, and is removed when the VM is stopped or
  destroyed too.
- The monitor encrypts it with AES-256-GCM under a key it makes for that nap and keeps in its own memory, nowhere
  else. The file on disk is as long as the memory the guest's programs hold, not as the guest's size. A changed,
  cut or reordered file does not decrypt, and its VM is lost.
- The key goes with the monitor. A napping VM whose monitor is gone (the host restarted, the monitor was killed)
  cannot be woken, and what is left of it on disk cannot be read.
- The monitor and the hypervisors it starts leave no core dump, whether the host writes core files or hands
  them to a crash handler: the monitor sets their core limit to 1 byte. On a host that starts the driver with a
  hard core limit of 0 it cannot, and says so in the VM's log. There the VM does not nap (a dump of the monitor
  would hold the nap's key), and a crash handler (a `core_pattern` that is a pipe) is still handed a crashing
  hypervisor's memory.
- This is what encrypted swap does with a box's memory, which `docs/hosts.md` counts as safe. The snapshot in the
  host's memory and the monitor's key can be paged out as any memory can: they are as safe as the host's swap,
  which a pool asks to be safe of every host it sends secrets to.
- A VM's monitor says in the VM's `state.json` that it encrypts what a nap saves. A VM that naps and whose
  monitor does not say so (one started by a driver that saved naps unencrypted keeps napping that way) is
  refused secrets (`forbidden`) until it is started again.
- Another user of the host can keep VMs from napping or waking, by taking the directory's name under `/dev/shm`
  or filling it. They cannot read a nap.

What a guest sees: its programs, files, memory and open sockets are as they were. A program left running in the
background by an `exec` (not a `spawn`) does not keep the VM awake: it stops at the nap and goes on at the wake.
Its clock jumps forward by the length of the nap. A TCP connection whose other end gave up during the nap is dead
when the guest wakes. Timers that were due during the nap fire at the wake.

A nap that fails before the VM is saved (the guest does not answer, the snapshot fails, `/dev/shm` is no memory
filesystem or has no room, the snapshot cannot be encrypted) leaves the VM awake when the hypervisor still answers,
with nothing of its memory saved; after three failures in a row the monitor tries no nap of that VM for ten
minutes. A wake whose saved state cannot be written to `/dev/shm` (it is full, or the directory there is not the
driver's user's) is refused as a wake with no slot is (below): the box naps on with nothing lost, the command
answers `unavailable`, and the caller may ask again. A VM is lost, and is ended and removed, when it can neither go
on nor be woken: its hypervisor did not shut down cleanly after the snapshot, it could not be resumed after a
failed snapshot, its saved state is damaged or gone at the wake, or its tap does not have its hardware address at
the wake. A command that asked for a lost VM answers exit 255.

### Napping boxes and slots

A napping box has no hypervisor process and holds no memory. With `ch_nap_slots=N` it holds no slot either:

- `linux_slots` is how many boxes are awake at once, and the binding holds up to `linux_slots` + N boxes in
  all. `describe`'s `slots.free` is how many an `acquire` would be given a box for now: the smaller of the
  slots no awake box holds and the boxes left of `linux_slots` + N.
- An `acquire` is admitted by that count, and answers `unavailable` when it is 0, as on any full host.
- A napping box takes its slot back before it wakes. Its monitor makes the driver's count, under the state
  directory's lock, and wakes the box when fewer boxes than `linux_slots` hold a slot. Otherwise it counts again
  until 10 seconds after the command asked. When no slot comes, the box naps on with nothing lost, and `exec`,
  `secrets` and a `spawn` with secrets answer `unavailable` with the words `no free linux slot` and the reason:
  nothing ran, and the caller may ask again. Commands that were waiting on the same wake are answered with it. A
  `spawn` without secrets has answered its `pid` by then, so its process ends with exit 255 and the same words
  in its output.
- A wake is refused the same way when the count cannot be made or the slot cannot be marked taken: the state
  directory is gone, its lock could not be taken in the wait (it is tried for a second at the least), its
  records cannot be listed, one record cannot be read, or the VM's `state.json` cannot be written. An `acquire`
  is refused when a record cannot be read. A file that is no record and is older than the host's last start
  (what an unclean shutdown can leave of a record just saved) is renamed `<name>.torn` by the driver's next
  sweep and kept seven days, and the count is made again. A newer one is left, and refuses until it is removed:
  `describe`'s `why` names it.
- After a refusal the driver sweeps before it answers, so a slot held by a box whose supervisor died is free
  for the next try.
- A `spawn` answered with a refusal's exit is not started again by the same `request_id`: the caller asks with
  a new one. `port` for a napping box with no slot to wake in answers `unavailable` within 8 seconds, without
  the words `no free linux slot`.
- A command that gives up while its box's wake waits for a slot takes none: the box naps on when a slot comes
  free, until the next command.
- No box is napped early to make room. A slot comes free when an awake box has been idle for `ch_nap_idle_s`
  and naps, or ends.

The count is of every live box in the binding's state directory, whichever binding made it. A box that never
naps holds its slot for as long as it lives: one a binding made without `ch_nap_slots`, or one of another
layer. A box being restored from a nap holds one too, and so do a stopped box until its hypervisor is gone
and a box whose start has not finished, napping or not.

A `stop` does not wake a napping box that gave its slot up. Its guest wrote its files to disk just before it
was paused for the nap, so the stop loses nothing the guest had written by then.

The binding's cores and memory limits (`host_cpu_limit`, `host_memory_limit_mb`, the lender's dials) count every
live box, napping or not: a nap ends whenever a command comes. A box's lease runs while it naps.

A box takes `linux_slots` as its binding has it when the box boots, and wakes by that number until it next
boots. `ch_nap_slots` cannot be set together with `warm`.

With `ch_nap_slots` unset (the default) a napping box counts as any live box does, and the wake of a box that
booted so is never refused for room.

Each VM's directory has `state.json` (awake or napping, since when, naps so far, total time awake and napping) and
`events.jsonl` (one line per nap and wake, with how long each step took and the bytes of memory saved, and with
`ch_nap_slots` one per wait for a slot, per refused wake and per wake dropped because its command had gone).

Measured on one host (cloud-hypervisor v53.0, Ubuntu 24.04 guest, 2 CPUs and 4096 MiB): boot to first command 2.7
to 3.1 s (2026-11-20). An idle guest (2026-11-22, the middle of its naps and wakes): a nap 0.4 s after the settle,
of which the snapshot 0.1 s and the encryption 0.2 s; a wake 0.3 s, of which the decryption 0.15 s and the restore
0.1 s; the saved state 370 to 460 MB on disk. A guest holding 1.5 GB more in memory: a nap 1.7 to 2.0 s, a wake 1.4
to 1.6 s, 1.7 GB on disk. An awake VM holds the memory its guest uses, not its full size: memory the guest frees
goes back to the host (free page reporting).

## Limits

A VM's size bounds its guest's CPUs and memory, and its image's size bounds its disk. How fast a VM uses its disk
and its link has no bound unless the binding sets one: on one host a guest wrote its disk at 1.3 to 1.8 GB a
second and put 5 to 9 Gbit a second on its link, and its hypervisor, which does that work on threads of its own
beside the guest's CPUs, took 2.2 to 2.5 host cores for a guest of 2 CPUs (measured, 2026-11-22). Three keys set
rates, each for every VM of the binding by itself:

- `ch_disk_mb_s`: the MiB a second a VM reads and writes on its disk, both counted together.
- `ch_disk_ops_s`: the disk requests a second, of any kind and size. A guest making many small requests meets
  this one first, a guest making large ones the other.
- `ch_net_mbit_s`: the megabits a second a VM sends, and as many again that it receives.

Set the two disk keys together. `ch_disk_mb_s` counts the bytes of reads and writes only: a flush, a discard and
a write of zeroes cost one request and no bytes, however much of the disk they cover, and only `ch_disk_ops_s`
holds a guest that makes many of them.

The hypervisor keeps the rates, in the VM's own process (its rate limiter): nothing is set on the host and no
root is needed. Each rate is a bucket that holds one second's worth and fills at the rate. So a VM that was idle
does one second's worth at once, and is at its rate from then on. A disk request larger than the bucket is let
through, and the next waits until it is paid for. On the link a frame is counted once it has passed: the frame
that empties the bucket passes whole, one in each tenth of a second the VM is held. A guest whose frames are 64
KiB (TCP's can be) so passes up to 5 Mbit a second more than its rate (from the hypervisor's source, not
measured). The guest is told nothing: it has a slower disk and a slower link, and no request fails.

A disk rate makes four things take longer:

- A boot, which reads its disk under the rates too: about 240 MiB in 4,800 requests, for a `docker=on` box of the
  image `scripts/cloudhv-image.sh` builds. Measured on a host busy with other work (a load of 20 on 32
  threads), from the acquire to a box that is ready: 4.8 s with no rates,
  5.2 s at 100 MiB a second, 7 s at 50, 15 s at 20 and 62 s at 5; at 1 MiB a second the box was not ready within
  its boot timeout. At 2,000 requests a second 4.8 s, at 500 15 s. At about 3 MiB or 50 requests a second that
  boot reaches the longest boot timeout (100 s), and below them no box of the binding is ever ready.
- A nap, which has the guest sync twice: first waited for 30 s, then again just before the VM is saved, waited
  for 10 s. A guest holding more unwritten than its rate writes in that time does not nap at that try, and is
  tried again after another idle period, as after any nap that failed; the guest goes on writing meanwhile.
- The first commands after a wake: a nap drops the guest's file cache, so they read their programs from the disk
  again.
- A kept box's stop, which gives the guest's sync 10 to 20 s and then cuts the guest off, whatever the rate: 10
  s from the VM's monitor, and before them 10 s from the driver when the stop was asked for or the box's lease
  ended (not at a reclaim, nor after its supervisor died). What the guest synced is on its disk. What it wrote
  and the sync did not write in that time is lost, as at a power cut. A Linux guest holds up to a fifth of its
  memory written and not yet on its disk, so a box that is to be kept needs a `ch_disk_mb_s` that writes a fifth
  of its memory within 10 s: a fiftieth of the VM's `memory` or more (82 for 4096 MiB).

A VM takes the rates its binding has when it boots, and keeps them over its naps: a wake starts the VM as it was
saved, its buckets full. A kept box takes its binding's rates at each start. When a binding's rates change, a box
that is running keeps the rates of its boot, and a warm box that booted under the old ones is ended, not handed
out.

The rates are no limit on a VM's host CPU (see "What it does not do"), but they take away most of what a guest
can make its hypervisor do beyond the guest's own CPUs: held to 20 MiB, 2,000 requests and 10 Mbit a second, the
busy guest above took 1.9 host cores.

## Logs

A VM's directory holds `hypervisor.log` (what its hypervisor says), `events.jsonl` (the record of its naps and
wakes) and, with `ch_console=on`, `console.log` (what the guest writes to its serial console). A guest can make
each of them grow: root in it writes to the console as much as it likes, and any guest naps and wakes as often as
commands come. So each log is at most two files, the newer under its name and the one before it under that name
with `.1` added, and neither is larger than 512 KiB. When the newer is full it takes the older's place and a new
one is begun. A log so takes 1 MiB of disk at the most, and holds the end of what was written to it.

The hypervisor writes no log file itself: what it says and the console come to the VM's monitor, which writes
the files. What the disk does not take is dropped, and the hypervisor and its guest go on. `hypervisor.log` and
`events.jsonl` stay with a kept box's disk over its stops; `console.log` is removed at each stop, and can be
read only while its box runs. When a hypervisor process exits by itself, the last 600 bytes that process said
go to the run's log.

## Network

Each VM has one tap device on the host: a point-to-point /30 with the host at one end and the guest at the other.
No bridge joins two VMs. The host routes and masquerades what the fence lets through. On the tap the host answers
ARP for the tap's own address only, so a guest cannot list the host's other addresses, and only to a sender
whose address is on that link. A guest's root can send ARP from any address; on a host that leaves `arp_accept`
and `proxy_arp` off, as Linux does, only the link's own addresses take a place in the host's neighbour table.

The host's end of a link has a hardware address made from the tap's name: `02:70:63` and the last three bytes of
the link's guest address. The helper sets it and the hypervisor is told it. A hypervisor told none takes the
address the tap has when the VM starts, and at a wake it may not set that address again. On a host with udev a
new tap's address is rewritten a moment after the tap is made, so a VM started in that moment could not be
woken. udev leaves alone an address a program has set. The helper gives udev its turn with the new tap first
(`udevadm settle --timeout=5`, where the host has it), sets the address, and reads it back as its last step. A
VM is not started, and not woken, on a tap that has another address: the monitor says which address the tap has.

A binding hands its links out in turn, so the link a VM just left is the last to be used again, and before a
link's next VM boots the kernel's connection records for that link's guest address are deleted: replies to what
one VM started never reach the next.

The fence is one nftables table, `potluck_cloudhv`, hooked before routing (priority raw), so no other program's
accept rule is consulted first. From any of the layer's taps it drops:

- every IPv6 packet (IPv6 is also off on the tap);
- a packet whose source is not the guest's own address (an address routed to the tap it came in on, and the
  guest's end of that link), so a guest cannot use another address, whatever its root gives its link;
- any address of the host itself, and broadcast;
- the ranges in [network.md](network.md): private, shared (tailnets), loopback, link-local (cloud metadata),
  benchmark and multicast. That covers the LAN and every other VM's link;
- outbound mail: tcp 25, 465, 587 and 2525.

Nothing is forwarded to a VM except replies to what it started.

The table is loaded whole, in one transaction, before every boot, so a fence that was flushed is back before the
next guest runs. It is not watched between boots: an owner who flushes nftables while VMs run has opened them
until the next boot. The guest's resolvers are public ones (`potluck-guest -dns`, default `1.1.1.1,8.8.8.8`); the
host's own resolver is behind the fence.

## What is proven, and where

On one host (2026-11-22 and 23; Ubuntu 24.04, kernel 6.8, cloud-hypervisor v53.0, nftables 1.0.9, Docker installed):

- `potluck-conformance --kind box --remote` four ways, each with no check failed and no tap left on the host:

  | binding | naps | net flags | passed |
  | --- | --- | --- | --- |
  | `docker=on`, `ch_nap_slots=8`, `ch_console=on`, the three rates | `ch_nap_idle_s=2`: VMs napped and woke during the suite | yes | 136 |
  | no root | `ch_nap_idle_s=2`: VMs napped and woke during the suite | yes | 126 |
  | `docker=on` | off | yes | 136 |
  | no root, the three rates | off | none | 112 |

  The net flags are `--net-allow`, four `--net-forbid` addresses (the host's LAN address, its tailnet address,
  another bridge's address and the LAN's router), `--net-mail` and `--net-peers`. The three rates are
  `ch_disk_mb_s=100`, `ch_disk_ops_s=3000` and `ch_net_mbit_s=100`. The secrets checks ran in all four. In the
  two runs that napped, the only file a VM's `snap` directory ever held was `sealed` (looked at ten times a
  second);
- a napping box's memory is on no disk in the clear. A box secret, an `exec`'s secret and a value made in the
  guest's memory were looked for as root in every file of the state directory and of `/dev/shm` while the box
  napped: in none, for an idle guest and for one holding 1.5 GB. The same search finds the guest's value in what
  a driver that does not encrypt saves. After each wake the guest held its value and its secret, six naps and
  wakes in a row, and the 1.5 GB read back whole. After the release nothing of the VM was left in `/dev/shm`;
- a terminal on a `docker=on` box with `ch_nap_idle_s=2`, driven by hand through the driver (33 checks, all
  passed): a `spawn` with `pty` woke the napping box, and its command ran as the box's user, leading its session
  on a terminal of the size asked that was that user's; `bash -i` had job control, a Ctrl-C ended its foreground
  job and the shell went on, a resize reached it with typing and with none, and `sudo` and Docker worked from it;
  the box stayed awake past its idle time while the terminal was open and napped once it closed; a timeout (124)
  and a `signal` KILL (137) left nothing of the command in the guest; 203,700 bytes typed and 20,000 lines shown
  arrived whole; what `spawn`'s `stdin` typed first for a napping box was shown once, by the guest's terminal,
  and a Ctrl-D in it ended the command's input; a process a command left on its terminal did not hold the
  answer, and its later write to the terminal failed;
- with a directory of a disk mounted over `/dev/shm`, an idle VM did not nap: it stayed awake, nothing was saved,
  its monitor said why, and the secrets were in no file of the state directory. With the driver started through
  `sudo -u` (a hard core limit of 0) it did not nap either, and its monitor said why;
- a hypervisor killed with SIGQUIT on a host whose crash handler is a pipe (apport): the kernel said
  `RLIMIT_CORE set to 1` and `Aborting core`, and the handler was handed nothing. Under a driver that leaves the
  limit at 0 the handler was handed the core;
- a tap keeps its own address and its VM wakes: with udev made to take a second longer over each new tap, 24
  boxes of 24 were started, napped and woken. A driver that leaves the tap's address alone lost all 24 at the
  wake: the hypervisor held another address than the tap had by then and exited at the restore
  (`TapSetMac ... Operation not permitted`). With udev as it is and the host at a load of 40, 150 boxes of 150
  woke, each tap with its own address;
- a hypervisor that exits during a restore leaves the end of its log in the run's log;
- the logs of a `docker=on` box with `ch_console=on` and `ch_nap_idle_s=2` (13 checks, all passed): the guest's
  boot and its login prompt were in `console.log`; root in the guest then wrote 3 MB to the serial console, and
  the console was two files of 1 MiB together at the most, holding the end of what was written in whole lines
  and not its start; after a nap and a wake what the guest wrote to the console was still kept, `events.jsonl`
  had the nap and the wake, and the guest answered;
- in the `docker=on` runs that passed every check the `root` group makes of a whole-VM Linux box passed, and
  Docker ran a container; a `docker=on` box was ready about 3 s after its acquire, with Docker already answering;
- the fence without its source rule fails the `root` group: root sent from its link's first address and the
  allowed address opened. With the rule it does not;
- root in a guest sent ARP from 1,200 made-up sender addresses: the host kept one neighbour on the tap, the
  guest's own. Without `arp_filter` on the tap it kept every one;
- a guest writing 45 files of 20 MB straight to its disk while it was napped part-way through: every file read
  back whole after the wake;
- 168 starts cancelled part-way, at varied moments, over two runs: no tap and no VM directory left;
- on a running VM's tap the helper refused both `up` and `down`, and the box's user could not bind the agent's
  port;
- the rates, on a `docker=on` box with `ch_disk_mb_s=20`, `ch_disk_ops_s=2000`, `ch_net_mbit_s=10` and
  `ch_nap_idle_s=3`, driven by hand through the driver (21 checks, all passed): the guest wrote its disk at 20.0
  MiB a second and read it at 19.9, and reading and writing at once at 20.2 together; it made 2,050 writes of 4
  KiB a second; a write through its cache, synced, 21.7 MiB a second; it sent 11.1 Mbit a second and received
  9.7; computing, writing and sending all it could, it took 1.9 host cores. After a nap and a wake the same, each
  within a tenth. A binding with `ch_disk_mb_s=100` alone held its guest's disk to 107 MiB a second and left its
  link as it was (7.2 Gbit a second). With no rates the same guest wrote 1.3 GB a second;
- a `docker=on` box napped while its disk and its link were held at their rates (`ch_disk_mb_s=20`,
  `ch_disk_ops_s=500`, `ch_net_mbit_s=10`, `ch_nap_idle_s=3`; 30 checks, all passed): with a reader, a writer and
  a sender left running in the guest, each all it could, the box napped six times. After every wake the three
  went on, a new read and a new write were answered, a new connection out of the box opened, and the sender was
  at 10.0 to 10.7 Mbit a second;
- a kept `docker=on` box of 4096 MiB stopped a quarter of a minute after its first boot, its guest holding
  about 450 MB of a 600 MiB file written and not synced. Held to 100 MiB a second: the stop took 5 s, and after
  the start the file read back whole. Held to 20 MiB a second: the stop took 20 s and the box did not start
  again, so nothing was read through it. On a copy of its disk a file the guest had synced before the stop was
  whole, and Docker's build database, written at that first boot and not synced, was damaged: that Docker does
  not start without it;
- 32 `docker=on` boxes of 2 CPUs and 4096 MiB at once on a host of 32 threads that other work kept at a load of
  20, each held to 100 MiB, 3,000 requests and 100 Mbit a second, with `ch_nap_idle_s=20`, driven by hand through
  the driver (7 checks, all passed). Started all at once, each was ready 18 to 20 s after its acquire (16 started
  8 at a time: 5.3 to 5.5 s each, 24 at once: 10 to 11 s). A command in each at once answered within 0.7 s. Idle
  and awake, a box's hypervisor held 0.65 GB of the host's memory and its monitor 6 MB. Every box computing on
  both its CPUs at once, 64 guest CPUs in all, a job of 4 to 5 s took 8 to 28 s. Every box writing 600 MiB at
  once took 5 to 12 s over the runs, never less than the 5 s its rate allows. Left alone, all 32 napped within 35
  s, a nap taking 6 to 10 s; napping, a box held no hypervisor, 21 MB for its monitor, nothing in `/dev/shm`, and
  460 to 560 MB of disk.
  A command for each at once woke all 32, and each answered within 6 s. After the releases nothing was left;
- `ch_nap_slots=4` with `linux_slots=2` and `ch_nap_idle_s=2`, driven by hand through the driver (35 checks, all
  passed): six boxes held where two run at once and a seventh refused; a command for a napping box with both
  slots taken refused after 10 s with the box napping on, and run once a slot was free, in the same guest; six
  commands at once to six napping boxes, none lost (each ran or was refused); six at once to one napping box
  with no slot, all refused together; a box of a second binding without naps in the same state directory held
  its slot throughout. The host was looked at ten times a second: never more than two hypervisor processes;
- the agent image (`scripts/cloudhv-image.sh` with `agent`): built in four to five minutes, the plain image in three,
  and 3.5 GB of the host's disk against the plain image's 2.0. Boxes of it, driven by hand through the driver (41
  checks, all passed): a box without root was ready 3 s after its acquire, its home held only the account's own three
  files, and as `box` every tool ran; a C, a C++ and a Go program were built and run, a `venv` was made and `pip`
  installed into it, `npm` installed a package, `git` cloned over https, and `claude` and `codex` answered, and said
  they had no login when asked to work. Claude Code reported its updater off and nothing wrong with how it is installed.
  The box napped and woke. A `root=on`, `docker=on` box installed with `sudo npm install -g` and with `apt-get`, ran a
  container and built an image. The conformance suite on boxes of this image, with the net flags: 136 passed and none
  failed with `docker=on`, `ch_nap_slots=8`, `ch_console=on` and naps, as on the plain image built beside it; 126 and
  none with no root and naps off. A build with a setup command that fails, and one whose image lacks a tool, each
  failed, wrote no image and left the boot's console.

Skipped by the suite, and so not proven: volumes, the shelf, warm boxes, a checkout, reclaim, box
authority, and mail over IPv6 (those addresses do not open from the host either, so nothing was shown).

Not tested: arm64, a host without Docker, any other cloud-hypervisor version, and more than 32 VMs at once
through the driver.

## What it does not do

- No volumes, no shelf.
- A nap and a wake each hold the VM's saved memory in the host's memory for a moment, beside what its hypervisor
  holds: as much as the guest's programs hold. The driver does not count it against any limit, and does not look
  at the host's free memory first: on a host with less than that free, the kernel ends a process to make room.
- A wake takes longer the more the guest's programs hold (about 0.7 s for each GB, measured), and the command
  that asked waits 30 s for it: a guest holding some tens of GB can be answered exit 255 while its wake goes on.
- A napping box still holds its lease, and counts against the binding's cores and memory limits. It holds its
  slot too unless the binding has `ch_nap_slots`.
- No idle box is napped early to make room for a box that is waking or starting.
- A box with a `spawn`ed process still running is not idle, so it never naps.
- Stopping a kept box (`persist`) that is napping discards its saved memory: it starts again from its disk.
- A stop does not shut the guest's system down: the guest syncs, and is cut off. A guest still writing then, or
  whose sync takes more than the 10 to 20 s it is given (Limits), loses its last writes as at a power cut. Then
  a `docker=on` box whose Docker lost files it needs does not start again, and its disk stays kept and out of
  reach.
- A VM's host CPU has no limit of its own. It is the guest's CPUs and the hypervisor's own threads for the VM's
  devices. With rates set a guest can make the disk's and the link's threads do little with reads, writes and
  frames (Limits); nothing holds the rest, such as how much one discard covers, what a command prints, and the
  console. VMs with more CPUs together than the host has threads share them as the host's scheduler decides, and
  nothing gives one VM a share before another.
- The rates are each VM's, not the binding's or the host's: N VMs may together use N times a rate.
