# Linux boxes with cloud-hypervisor (`vm=cloudhv`)

A VM layer of the VM driver (`vmhost/cloudhv.go`; layers: [vm-layers.md](vm-layers.md)). Each box is a KVM virtual
machine run by a [cloud-hypervisor](https://www.cloudhypervisor.org/) process of the driver's own user. There is no
daemon and the driver does not run as root.

What this layer adds to the others is the nap. A box that has run no command for a while is saved to disk and its
hypervisor process exits, so a napping box holds no memory on the host. The next command restores it, with the same
programs still running, and then runs.

## What the host needs

- Linux on x86-64 with KVM, and the driver's user in the group that owns `/dev/kvm`;
- `cloud-hypervisor` and its UEFI firmware `CLOUDHV.fd` (both from the cloud-hypervisor project's releases). Tested
  with v53.0: the disk and restore options the layer passes are that version's;
- `qemu-img` (it makes each VM's disk), `nft`, `ip` and `conntrack` (the `conntrack` package), in `/usr/sbin`,
  `/sbin`, `/usr/bin` or `/bin`: the helper looks nowhere else;
- `sudo`, with one line that lets the driver's user run the driver's network helper and nothing else (below).

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
| `up <tap> <uid> <host address>` | loads the fence (below), makes the tap `<tap>` owned by `<uid>`, deletes the kernel's connection records for the link's guest address, gives the tap the address, turns on IPv4 forwarding, and on a host with Docker adds two accepts for the layer's taps to Docker's `DOCKER-USER` chain |
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
of the layer's taps that no process holds open (a napping VM's), reload the fence, and delete connection records
for those addresses. Allow it only for the user the driver runs as.

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

It prints the pin to use. A box binding's `images` are pinned by digest (`<name>@sha256:<hex>`), and the digest is
the image file's own sha256, which the script writes beside it in `<name>.raw.sha256`. Hashing a 20 GB file at
every acquire is too slow, so the layer checks the pin against that file, and refuses an image whose modification
time is later than the digest file's. That catches an image rebuilt or edited without a new digest. It is not a
check against someone who can write the images directory: keep that directory writable only by the owner.

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
| `ch_dir` | `<state_dir>/cloudhv` | where each VM's directory is made: its disk, and its saved memory while it naps |
| `ch_net` | `10.213.0.0/16` | the private range each VM's link is cut from, a /30 each; inside `10.0.0.0/8`. Two bindings on one host need two ranges. Given one range they can pick the same link, and the second VM to start on it is not started |
| `ch_nap_idle_s` | `0` | nap a VM that has run no command for this many seconds; `0` never naps |
| `root` | `off` | `on`: the box's user has sudo with no password (capabilities `root` and `root-vm`). Box bindings only |
| `docker` | `off` | `on`: Docker runs in the box and its user can `docker run` (capability `docker`). Turns `root` on |
| `ch_console` | `off` | `on` writes the guest's serial console to `console.log` in the VM's directory. Off by default because a guest can fill the host's disk through it |

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

## The nap

A binding with `ch_nap_idle_s` set serves no secrets: it lists neither the `secrets` nor the `box-secrets`
capability, refuses both, and cannot have a `runner_dir` (a runner's registration is a credential). A nap writes
the whole of a guest's memory to a file on the host, and secrets are files in the guest's memory, which the
protocol says never reach a disk. The saved file is not encrypted. Use naps for boxes that hold nothing secret in
memory, or leave them off.

A VM naps as its binding says when the VM starts. Set `ch_nap_idle_s` to 0 and a VM already running still naps,
and is refused secrets, until it is started again.

With `ch_nap_idle_s` set, each VM's monitor (one host process per VM, the driver run as `--cloudhv-vm`) watches for
the VM being idle: no `exec` or `spawn` command in flight. When it has been idle that long the monitor:

1. asks the guest to hand back what it is not using (it syncs, drops its file cache and compacts free memory), so
   the saved memory is little more than what the guest's programs hold;
2. waits 3 seconds for the freed memory to reach the host, and gives up if a command arrived meanwhile;
3. pauses the VM, saves its memory and device state into the VM's directory, and asks the hypervisor to shut
   down, and waits for it to exit.

The hypervisor is asked to shut down, never killed. It holds disk writes the guest has finished that are not yet
in the VM's disk file, and writes them out as it shuts down. Killed after the snapshot instead, it lost them: a
file the guest wrote just before the nap read back different after the wake (measured, 2026-11-20).

The next command asks the monitor to wake the VM first. The monitor starts a hypervisor from the saved state,
resumes it, tells the guest the time (a guest's clock stops while it naps) and has it reseed its random pool, and
deletes the saved state. Then the command runs. One nap is restored once: two guests never start from the same
memory.

What a guest sees: its programs, files, memory and open sockets are as they were. A program left running in the
background by an `exec` (not a `spawn`) does not keep the VM awake: it stops at the nap and goes on at the wake.
Its clock jumps forward by the length of the nap. A TCP connection whose other end gave up during the nap is dead
when the guest wakes. Timers that were due during the nap fire at the wake.

A nap that fails before the VM is saved (the guest does not answer, the snapshot fails) leaves the VM awake when
the hypervisor still answers; after three failures in a row the monitor stops trying for that VM. A VM is lost, and
is ended and removed, when it can neither go on nor be woken: its hypervisor did not shut down cleanly after the
snapshot, it could not be resumed after a failed snapshot, or its saved state is damaged or gone at the wake. A
command that asked for a lost VM answers exit 255.

Each VM's directory has `state.json` (awake or napping, since when, naps so far, total time awake and napping) and
`events.jsonl` (one line per nap and wake, with how long each step took and the snapshot's size).

Measured on one host (2026-11-20, cloud-hypervisor v53.0, Ubuntu 24.04 guest, 2 CPUs and 4096 MiB, idle): boot to
first command 2.7 to 3.1 s; a nap 0.2 to 0.3 s after the settle (0.5 s with the disk busy); a wake 0.2 s; the
saved state 450 to 460 MB on disk. An awake VM
holds the memory its guest uses, not its full size: memory the guest frees goes back to the host (free page
reporting).

## Network

Each VM has one tap device on the host: a point-to-point /30 with the host at one end and the guest at the other.
No bridge joins two VMs. The host routes and masquerades what the fence lets through. On the tap the host answers
ARP for the tap's own address only, so a guest cannot list the host's other addresses, and only to a sender
whose address is on that link. A guest's root can send ARP from any address; on a host that leaves `arp_accept`
and `proxy_arp` off, as Linux does, only the link's own addresses take a place in the host's neighbour table.

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

On one host (2026-11-22; Ubuntu 24.04, kernel 6.8, cloud-hypervisor v53.0, nftables 1.0.9, Docker installed):

- `potluck-conformance --kind box --remote` four ways, each with no check failed and no tap left on the host:

  | binding | naps | net flags | passed |
  | --- | --- | --- | --- |
  | `docker=on` | `ch_nap_idle_s=2`: VMs napped and woke during the suite | yes | 109 |
  | no root | `ch_nap_idle_s=2` | yes | 100 |
  | `docker=on` | off | yes | 131 |
  | no root | off | none | 107 |

  The net flags are `--net-allow`, four `--net-forbid` addresses (the host's LAN address, its tailnet address,
  another bridge's address and the LAN's router), `--net-mail` and `--net-peers`. A binding that naps serves no
  secrets, so those checks are skipped in its runs;
- the `docker=on` run with naps was made eleven times. Ten passed whole. In one, a VM's hypervisor exited while
  the VM was being restored from a nap: its box was lost, its commands said so, and four checks of the `root`
  group failed. The cause is not known, and twelve more runs of the `root` group alone did not show it again.
  A hypervisor that exits during a restore now leaves the end of its log in the run's log;
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
  port.

Skipped by the suite, and so not proven: secrets on a binding that naps, terminals, volumes, the shelf, warm
boxes, a checkout, reclaim, box authority, and mail over IPv6 (those addresses do not open from the host
either, so nothing was shown).

Not tested: arm64, a host without Docker, any other cloud-hypervisor version, and more than a few VMs at once
through the driver.

## What it does not do

- No terminal (`spawn` with `pty`), no volumes, no shelf.
- No secrets on a binding that naps (above).
- A napping box still holds its slot and its lease: napping frees memory, not a place in the binding.
- A box with a `spawn`ed process still running is not idle, so it never naps.
- Stopping a kept box (`persist`) that is napping discards its saved memory: it starts again from its disk.
- The driver does not limit a VM's host CPU or disk beyond the VM's own size.
