# Box networking

How a Potluck box gets a network: so a sandbox can reach the internet (trusted machines open egress; public
machines, later, an allowlist) and never the lending machine or its LAN. The driver's side of the contract (the
`net` argument and capability `net-off`) is in `docs/protocol.md`, "Network". This file covers what the daemon
sends, how `potluck-driver-tart` builds the filtered network on each layer, and how `potluck doctor` and
`potluck host` check it.

## What the daemon sends

- A driver that honours `net` lists capability `net-off` (`potluck-driver-tart` on `vm=oci`).
- The daemon sends it `net` on every box acquire (`runtarget/boxnet.go`): `on` when the binding's machine is
  `trusted` (this machine's own bindings are) and the renter did not ask for `off`; else `off`.
- With `on`, the box is made on the binding's filtered network and nothing wider. On oci that is its `oci_network`:
  Potluck's `potluck-box` (below), or an owner-named one that passed `potluck host`'s net checks. `NET_RAW` is dropped and
  the box has its own resolvers, on a runner binding as on any other. A binding with network `none` stays `none`.
- So a trusted machine's sandbox or job reaches the internet (Claude's API, npm, GitHub) from the lender's IP, and
  never the lender's machine or LAN.
- With `off`, the box is made with network `none` whatever the binding's network, its checkout fetched by the host.

### The answer

- `acquire`'s answer says which the box got, `net`: `on` or `off`. It comes from the driver, which knows the
  binding's network; `potluck-driver-tart` answers it for every box (Tart and Incus boxes are `on`).
- The daemon passes it on in its acquire answer only and never stores it. `potluck sandbox create` prints it
  (`net=on`).
- Fail closed: when the daemon sent `off` and the answer's `net` is not exactly `off` (missing, or `on`: say a host
  put back an older driver while its last advert still lists `net-off`), the daemon releases the box and answers
  `internal`, as for an image off the pin.

### Public machines

A public machine's box gets `off`. No egress allowlist exists yet, so until one does a public machine's oci box has
no network. An allowlist would be a binding key the lender sets.

### Rules on requests and classes

- `net` is part of the request: `on` and unset are the same request, and a `request_id` reused with the other is
  `conflict` at the daemon (the driver answers `bad_request`).
- The class is read at `acquire` only. A machine demoted from `trusted` keeps its existing boxes' network until they
  end (`renew` does not check again), and a `request_id` retried after the class changed is refused as a different
  net.

### Drivers without net-off

A driver without `net-off` (Tart, whose Softnet gives every box egress; Incus; any older driver) is sent no `net`
and keeps its own rule. `net` `off` to it is `bad_request` before it is called, and placement (`potluck sandbox
create --net off`, `potluck run --net off`) skips it with the
reason.

### How code gets in

Unchanged by the network: the binding's checkout comes through the host on a runner binding (a box fetches for
itself only on a binding without `runner_dir` set to `fetch=guest`), and the renter's own commit comes as a bundle
through `exec` (`docs/daemon.md`, "Code from the renter"). So no host ever fetches a URL a renter chose.

## The potluck-box network (oci, docker)

Potluck's filtered network is a docker network named `potluck-box` on the bridge `br-potluckbox`, subnet
`10.199.0.0/24`, whose filter is installed by a systemd unit, `potluck-box-net.service` (`hostprobe/boxnet.go`).

A machine set up when Potluck was still zm has the same network under zm's names: `zm-box`, `br-zmbox` and
`zm-box-net.service`. Doctor reads those names where that is what the machine has (the new ones absent), checks them
the same way, and prints its steps under them, so a lender keeps working with live boxes on it. The two use one
subnet and one filter, so doctor never makes the new network beside the old. Rolling a machine to the new names is
the owner's, at a time with no box running on it:

1. note the cap if there is one (`potluck host doctor` prints it; it lives in the old unit file);
2. `sudo systemctl disable --now zm-box-net.service` and `sudo rm /etc/systemd/system/zm-box-net.service`;
3. `docker network rm zm-box` (docker refuses while a box is on it); the old iptables rules name a bridge that is
   gone, harm nothing, and end at the next reboot;
4. `potluck host doctor`, whose root line now makes `potluck-box`; run it, then share again (with `--net-mbit` for
   the cap), which rebinds `oci_network` to the new name.

Until step 4, a binding that names `oci_network` `zm-box` keeps starting boxes while the old unit is `active`.

### What doctor checks

`potluck doctor` (docker only) looks for the network, readable without root. `docker network inspect` must show
exactly what Potluck makes:

- driver bridge, IPv4 only, not internal, ingress or config-only;
- IPAM subnet `10.199.0.0/24` alone, gateway unset or `10.199.0.1`;
- driver options exactly `com.docker.network.bridge.name=br-potluckbox` (`br-zmbox` under the old names) and
  `com.docker.network.bridge.enable_icc=false`, plus `enable_ipv4=true` and `enable_ipv6=false` if Docker records
  them.

Any other option (a gateway mode, masquerading off, trusted host interfaces) is not Potluck's network and is named.

Its filter unit `potluck-box-net.service` must be `active` (`systemctl is-active`):

- When the unit is missing or inactive, doctor says so (the layer stays ready: boxes then serve jobs only, and
  sandboxes have no internet), and adds to its one root line the steps that write that systemd oneshot unit, enable
  it and restart it.
- When it is `failed`, doctor names the last line of its journal (`journalctl -u`, when readable; for example a
  subnet already in use) and `sudo systemctl restart potluck-box-net.service`, not the root line again.

### The filter unit

- The unit runs only `sh`, `iptables`, `ip6tables`, `sysctl` and `docker`, never Potluck.
- Every `iptables` and `ip6tables` call waits for the xtables lock (`-w`). Each rule, one address apiece, is checked
  (`-C`) and inserted only when missing, so a re-run, as on each Docker restart, never opens a gap or doubles one.
- The rules go in the raw table's `PREROUTING`, the first hook a packet from the bridge meets, where no other
  daemon's ACCEPT precedes it. (tailscaled inserts its `ts-forward`, ending `-o tailscale0 -j ACCEPT`, and
  `ts-input` at position 1 of the filter table's `FORWARD` and `INPUT` whenever it starts, which would outrun rules
  there.)
- It drops, from `br-potluckbox`:
  - tcp 25, 465, 587 and 2525, to anywhere;
  - any address of this machine (`-m addrtype --dst-type LOCAL`) or broadcast address (`--dst-type BROADCAST`);
  - 0/8, 10/8, 100.64/10, 127/8, 168.63.129.16/32, 169.254/16, 172.16/12, 192.0.0/24, 192.168/16, 198.18/15, 224/4
    and 240/4;
  - and, where the host has IPv6 (`/proc/sys/net/ipv6`), every IPv6 packet.
- It then makes the network if it is missing (after the rules, so a unit that failed part way leaves no new network)
  and, with IPv6, sets the bridge's `disable_ipv6`.
- iptables rules end at reboot, so the unit is `WantedBy=` and `PartOf=` `docker.service`, `After=` it (and
  `Requires=` it): it runs whenever Docker starts.

### Fail closed, and what goes unseen

- `potluck-driver-tart` makes no box on `potluck-box` (or `zm-box`) unless that network's unit is `active`. The acquire is then `unavailable` and
  names `--net off`.
- Potluck cannot read the rules without root, so what removes them while the unit stays active goes unseen: a
  firewalld reload on firewalld's iptables backend, an `nft flush ruleset` (iptables-nft keeps its tables in
  nftables), an `iptables -t raw -F`. Restart the unit after one.
- A machine shared without the network binds network `none`, so its sandboxes have no internet, and `doctor` says
  so.

## What potluck host binds

### Runner boxes

On oci a runner must reach GitHub, which a box on network `none` cannot; the driver refuses `runner_dir` with
`oci_network` `none`.

- `potluck host` binds a runner box with `oci_network` `potluck-box` (`zm-box` where the machine has only that) when no `--oci-network` is given, with `--net-mail`
  defaulting to `smtp.gmail.com` (a submission server most networks reach, so the mail check proves something).
- On an oci binding with `runner_dir`, a box whose acquire brought a `jit_config` is made on the binding's
  `oci_network`. That holds for any `jit_config`, with or without `jit_guard`: the guard is the caller's own, its
  shape all the daemon checks, so requiring it would stop no renter and would cut off a renter's own runner, which
  the protocol allows. What bounds a renter's networked box is the network's filter, and that a `jit_config` is
  taken only on a `trusted` machine.
- So is every other box (a sandbox, a job) acquired with `net` `on`, which the daemon sends only for a `trusted`
  machine's box. A box acquired with `net` `off`, or with no `net` (an older daemon), is made with network `none`.
- Either way a runner binding's checkouts are fetched by the host (`fetch=guest` refused), and describe's network
  says so (`potluck-box (filtered by its owner) for GitHub runners and boxes acquired with net on (a trusted host's);
  none for the rest`).
- A box on any network other than `none` drops `NET_RAW` (`--cap-drop`: no forged ARP or addresses on the shared
  bridge, which a Kata guest's own kernel would allow).

### The package shelf changes no network

`potluck host --shelf-gb` adds no rule, network or port: the shelf is files on a read-only volume mounted in every box
(`/opt/potluck-cache`), which a box reads like any file and cannot write. A box's own downloads, filtered as
above, are unchanged and still run (a package not on the shelf is fetched from the registry as before). The host
fetches shelf packages itself, from `registry.npmjs.org` only and without credentials, outside the filtered
network and so outside `--net-mbit`; it paces them to `shelf_fill_mbit` ([shelf.md](shelf.md)).

### Net checks for a runner box

- For a runner box on any network, `potluck host` also passes `--net-allow github.com:443` (a runner must reach
  GitHub; with a name, the box must also resolve it).
- It passes a `--net-forbid` for the forward path past this machine, each only when it opens from here: the default
  gateway on 53, 80 or 443 (`/proc/net/route`) and Tailscale's `100.100.100.100:53`. When none opens, the result says
  the forward path is unchecked.
- Conformance runs the binding's config without `runner_dir`, so its boxes (which bring no `jit_config`) are made on
  exactly the network a runner box gets; the binding carries `runner_dir`.
- A size binds only when its net checks passed, as for any named network. On a mail SKIP the error also names
  `--runner-dir none`.
- Without the network, or with `--oci-network none`, it binds the box for jobs only (no `runner_dir`) and prints why,
  with the root line.
- An owner-named `--oci-network` is used as before: the owner's word, `--net-mail` required.

## Layers other than potluck-box

### OCI with an owner-named network

With `vm=oci` there is no network unless the owner names a filtered one, whose filter, mail included, is the owner's
word. On a named network its resolvers are `oci_dns`:

- comma-separated public unicast IPs, at most 3, IPv6 only in 2000::/3;
- a private, CGNAT, loopback, link-local, multicast, documentation, Teredo, 6to4 or NAT64 one is refused, since the
  filter drops it, it can tunnel to a private address, or it is the host;
- default `1.1.1.1,8.8.8.8`;
- refused with network `none`, or one joining another namespace (`container:`, `ns:`), which keeps that namespace's
  DNS.

They are passed as `--dns` and, but for nerdctl, as a read-only bind of the box's own resolv.conf (under
`state_dir`, removed with the box) onto `/etc/resolv.conf`. That is because on a user-defined network Docker keeps
its embedded resolver `127.0.0.11`, and Podman aardvark-dns on the gateway, which a boundary runtime or the filter
cannot reach. nerdctl writes `--dns` into a resolv.conf of its own, gets `--dns-search .` so no host search domain is
copied, still copies the host's resolver options, puts slirp4netns's resolver first when rootless, and binds the
file writable.

### Tart

Tart boxes run on Softnet: Softnet's gateway blocked but DNS, outbound mail blocked (tcp 25, 465, 587 to anywhere).
Softnet gives every box egress, so Tart lists no `net-off`, and `potluck host` forbids the forward path (the default
gateway, read with `route -n get default`, and Tailscale's resolver) in every Tart size's net checks, as for a runner
box above. `potluck isolation test` runs the same checks on a bound size again (`docs/isolation.md`).

### Incus

Each VM's NICs must have port isolation and an ACL, and the ACLs must drop or reject outbound mail (tcp 25, 465, 587
to anywhere); the driver checks the expanded config before boot. An ACL it cannot read as safe counts as unsafe.

### network=nat

`network=nat`, on any layer, turns the isolated network off and so describes `isolation: "none"`: refused as a box.

## Conformance's net group

- With `--kind box`, group `net` acquires its networked box with `net` `on` from a driver listing `net-off`, so the
  net checks (this machine's addresses, `--net-forbid`, outbound mail, peers, `--net-allow`) cover a sandbox on that
  network.
- It then checks that a box acquired with `net` `off` reaches none of `--net-allow`, while its probe still sees a
  port open inside it. With no `--net-allow` that is a SKIP.
- The flags themselves are in `docs/conformance.md`.
