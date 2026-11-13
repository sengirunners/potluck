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
from outer sudo and a privileged nested Docker container.

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
