# Docker service containers

Kata `docker=on` bindings request bridge networking in the image's rootless
Docker daemon. gVisor retains its no-bridge mode and exact `runsc-root`
configuration. No runtime gets added outer capabilities or host devices.
The existing `root` and `net` conformance groups remain mandatory.

This is implementation prepared for runtime acceptance, not a claim that an
existing producer can already serve it. No live producer was changed.

## Producer setup for review

Use an idle, dedicated Linux test producer with nested virtualization or bare
metal `/dev/kvm`. Install Kata using its supported distribution and register
its actual runtime with Docker (the existing Potluck Kata installation path).
Do not replace the runtime used by other workloads or restart a shared daemon.
Potluck does not install Kata automatically.

Build and publish the updated `cmd/potluck-driver-tart/images/linux` image via
the existing box image workflow. Select its immutable digest explicitly on the
test binding before changing the default image pin. Keep Docker on the current
image disabled for this acceptance run.

Before sharing a binding, inspect a disposable guest under that exact runtime:

- `/dev/net/tun` is a usable guest character device; the Kata guest kernel has
  TUN/TAP, network namespaces, user namespaces, veth, bridge and netfilter/NAT.
- The guest allows unprivileged user namespaces and the image's `newuidmap` and
  `newgidmap` work with the runner's subordinate UID/GID ranges.
- `iptables` and `ip6tables` work inside RootlessKit's network namespace.
- Root in the outer box still lacks `NET_ADMIN`, `NET_RAW` and `MKNOD`.

The driver supplies no host `/dev/net/tun` device. If the Kata configuration
cannot supply a guest-local tun device, this mode is unavailable on that host.
Do not grant outer `MKNOD`, add `--privileged`, disable seccomp, or expose host
networking as a workaround. A usable guest tun configuration is an unresolved
runtime prerequisite until tested; the upstream Kata rootless-VMM instructions
are about the host VMM's identity, not proof of nested Docker support.

## The guest's `/proc`, and the guard that makes it safe

Nested containers need a fresh procfs, and the kernel refuses one inside
RootlessKit's user namespace while any masked or read-only path covers the
box's `/proc` (those mounts are locked there). A Kata `docker=on` box is
therefore made with `--security-opt systempaths=unconfined`. Nothing else is
loosened: no capability, no `--privileged`, seccomp and AppArmor as before,
and the container's `/sys` stays read-only (to the box's processes; root that
takes the guest kernel is not bound by it). That is why nested `docker run --privileged`
is unavailable in such a box: it wants a writable `/sys`. A nested container may
still take every capability (`--cap-add ALL`) inside its own namespaces.

With its `/proc` unmasked, root in the box can reconfigure its own guest
kernel (`kernel.core_pattern` runs a helper as the guest's full root), and with
it send any frame from the box's interface. The VM boundary, the host-side
egress filter and the host-side cgroup ceilings stay outside the guest; what
moved is the shared bridge. Measured on Kata: root in such a box, through
`core_pattern`, took a neighbour's address at the host (the host's ARP entry
moved to its MAC, and the neighbour lost its traffic).

So the fence moves to the host. `potluck-box-guard.service` (hostprobe
`boxguard.go`, its script `boxguard.sh`) keeps an nftables bridge table on
`br-potluckbox` (on a machine set up under zm: `zm-box-guard.service` on
`br-zmbox`):

- every frame from a box port must carry the MAC and IPv4 Docker gave that box
  (ARP: its Ethernet source, sender MAC and sender IPv4 too);
- no frame is forwarded from one box port to another;
- every other ethertype (IPv6, VLAN tags, the rest) is dropped;
- link-local frames (`01:80:c2:00:00:0x`) skip the bridge's prerouting: the
  bridge learns no address from them (`no_linklocal_learn 1`, which doctor
  checks; LLDP would otherwise let a box claim a neighbour's MAC in the
  forwarding table), those its input hook sees meet the same rules there, and
  nothing reaches the host's own IP or ARP stack on any port of the bridge,
  admitted or not (the kernel hands those frames up on the port's device, not
  on the bridge, past every rule keyed on the bridge).

The allowed (port, MAC, IPv4) set comes from Docker's record of the network,
never from a box, and is rebuilt whenever a container joins or leaves. A box
not yet in it sends nothing: the guard fails closed. `potluck host share --docker`
on Kata installs it with its other root steps; the driver makes a Kata
`docker=on` box only with no network, or on a Potluck network while its guard unit
is active, never on an owner's network that no guard watches (share names the
machine's own Potluck network, whose guard it reads and installs). It checks again
before every boot of a Kata box, from the container's own record (its
`systempaths` option and its networks), so a box made while the guard ran
does not start after the guard has stopped. The unit is active only once
its tables and set are loaded (`ExecStartPre`), it re-applies the tables
every 15 s, so a flushed ruleset comes back within one round, and systemd
restarts it however often it fails. When Docker does not answer for one box, a
round adds the boxes it found and removes none.

The guard covers every box on that bridge, not only Kata docker ones: a box
whose port is not yet in the set sends nothing, whatever its runtime. Its
tables stay in the kernel when the unit stops (the set it last wrote still
admits only the boxes that were there), and doctor warns while an installed
guard is not running. To remove it:
`systemctl disable --now potluck-box-guard.service && nft delete table bridge
potluck_box_guard && nft delete table inet potluck_box_guard && nft delete
table arp potluck_box_guard` (on a machine set up under zm: `zm-box-guard`,
`zm_box_guard`). An existing Kata `docker=on` binding stops making boxes on a
Potluck network after the upgrade until its owner runs `potluck host share
--docker` again, which installs the guard.

What else the unmasked guest reaches: root in the guest can talk to Kata's
agent, its vsock and the VM's devices, which is Kata's own boundary (the host
treats the guest as untrusted). Through virtiofs it reaches only its own
container's files: a Kata box gets no outer named volume (no shared download
cache, no kept HOME volume; vmhost `outerVolumeEngine` refuses Kata), so there
is no read-only share another tenant also mounts for a guest kernel to remount.

Use an already-created pool with `potluck host <pool> --docker
--oci-runtime <registered-kata> --image <new-image@sha256:digest>`, a distinct
test binding prefix and the normal filtered `potluck-box` network. Read the command's plan before applying root
steps. Sharing runs the existing conformance groups; any failure blocks use.
Do not enable this on production bindings as part of source review.

## Real acceptance

Acquire a short-lived box from only that binding. Copy
`scripts/box-docker-services-check.sh` into it and execute it there. Set
`POTLUCK_DOCKER_FORBID` to at least five literal IPv4 `address:port` targets:
producer, another box, LAN, tailnet and metadata. Establish each positive
control outside the guest first and save its result. If a category has no
reachable endpoint, record that category as unproved; do not treat a timeout
to a nonexistent listener as isolation evidence. The script checks each target
from outer sudo and a nested container holding every capability.

The script builds a Python web image with an ordinary networked build, starts
Postgres and the web service on a user-defined network, queries Postgres through
service DNS, verifies the web response from another container and a published
box-loopback port, and removes only its own containers, network and image.
It also checks the outer capability boundary. Its registry downloads use the
box's ordinary egress and must not be used on a customer job.

Record the runtime version, image digest, exact commands, output and exit codes.
After the successful run, start the example services again and let the box's
lease expire; confirm the outer container and its process tree are gone on the
producer. Repeat with explicit sandbox removal. A client-side cleanup alone
does not prove lease cleanup. Do not advertise the ability until these runtime
and fence results exist.

The guest's published port is a box-local socket. Public preview access and its
lease revocation use Potluck's generic port transport; nested Docker does not
publish a producer port itself.

## References

[gVisor Docker guide](https://gvisor.dev/docs/tutorials/docker-in-gvisor/)
requires raw networking flags for Docker bridges. Those flags remain refused.
[slirp4netns](https://github.com/rootless-containers/slirp4netns) connects a TAP
device in the child network namespace to userspace sockets. This is why tun
support must be demonstrated on the chosen Kata guest.
