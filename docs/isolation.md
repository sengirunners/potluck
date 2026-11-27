# Isolation

What Potluck promises about other people's code on your machine, how it refuses work it cannot keep that promise
for, and the test that shows the promise holds on your machine.

## The claim

A box (a sandbox, a job, a GitHub runner) on a lent machine runs in a throwaway VM or isolated container that:

- cannot reach the lending machine itself, on any port (on Tart, but for Softnet's DNS forwarder on the host),
  including the ports your own programs listen on;
- cannot reach the lending machine's LAN: its router, a NAS, a printer;
- cannot reach the lending machine's tailnet: its peers or Tailscale's resolver (`100.100.100.100`);
- cannot reach another box running at the same time;
- cannot send mail (tcp 25, 465, 587 to anywhere);
- holds no credentials of the lender's.
- on Tart, gets none of the lending Mac's USB devices: Apple's virtualization framework cannot pass one through and
  Tart attaches none of its own; its virtual USB keyboard and pointer are turned off with
  `--no-usb-accessories` on Tart 2.38.0 or newer (`vmhost/tartboot.go`). This is read from Tart's source, not yet
  tested on a real Mac (`docs/lending.md`, "What is not proven").

The block test below checks these on your machine by sample: this machine's addresses, the LAN's router,
Tailscale's resolver (standing for the tailnet: a box that reaches it got through this machine's forward path), any
other host you name with `--net-forbid` (a NAS, a tailnet peer), and a second box. It does not try every LAN host or
tailnet peer. Mail is checked by `potluck host` (`--net-mail`) and is blocked in code on Tart and Incus.

How each layer builds that network is in `docs/network.md`.

## Refusal: work that cannot be isolated is not run

Every binding has a class: its isolation (`ephemeral` or `none`) and whether it holds credentials. The daemon uses the
stricter of what the binding says and what its driver's `describe` says. Only `ephemeral` with no credentials may run
anyone's code. Anything else (an isolation it does not recognise included) may not, and the daemon refuses, saying
why in plain words, for example:

```text
run target ci cannot isolate untrusted code (work on it is not walled off from this machine, its LAN and its
tailnet), so only this machine's owner may use it, never the pool
```

- A box binding on such a class is refused at bind; a box whose driver stops describing itself as isolated makes no
  box.
- A run or db binding on such a class takes only this machine's owner's own work (local, never over the pool): the
  head of its branch and its listed scripts. A commit off the branch or a db without a ref counts as untrusted and is
  refused.
- A driver that does not answer `describe` takes no work.
- Each VM layer refuses an isolated boot it cannot isolate (`vmhost/layer.go`): an Incus NIC without port isolation
  or a mail-dropping ACL (an ACL it cannot read counts as unsafe), an OCI network that may reach the host. Tart boots
  an isolated VM only on Softnet. `network=nat`, on any layer, describes `isolation: none`.

## The block test

`potluck isolation test <size>` checks the claim for one size this machine serves: one `potluck host` shared, or a
pool's own size served here (the size a lent machine runs other people's work on). It runs
conformance's `net` group (`docs/conformance.md`) on the binding's own driver and config. These are the blocking
checks `potluck host` runs before it binds a size; host also checks a runner box can reach what it needs, which the
block test leaves out:

- this machine's addresses as they are when the test starts (all but loopback and link-local), on a port the test
  listens on for its length (so a pass means blocked, not closed). An address that appears only once a VM runs, such
  as Softnet's bridge on a Mac, is not among them; Softnet itself blocks its gateway but for DNS;
- the LAN's router (the default gateway, on 53, 80 or 443, whichever opens from here; Linux reads `/proc/net/route`,
  macOS `route -n get default`);
- Tailscale's resolver, `100.100.100.100:53`, when it opens from here: a box that reached it got through this
  machine's forward path to the tailnet;
- any `--net-forbid host:port` you add (each must open from here);
- two boxes at once, neither reaching the other (`--net-peers`);
- a datagram a box sends to a multicast group, which must reach no program on this machine that joined the group on
  any of its interfaces (`--net-multicast`).

It passes only when every one of those checks ran and passed. A check that could not run (a target that does not
open from here, a box without `nc`) proves nothing, so it fails the test. When neither the router nor the tailnet
opens from here and you name no `--net-forbid`, the test fails before it starts, since nothing would show the LAN is
blocked.

It runs the driver file the daemon would run: the one pinned for that driver (`DRIVERS.md`). A driver with no pin,
or whose file is not the pinned one, is refused; pin it first with `potluck driver pin <driver>`.

It refuses to start while sandboxes of any size this machine serves run here (the sizes `potluck host` shares, and
each pool's own sizes served here), or when it cannot tell whether any do: it boots two of its own beside them, past
the sizes' slot limits. That is a check when the test starts, not a hold: a sandbox asked for during the test still
starts. Run it when the machine is idle. The result is kept by the size's name, so a name that names two different
sizes here (a shared size and a pool's, or two pools') is refused: give one another name.

The result is kept in `$POTLUCK_HOME/isolation/<size>.json`: when, pass or fail, what the box had to fail to
reach, the exact conformance command, each check, and a fingerprint of the driver's name, its pinned sha256, and the config and images it tested. A pass counts only
while the size still has that fingerprint; once the size is re-bound differently, or the driver is upgraded and
re-pinned, doctor shows the test as out of date (so does a pins file it cannot read). The fingerprint does not cover
the host's own network: a move to another LAN, a new gateway, or edited Incus ACLs or OCI networking keep an old pass
showing, so run the test again after any of those. `potluck isolation` and `potluck doctor` show it beside the binding:

```text
isolation, per size and target this machine lends:
  mac-c2m4 (sandbox size): isolated: each piece of work runs in a throwaway machine walled off from this machine, its LAN and
  its tailnet, with no credentials
    block test passed 2026-11-12 10:00 UTC: a sandbox reached none of this machine, 192.168.1.1:53, 100.100.100.100:53,
    or another sandbox
```

`potluck isolation --json` and `potluck doctor --json` carry the same, per binding; `block_test_ok` is true only for a
readable record that passed and is not out of date.

To run the same checks by hand against any driver:

```sh
potluck-conformance <driver> --kind box --config k=v... --only net \
  --net-forbid <this machine's address>:<a port it listens on> \
  --net-forbid <router>:53 --net-forbid 100.100.100.100:53 --net-peers --net-multicast
```

## Results

| machine | layer | date | result |
| --- | --- | --- | --- |
| a lent Mac (Tart, Softnet) | tart | not run yet | owed: the first real run of `potluck isolation test` on a Mac is recorded here |
