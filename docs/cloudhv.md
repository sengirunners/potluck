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
named `box` with no sudo: every command runs as that account. The layer talks to the guest only through
`potluck-guest`, over the VM's vsock. No SSH server and no password or key are involved.

`scripts/cloudhv-image.sh` builds one from Ubuntu 24.04's cloud image (checked against Ubuntu's published
checksum). It boots the image once with no network card to install `potluck-guest`, so nothing is downloaded into
the image:

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
| `ch_console` | `off` | `on` writes the guest's serial console to `console.log` in the VM's directory. Off by default because a guest can fill the host's disk through it |

`network=nat` is refused: the layer has one network, the fenced one.

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
ARP for the tap's own address only, so a guest cannot list the host's other addresses.

A binding hands its links out in turn, so the link a VM just left is the last to be used again, and before a
link's next VM boots the kernel's connection records for that link's guest address are deleted: replies to what
one VM started never reach the next.

The fence is one nftables table, `potluck_cloudhv`, hooked before routing (priority raw), so no other program's
accept rule is consulted first. From any of the layer's taps it drops:

- every IPv6 packet (IPv6 is also off on the tap);
- a packet whose source is not an address routed to the tap it came in on, so a guest cannot use another address;
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

On one host (2026-11-21; Ubuntu 24.04, kernel 6.8, cloud-hypervisor v53.0, nftables 1.0.9, Docker installed):

- `potluck-conformance --kind box --remote` with `ch_nap_idle_s=2`, so VMs napped and woke during the suite, and
  with `--net-allow`, four `--net-forbid` addresses (the host's LAN address, its tailnet address, another bridge's
  address and the LAN's router), `--net-mail` and `--net-peers`: 100 checks passed and none failed (the secrets
  checks are skipped: a binding that naps serves none), and no tap was left on the host;
- the same suite with naps off and no net flags: 107 passed and none failed;
- a guest writing 45 files of 20 MB straight to its disk while it was napped part-way through: every file read
  back whole after the wake;
- 168 starts cancelled part-way, at varied moments, over two runs: no tap and no VM directory left;
- on a running VM's tap the helper refused both `up` and `down`, and the box's user could not bind the agent's
  port.

Skipped by the suite, and so not proven: secrets on a binding that naps, terminals, volumes, the shelf, warm
boxes, a checkout, reclaim, box authority, root, and mail over IPv6 (those addresses do not open from the host
either, so nothing was shown).

Not tested: arm64, a host without Docker, any other cloud-hypervisor version, and more than a few VMs at once
through the driver.

## What it does not do

- No terminal (`spawn` with `pty`), no `root` or `docker` capability, no volumes, no shelf.
- No secrets on a binding that naps (above).
- A napping box still holds its slot and its lease: napping frees memory, not a place in the binding.
- A box with a `spawn`ed process still running is not idle, so it never naps.
- Stopping a kept box (`persist`) that is napping discards its saved memory: it starts again from its disk.
- The driver does not limit a VM's host CPU or disk beyond the VM's own size.
