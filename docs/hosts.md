# Sharing a machine

How a machine's owner lends it as box targets, and how the VM driver, `potluck-driver-tart` (`vmhost/`), runs the
boxes. None of this is part of the driver protocol (`docs/protocol.md`): another driver may do all of it
differently. Box networking and its filter are in `docs/network.md`.

A page for someone who lends a Mac and does not code, covering what setup installs, who can do what, and how to
pause, leave and remove everything: [lending.md](lending.md).

## Two commands: doctor and host

Any machine's owner lends it as box targets in two commands, with nothing host-specific built in: facts are
detected at run time or given in flags (`hostprobe/`).

- `potluck doctor` is a read-only report. It prints the root step a machine needs, never runs it.
- `potluck host` runs that root step itself (with sudo at a terminal), checks each size, and binds the sizes that
  passed.

On a Mac, doctor and host use the same compatibility preflight. It reads the macOS product version and
hardware architecture, resolves Tart on PATH and Softnet (the managed install of [softnet.md](softnet.md) when
there is one, else PATH), pins those paths in the binding, and runs bounded executable and isolation checks.
Softnet must parse the exact host DNS exception and outbound SMTP blocks used at boot. The probe returns
before sudo, bootpd or vmnet setup; a version string alone is insufficient. Stock Softnet 0.23 lacks these
rules, and newer releases need macOS 26. Which Softnet each macOS uses, and Potluck's own macOS 15 build (a
candidate until it passes real-Mac validation), are in [softnet.md](softnet.md).

Each supported Tart digest carries `arch` and `min_host_macos` in `imagepins/images.pin`. These are conservative
support floors: Tahoe guests are selected on Tahoe hosts, Sequoia on macOS 15, while the ARM Linux guest uses
[Tart's documented macOS 13 host floor](https://tart.run/quick-start/#creating-a-linux-vm). They do not assert that every older host is technically unable to boot a newer guest. Exact-digest
live checks on macOS 15 and 26 remain required release evidence. Unknown digests or missing compatibility
metadata are refused before pulling, even when cached. Add verified metadata before lending a custom digest.
On macOS 15 the default is the pinned Sequoia base. Earlier hosts fall back to the published small Linux
image, which has no GitHub Actions runner;
`--with xcode` is refused when its Tahoe guest is unsupported. Image CPU/RAM floors and the existing disk
budget checks still apply before downloads, and the host redeems an invite only after conformance passes.

## What doctor reports

`potluck doctor [--share 0.5] [--max-cores N] [--max-ram-mb N] [--state-dir DIR] [--json]` reports:

- OS, arch, cores, RAM, free disk and KVM.
- Each layer, with its problems in plain words:
  - `incus`: incus plus KVM;
  - `oci`: a verified local Docker engine with a Kata or runsc runtime (runc, crun and youki are never counted as
    isolation);
  - `tart`: macOS with tart and Virtualization.framework.
- The sizes it would lend (below), exactly what `potluck host` lends without `--sizes` or `--image`
  (`hostprobe.Lend`).
- Every pending root step, as one `sudo sh -c '...'` line.
- With admission keys bound: `admission: <bindings> take a lease from <url> (lease N s, token from PATH) before each
  box`, one line per controller (`admission` in `--json`).
- On docker: whether Potluck's filtered network and its filter unit are there (`docs/network.md`).
- This machine's swap (`swap` in `--json`: `off`, `zram`, `encrypted`, `plain-disk` or `unknown`) and, when it is not
  safe (`swap_note`): a box's memory, the secrets in its memory filesystem included, can be paged out to that disk.
  A Mac's swap is encrypted. On Linux, no swap, zram and swap on a dm-crypt device are safe. `plain-disk` and
  `unknown` (it cannot tell) make `potluck doctor` fail, and the fix is: turn swap off (`swapoff -a`, and remove
  it from `/etc/fstab`), use zram, or encrypt swap (dm-crypt).
  The host reports the same state in its advert, and the pool gives box secrets (the `secrets` verb, an exec's
  secrets, a minted credential) only to a host whose state is safe: any other is refused with the fix named, and
  its secret-free work is not held back. A host on a build from before the field reports none, which counts as not
  safe: update Potluck on it, and it is given secrets again. A box binding on the pool's own machine is not
  gated by the pool; nothing refuses it but `potluck doctor` failing there.
- When a layer is ready, the next step: `next: potluck host   (layer <layer>; the default image unless --image
  name@sha256:<digest>)`.

It exits 1 when no layer here is ready (`no layer here is ready, so this machine cannot share sandboxes yet`), also
with `--json`.

## Sizes

The sizes are `c1m2`, `c2m4`, `c2m8`, `c4m16` and `c8m32` (vCPU and GiB).

- A size is lent when two VMs of it fit within `--share` of the RAM and cores (default 0.5; `--max-cores` and
  `--max-ram-mb` cap it further), and it is not below the floor of the best layer's default image.
- Two, because `potluck-driver-tart` runs two boxes at once per guest OS on a machine by default (`linux_slots`,
  `macos_slots`; Apple's licence caps macOS guests at two per Mac). It counts every live box of its state directory,
  whichever size's binding made it, so two of the largest size lent may run together.
- `potluck host --linux-slots N` changes the Linux number (below, "How many Linux boxes at once").

### How many Linux boxes at once

`--linux-slots N` (1 to 256) binds `linux_slots=N` on every size `potluck host` shares, so the driver runs up to N
Linux boxes at once across those sizes. macOS boxes stay at two per Mac: the flag is refused for macOS images.

- Without an admission controller, every size lent must fit N times inside `--share` (and the caps), since N boxes
  of the largest size may run together. With no `--sizes` that leaves the sizes that fit N times; a named size that
  does not, or none at all, is refused (`no size fits N times inside the share: raise --share or the caps, or lower
  --linux-slots`). N of 1 or 2 keeps the rule that two must fit.
- With an admission controller bound (`--admission-url`, or kept from an earlier run), the controller decides room
  for every box and fails closed (no lease, no box), so N is the owner's ceiling and the sizes fit twice, as without
  the flag. On a large host whose controller has room, raise N to lend more than two boxes.
- Every size of one share must carry the same N, since the driver counts every live box against each binding's own
  number: a run whose `--sizes` leaves out a size bound with another number is refused until that size is in the run
  or has the same number.
- A re-run without the flag keeps the earlier bindings' number (bindings that disagree are refused until the owner
  passes `--linux-slots`); one box at a time (below) is each run's own finding and is not kept. A run that could not
  read those bindings is refused, `--admission-url none` included, since it would drop their number.
- N of 2 or more on a machine where nothing fits twice inside the default share is refused, with an admission
  controller or without: Potluck would have to run one box at a time. Pass `--linux-slots 1` or a larger `--share`.
- A run that passes the flag but could not read the earlier bindings is refused too, unless it shares every size
  (`--sizes c1m2,c2m4,c2m8,c4m16,c8m32`): it cannot check the sizes it leaves alone.
- Conformance runs with `linux_slots` of N, at least two (its peers check runs two boxes), since its boxes count
  against the same slots as the live ones.
- So a re-run on a busy host needs free slots for conformance. When live boxes hold every one (conformance's first failure is
  the driver's `no free linux slot`), the size stays unbound as `every Linux slot on this machine is in use by live boxes (N at once), and
  conformance needs free ones for its own boxes (two for its peers check): try again when one ends, or raise
  --linux-slots`, and the quick check's other sizes say `not checked: every slot was in use ...` instead of blaming
  the security checks. An earlier binding of the size stays bound as it was. On macOS the line names Apple's two.
  Before that, the check waits for a slot to free: it runs the size's conformance again for up to `--wait` (default
  30m; `--wait 0` gives that answer at once), with a line per change (`<binding> has no room: every Linux slot is in
  use by live boxes; asking again at ...`).

### A machine too small for two

When the owner gave no `--share` (caps alone keep the default) and no size fits twice inside the default half:

- `potluck host` lends the sizes that fit once inside it, none below the image's floor. (Example: a 4-core, 7 GB
  Linux machine fits one `c1m2` there, not two.)
- It binds each with `linux_slots=1` and `macos_slots=1`, so the driver runs one box at a time across the bindings it
  makes, and they never take more than the default half. A binding of another size left from an earlier run keeps
  its own slots.
- The result says so in one line, and doctor's line reads `sizes (one at a time: none fits twice inside 50%)`.
- Conformance runs without those keys: its peers check runs two boxes at once, and two that fit once inside half fit
  inside the whole machine.

This does not apply when sizes fit twice but none clears the image's floor: there two boxes of a size that fits
once could take the whole machine during conformance, so that stays refused. An explicit `--share`, even 0.5, keeps
the rule that two must fit: nothing fitting twice is refused, as before.

## The root step

### What host does with it

`potluck host` runs the root step it needs itself, once its own refusals have passed (`hostprobe.Precheck`: host's
dry run; when that stops at a layer that is not ready, its dry run again with that layer as the root step would
leave it). So Potluck never asks for root and then refuses.

The step is doctor's root steps for the layer it uses (`--layer`, else the best available, else each supported
one), less the filtered network's steps when no box would serve a runner on it (`--runner-dir none`, an image with
no known runner, or the owner's own `--oci-network`).

- When there are any, and stdin and stdout are a terminal, it prints one plain sentence of what and why (`potluck
  needs admin rights once to ...`, ending with what the steps set up: the firewall its VMs run behind, what its VMs
  need, or both; `cli/host.go` `rootWhy`).
- It runs that very script (`hostprobe.RootScript`, which `RootCommand` quotes for doctor) as `sudo sh -c <script>`
  on the terminal. sudo asks for the password itself; Potluck never reads, sees, stores or passes it.
- Then it reads the machine again and goes on as if the owner had run it.
- Not at a terminal, without sudo, with `--no-sudo`, in a dry run, or when sudo does not finish (a wrong password, a
  failed step), it says so in plain words and goes on as before: it prints the root line and lends jobs only, or
  refuses.
- Ctrl-C at sudo's prompt reaches sudo from the terminal (Potluck does not kill it, so sudo restores the terminal's
  echo) and stops Potluck, as anywhere.

### Reading Docker's socket owner

On every Docker connection the driver checks, as root, that the socket belongs to docker.service's own root dockerd
in this machine's namespaces (`vmhost/oci_engine_owner.go`). It runs that read with `sudo -n -k`: no prompt, and a
sudo ticket cached at this terminal ignored, because the daemon's driver has no terminal and no ticket. Doctor and
host ask the same read, so a password typed for sudo a minute ago does not make them call oci ready.

- With passwordless sudo for everything, the read runs inline and needs no root step.
- Otherwise the root step installs the read as a root-owned helper, `/usr/local/libexec/potluck/docker-owner`, and a
  sudoers drop-in `/etc/sudoers.d/potluck-docker-owner-<uid>` allowing that user exactly that path with no arguments.
  The step removes the rule again unless `sudo -n -k` now runs the helper as that user.

### Kata and passwordless sudo

A Kata box runs inside a systemd slice of its own that bounds its VM on the host (`docs/oci-host-budget.md`). The
driver makes and removes that slice with `busctl` and `sh` as root, with `sudo -n -k`, for every box, and reads
Docker's journal with `journalctl` the same way where Docker hides its runtime options. So Kata needs the lending
user's own passwordless sudo, which the root line does not grant (`sh` as root is all of root).

- Doctor and the driver both run a harmless form of those calls (`busctl --version`, `sh -c :`) with
  `sudo -n -k` (`vmhost/oci_host_parent.go`): a password typed at this terminal does not count.
- Without passwordless sudo, oci is not ready on Kata. Doctor and host say so once and name two ways on: give the
  user passwordless sudo yourself, or lend with gVisor instead, with the exact root line that installs it (the layer
  takes gVisor before Kata once docker lists it).
- The driver refuses a Kata box before its admission lease with the same reason.

### Installing gVisor

On Linux with a container CLI but no isolating runtime, the root line installs gVisor from its own release channel.
(Debian and Ubuntu carry no `kata-containers` package, and Ubuntu's `runsc` is a 2023 or 2024 snapshot.)

- The release is pinned in `hostprobe/gvisor.go`: one point release, `<yyyymmdd>.0` (never `latest`, and never the
  bare date, which is that date's latest point release).
- Per arch (`amd64`, `arm64`; another arch gets a problem and no step) it pins the URL
  `https://storage.googleapis.com/gvisor/releases/release/<yyyymmdd>.0/<x86_64|aarch64>/gvisor.tar.bz2` and the
  SHA-512 its `.sha512` publishes, written into Potluck.
- In one step it downloads into a fresh `mktemp -d` (curl `--proto =https`, else wget), checks that SHA-512
  (`sha512sum -c`), and only then unpacks into `/usr/local/bin`: `gvisor-bin/` (the sidecars runsc needs) and
  `containerd-shim-runsc-v1`, then `runsc` last.
- The temporary directory is removed on every path. A failed download or check unpacks nothing and stops the script.
  Never a script piped into a shell.
- curl or wget and bzip2 come first from the distribution when missing.
- With docker it then registers that runsc (`/usr/local/bin/runsc install --download-sidecars=NEVER`: gVisor merges
  runtime `runsc` into `/etc/docker/daemon.json`, keeping every other key, backing the old file up as `daemon.json~`,
  replacing a `runsc` entry docker never loaded, and downloading nothing).
- It reloads docker (`systemctl reload docker`: dockerd re-reads its runtimes on SIGHUP, so running containers keep
  running; never a restart), waiting up to 10 s for `docker info` to list it.
- Podman and nerdctl are unavailable for isolated boxes: doctor reports the missing engine proof before a pull.
- A runsc already on the PATH that docker does not list is never registered as it is (it may be the user's own,
  writable without root, or lack the sidecars its `runsc install` would fetch unpinned): the same pinned install
  replaces it in `/usr/local/bin` and docker gets that one.
- Kata, when installed, is used as before; Potluck never installs it.

## potluck host

```
potluck host ['<invite code>' | <pool>] [--image name@sha256:<64 hex>]... [--layer] [--sizes] [--name-prefix]
  [--oci-runtime] [--oci-cli] [--oci-network] [--net-allow] [--net-forbid] [--net-mail] [--net-mbit] [--runner-dir]
  [--docker] [--root] [--shelf-gb] [--shelf-fill] [--volume-max-gb] [--linux-slots]
  [--admission-url] [--admission-token-file] [--admission-ttl-s] [--full] [--reclaim-idle-s | --when any|away]
  [--share] [--max-cores] [--max-ram-mb] [--state-dir] [--no-sudo] [--dry-run] [--detach] [--yes]
```

`potluck host` shares with a pool. `potluck host stop [--force]` removes every size it shared; when it removed
some and left nothing shared, a paused host switch goes back to auto, so the next share lends at once.
`potluck host status` shows what this machine lends and runs now, and whether it takes new work, then the setup
checklist below; it changes no setting. `potluck host jobs` lists every job this machine ran, with counts and an
electricity estimate ([menubar.md](menubar.md#jobs-this-mac-ran)). `potluck host lend --cores N --memory-gb N
--keep-free-gb N` turns down, or back up, what this machine lends without a setup run; jobs running finish
([menubar.md](menubar.md#memory-and-the-dials)).

### Stages and the setup checklist

A host run goes through five stages and says each as one line on stderr (not under `--json`), `[n/5] <stage>: <words>`, ending in
`succeeded` or `blocked`:

| stage | what it is |
| --- | --- |
| preflight | everything checked before a download: the layer, the image's fit, the disk, Softnet, the admin step |
| images | `image N of M`: each pinned image pulled, or reused when it is already here at its exact digest: on oci, when `docker image inspect --format '{{json .RepoDigests}}' <pin>` lists the pin itself. That is how a host lends a private image it loaded itself without a registry credential: `docker load` keeps the pin only on Docker's containerd image store, from an archive whose index names the pin's repository (a pull says its percentage) |
| validation | the conformance checks, `size N of M` |
| join | joining the pool of the invite (only once a size passed), or the pool already joined |
| usable capacity | sharing the sizes that passed and seeing the pool list them: what the pool can use |

A blocked run's error says what happened and what to do. A run that went through ends with `next:`, the one thing
left to do. The pull and conformance command lines are diagnostics: `--show-commands` prints them.

The run writes its stages to the home's `host-setup.json` as they change (never the invite code; the command that
runs it again names it `'<invite code>'`). `potluck host status` reads that file with what else is set up and prints
the checklist, and `--json` carries it as `setup` (`state`, `stages`, `checks`, `next`):

- Potluck's version and whether its daemon runs;
- the last setup: none, running, succeeded, blocked at a stage, or interrupted (its record says it was still going,
  but no run holds the lock);
- what this machine shares now, and apart from that, when it may lend: only while you are away (after the idle
  time, `--when away`) or any time, and whether it takes work now;
- on a Mac, the menu bar item ([menubar.md](menubar.md#what-host-status-knows-of-the-app)): installed, running
  (only once the app itself has read this status), and Login Items, which Potluck cannot read;
- the daily update (`potluck upgrade enable`), which covers Potluck alone: not the menu bar item, Tart or Softnet.

`next:` is the first thing left in that order. Nothing in the checklist is done for you: it never installs the
menu bar item, adds a login item or turns on the daily update. The menu bar item's title and note come from the
same facts, so the menu and the terminal name the same blocked stage.

Run again where this machine already shares sizes with the pool (`<prefix>-<size>`: in a pool this machine runs,
its sizes there; in a pool it joined, the machine-level sizes shared with that pool), host would pull, re-check and
share them again. A bare re-run, with no flag that changes the share (none, or only `--no-sudo`, `--pull-timeout`,
`--show-commands`, `--detach` and the host program's AfterLend skip), does none of that: it prints what this
machine shares and the dry run's plan, changes nothing, and exits 1 with a `conflict` error asking for `--yes` (under
`--json` stdout stays empty and the error is the answer; when the re-run would fail, the error says why instead).
A run with a flag that says what to change (`--image`, `--with`, `--sizes`, `--docker`, `--admission-url` and the
rest) goes ahead as before, as do a join with an invite code and the old spelling `host share`, which programs run.
With no pool named, on a machine in more than one pool, host names them and the forms it takes: `potluck host
<pool>` to share, `potluck host status` to look (in a program that mounts host, in its words).

One host run at a time per home (`host.lock`): a second is refused, naming the first. `--detach` runs the checks,
the admin step and a dry run of the plan here, then the same command (its flags as given, without `--detach` and
`--json`, with `--no-sudo`) again in the background, hands it the lock held, and returns; its words go to the
home's `host.log`, and `potluck host status` (and the menu bar item) says that it goes on, with its last line.

`potluck host pause [--for 2h | --until 23:00]` closes the host switch (no new work; what is shared stays shared)
and is refused while work runs on a shared size, which it would end, or when whether it runs cannot be read
(`--json`: `{"state":"refused","refused":{"reason":"job_running"|"job_unknown",...}}`, exit 0). `potluck host resume` hands a closed or draining switch back to auto. Both
are words for the one switch file `potluck host set` writes and every driver reads.

A program that mounts these commands ([mounting.md](mounting.md)) may give host a step of its own once a run lent
the machine (`Host.AfterLend`), with a flag that skips it.

`--state-dir DIR` sends the same absolute driver state path to conformance and every shared binding.
Without it, the driver keeps its own default state directory (see [the protocol](protocol.md));
`POTLUCK_HOME` selects daemon state and the default disk-space probe path.

`--net-mbit N` (oci) caps what all sandboxes on Potluck's network download together, for a machine whose uplink is
guarded. It is refused when the layer lent is not oci (a Tart box is not on that network), with or without a cap to
set, never accepted and ignored. A volume named `potluck-cache` gives every sandbox a read-only tool cache
([cmd/potluck-driver-tart/README.md](../cmd/potluck-driver-tart/README.md), "A guarded uplink"); the next section
turns it on.

### The package shelf (oci)

```
potluck host --shelf-gb N [--shelf-fill seed|boxes|off] [--volume-max-gb N]
potluck host shelf status
potluck host shelf fill --npm-lock package-lock.json
```

The shelf is the `potluck-cache` volume holding public npm tarballs that the host fetched and hash-checked, read-only
in every box, so `npm ci` finds its packages there instead of downloading them
([shelf.md](shelf.md) says how a box uses it, who fills it and the limits).

- `--shelf-gb N` turns it on: it makes the `potluck-cache` volume when the engine has none (before conformance, so
  conformance tests the binding as it will run) and binds `shelf_max_gb=N` on every size this run binds. `0` fills
  nothing; the volume and what is on it stay, and boxes keep reading it (delete the volume to take it away).
- `--shelf-fill` binds `shelf_fill`: `seed` (the default: only `potluck host shelf fill` puts anything on the
  shelf), `boxes` (also the public packages the lockfiles in boxes name) or `off`. `seed` and `boxes` need a shelf
  above 0. No other flag turns box fill on: it is on only where you passed `--shelf-fill boxes`, or where an
  earlier binding of the same engine already has it (a re-run keeps it, and the output says "kept from" that
  binding) until you pass something else.
- `--volume-max-gb N` binds `volume_max_gb`, the most each renter's named volumes may hold on each size (default
  20; `0` turns volumes off; each size counts its own, so it is kept alike across this run's sizes, not across the
  engine). The shelf is not a renter's volume and does not count against it; copy-on-write copies
  do, so raise it where boxes use `cow` volumes.
- One engine has one `potluck-cache` volume, and each size applies its own `shelf_max_gb` and `shelf_keep_s` to all
  of it (the smallest wins). So `potluck host` binds the same values on every size it binds, a re-run without the
  flags keeps what is bound, and a run that would leave another size of the engine (a `--sizes` that leaves one
  out, other sizes bound by hand) with a different `shelf_max_gb` or `shelf_fill` is refused, naming that size.
  It reads the bindings in the scope it binds in (this machine's, or the pool it runs), so sizes bound in the other
  scope are not checked; `potluck host shelf status` reads both. A size that fails
  conformance stays bound as it was, with its old limits: the output says so, and the run fails; run it again.
  `potluck host shelf status` warns where sizes of one engine differ in `shelf_max_gb`, `shelf_fill` or
  `shelf_keep_s` (which `potluck host` does not set: set it alike by hand). A re-run keeps `shelf_keep_s`,
  `shelf_fill_mbit` and `shelf_fill_delay_s` as set by hand (from the first of its sizes that has them), as it keeps
  the three flags' values.
- All three flags are refused on a layer that is not oci.

**Sizing.** `shelf_max_gb` counts every file in the volume (the helper in `bin/`, `tools/` and `goproxy/` count too),
not disk blocks. Fills stop at it and evict npm blobs older than `shelf_keep_s` (30 days by default), then, while the shelf is
over the cap (after lowering it), the oldest ones; a fill never pushes out a blob still within its keep time to
make room, so a cap below what your lockfiles name drops the rest as `full`. Fill from your lockfiles
(`potluck host shelf fill --npm-lock`; it counts what it dropped as `full`), then read `potluck host shelf status`:
its bytes against the limit, and raise `--shelf-gb` while anything is dropped. Nothing is fetched below `persist_min_free_gb` of free disk (default 10), and fills are paced
to `shelf_fill_mbit` (default 16 Mbit/s), which is outside the filtered network's cap (`--net-mbit`), so set it
below what your uplink can spare.

**What `shelf status` prints.** For each oci size (one per state directory): the shelf's path, bytes and file count
(never a package name), the fill mode, the limits (`shelf_max_gb`, `shelf_keep_s`, `shelf_fill_mbit`,
`shelf_fill_delay_s`, `persist_min_free_gb`, `volume_max_gb`) and how much of boxes' wants is queued. It makes no
volume and fetches nothing (it may measure the shelf, in a read-only helper container, as describe does).

**What the owner accepts.** Every box on the engine can list the shelf, because it is mounted read-only and
the files are named by hash. A box can therefore tell that someone on the same host recently used a given public
package (within `shelf_keep_s`): the presence of its tarball. It cannot tell who, or which project, or read what
anyone installed that is not public: the shelf holds only what the public registry served and the host hashed, and
a private package is never fetched. With `shelf_fill=seed` this shows only what the owner seeded;
`--shelf-fill boxes` also shows what boxes asked for. Nothing a box writes reaches another box: the shelf is
read-only to boxes and only the host writes it.

`--root` (tart) lends sandboxes whose user keeps the image's passwordless sudo, as GitHub's own macOS runners do:
root inside a throwaway VM that is deleted after each lease. Cirrus Labs' macOS images give their user `admin`
passwordless sudo, so with `--root=false` they fail the root group's "without capability root, sudo does not work in
the box" check and do not bind. The Mac pins default root on; each size binds `root=on`, describe lists capability `root`, and the root
group checks that sudo works and that the net probe, run as root, still cannot open this machine's addresses,
`--net-forbid` or `--net-mail` (a macOS guest has no Linux bounding set to check). The output says so in plain words.
A re-run keeps earlier `root=on`, else the pin decides; pass `--root=false` on each run to turn it off. oci refuses it (`--docker` gives root there).

### What it lends

Without `--image` it lends Potluck's default image for the chosen layer (`imagepins/images.pin`, below). It refuses,
before running anything:

- an image not pinned by digest;
- a layer that is not ready (printing the one root command);
- a named OCI network (`--oci-network` other than `none`, whose filter Potluck cannot read) without `--net-mail`.
  Such a size is left unbound unless a mail check passed: all SKIPs prove nothing.

A layer `potluck-driver-tart` cannot serve yet (`incus`) is reported by `doctor` but never bound;
`hostprobe.Supported` is the one place to change that.

### The check before binding

Then it pulls each image through the layer's CLI and, per size, runs `potluck-conformance <driver> --kind box` on the
box config (`vm`, `size`, `cpu`, `memory`, `images`, and for oci `oci_runtime`, `oci_cli`, `oci_network`), with the
box network targets: `--net-peers`, the owner's `--net-allow`, `--net-forbid` and `--net-mail`, and a `--net-forbid`
for each of this machine's own addresses but loopback and IPv6 link-local, on a port `host` listens on during the
runs, so a pass means blocked rather than closed.

It binds `<prefix>-<size>` (kind box, ephemeral, credential-free, the images pinned; the prefix defaults to this
machine's name) only if it passed. A failed size stays unbound and is reported.
A size that failed because a driver call ran past its time limit (the suite's `every verb answers in 30 s, acquire
in 120 s`, or a verb with `no answer within` its limit) says so and to run this again once the machine is quiet:
another share, an image pull or other heavy work on the machine fails it this way though the driver is sound.

By default the check is quick:

- The first size runs `--only handles,lifecycle,refuse,net,root,box-secrets` with the network targets. That is every
  security check: a new box sees nothing of the last, renter secrets and their refusals, refused images, shell
  strings and foreign handles, peers, this machine's addresses, `--net-forbid`, outbound mail, for a box listing
  `root`, that root has no `NET_ADMIN` (on a macOS guest: that root still cannot reach the forbidden addresses), and, for a driver listing `box-secrets`, that box secrets are isolated
  (modes, a memory filesystem, no swap, the owner, a planted path, no value in env, argv or the driver's state, gone
  after release).
- Each other size runs `--only boot`: one box of that size boots, answers its size and exec, releases (`boot` runs
  only when named).
- When the first fails, no size binds.
- It drops `renew` and `expiry`, which wait out short leases and are the driver's own code on every size, and
  `box-secrets-ops` (unset, an exec's own secrets, atomic replace, refusals), which waits out a 5 s reader and is
  the driver's own code too (`code`, `ended` and `reclaim` are SKIPs for a shared machine anyway).

`--full` runs the whole suite, network included, on every size. Measured on the reference driver: 3 sizes in 3.8 s
quick, 44 s full, before box secrets; `box-secrets` adds about 0.5 s to the quick check and `box-secrets-ops` about
5 s per size to the full one. `--dry-run` prints the plan.

### When a box does not start

A box whose VM exits while it boots fails with the VM layer's own words: its exit code, the line of its output that
says why (secrets redacted, at most 200 bytes), and in brackets the programs that ran it (on Tart: tart's version,
Softnet's version and path, the macOS release). A cause known to come from this machine's setup is marked
`setup fault`: the same boot fails the same way until the setup changes, so `potluck host` says running it again
unchanged fails the same way, and what to do instead. The one known today is Softnet refusing a rule Potluck isolates
a box with (`out proto udp @host port 53`; seen with Softnet 0.23.0 on macOS 15.6): Potluck needs a Softnet that
accepts protocol and port rules ([softnet.md](softnet.md) says which one this macOS uses). Any other boot failure carries no such mark, and may pass when run again.

Every start that fails (a boot, a checkout, a runner, but not a cancelled acquire or the admission controller's no
room) leaves a report, which its error names with the machine it is on (`report: <path> (on <machine>)`, since the
error also reaches callers on other machines): `failed/<id>.txt` in the driver's state directory (`state_dir`, by default
`~/.potluck-driver-tart`). It holds why, and the last 16 KiB of the run log (the VM layer's output) and of the
supervisor's log, with invite links, tokens, credentials in URLs and other secrets redacted and the home directory
written `~`. It stays after the VM and the record are gone: the 20 newest reports are kept, none older than 7 days.

### Images in the binding

The binding carries the pins as its `images` only, never also as a config key `images`. The daemon sends them to
the driver as config `images`; a machine-level binding with both could not be read back from `run_targets.toml`.

When the driver cannot read a binding's image (it is gone, after a disk swap, or Docker cannot be read), it describes
the box without its size, and the daemon does not advertise or place that size. The binding's error (in doctor, and
in the bindings list) gives the driver's reason and says to run `potluck host` on that machine: it checks the machine
again, pulls its image if that is missing and rebinds the size in place.

A size that no box fits on this machine any more (more cores or memory than it has, after a hardware or disk swap)
is never rebound. `potluck host` unbinds such an earlier `<prefix>-<size>` on the layer it lends, and says so; if
the unbind fails it names the command that does it. A size that still fits stays bound, also when this run does not
lend it (it came from an earlier `--share` or `--size`).

### Hand-back on a Mac

On macOS the binding also carries `reclaim_idle_s` (`--reclaim-idle-s`, or `--when away`, default 300; 0 or
`--when any` lends all the time; refused elsewhere). Without either flag a run again keeps what the earlier binding
of the same name carried (`reclaim_idle_s_kept_from` in `--json`). A box ends as `reclaimed` (never billed) within about a second of the owner's keyboard or pointer input,
and none starts until the Mac has been idle that long. Conformance runs without it, since the owner is at the
keyboard.

### Sleep and reboots on a Mac

With `vm=tart`, each run's supervisor holds `caffeinate -i -w <its pid>` while its VM lives, so the Mac does not
go to idle system sleep under a box (the display may still sleep). The hold ends with the supervisor, including when
it is killed. A warm box counts: it keeps the Mac awake while it waits. A Mac with no box up is not held: `potluck doctor` warns when the Mac goes to system sleep.

With FileVault on, a Mac that reboots runs nothing until its owner logs in. To restart for an update without
that stop, run `sudo fdesetup authrestart`.

### Pools

`potluck host '<code>'|<pool> [--image pin]... [flags]` shares with a pool:

- It runs the root step as above, once the pool is known (with sudo at a terminal; `--no-sudo` prints it instead),
  and lends the same sizes (one box at a time on a machine too small for two at the default share).
- It runs the checks, pulls and conformance first (`hostprobe.Check`) and joins with the code only when a size
  passed. So a machine that cannot lend, or fails the security checks, never spends the invite. A pool named must
  already be joined, or run here, where it binds in the project.
- It binds each passed size at machine level (`hostprobe.Bind`) and waits up to 45 s for the pool's machine to list
  each as `<binding>@<machine>` (a host's heartbeat carries it every 15 s). A slower one is reported, not an error.

#### Pasting the invite, and what is safe to share

`potluck invite <name> --host` prints the command for the friend's machine as one line, the whole invite in single
quotes (`potluck host '<invite>'`); from a program that mounts the commands it also prints that program's whole line
(`where they run zm instead, the same as one line: zm potluck host '<invite>'`). Copy the line as it is. Host and
join check their arguments before anything else (the daemon, the host lock, any download), and refuse, exit 2, saying
what is wrong and repeating nothing typed:

- only the invite's last part (`XXXX-XXXX`, its secret), or an invite missing a part or cut short;
- a word a copy changed: a no-break or other typeset space, a curly quote, a long dash, an invisible character. A
  command pasted over several lines whose indentation is no-break spaces arrives as extra words such as
  `<no-break space>--image`; the refusal names the flag it looks like. Nothing is corrected and used: a secret is
  used as typed or not at all;
- more than one argument (an invite without its quotes, or split by a paste).

The invite is a secret until it is used: send it to the one friend, never to a group chat or support thread. What
`potluck host`, `potluck host status` and `potluck doctor` print holds no invite, and the refusals above repeat
none, so that output is safe to share when asking for help. A start's failure report is redacted too (above).

How pools, invites and machine classes work is in `docs/daemon.md`.

## Default images and templates

### images.pin

`imagepins/images.pin` is embedded in Potluck, so its pins update with Potluck.

- One line per layer and guest OS: the layer, the OS, then a full digest, or `-` while Potluck's own image is
  unpublished. A test fails on any other.
- `tart`: Cirrus Labs' macOS base image on a compatible host, else a compatible Sequoia or small Linux image. `oci`: Potluck's Linux image from `cmd/potluck-driver-tart/images/linux`,
  built and pinned by `.github/workflows/box-images.yml`.
- A layer with no image is refused in plain words naming `--image`.
- A line may carry the image's floor, `min_cpu=N min_memory_mb=N` (a Tart image's `cpuCountMin` and `memorySizeMin`,
  below which `tart set` refuses). No size below the floor of a known image, default or `--image`, is lent or
  accepted in `--sizes`; a `--sizes` under it is refused before anything is pulled, naming the sizes that fit. A
  published macOS Tart line must carry both (a macOS guest under its floor fails only at `tart set`, after the pull).
  A Tart image with no pin is refused before any pull, as its host compatibility is unknown; zm does not guess an
  image's OS or floor from its name.
- A line may carry `runner_dir=PATH` (below) and `template=NAME`.
- The pin is checked at acquire like any other.

### After an update: the daemon moves a host to the new pins

A binding keeps the pins it was bound with, so a host lends the images of the build that shared it until something
rebinds it. The daemon does that itself for its own machine-level box bindings (`runtarget/hostimages.go`), at start
(an update restarts it) and every 10 minutes after:

- It moves only a binding whose images Potluck chose (`imagepins.Current`). `potluck host` records who chose them in
  `image_default`: `true` when it lends with no `--image` or `--with`, `named` for `--with NAME`, `false` for the
  owner's `--image`. A binding from before that key counts as Potluck's when it lends template images beside its
  first (share adds those only without `--image`) and every one is from a repository `images.pin` publishes on its
  layer. Each image moves to this build's pin of the same repository, in the same order; a template the binding did
  not lend is not added. A binding with `image_default=false`, any other value share does not write, or (with no key)
  any image Potluck does not publish is never touched.
- It changes the images only. It leaves the binding as it is, and its view names `potluck host --yes` (with
  `--with NAME` for a named image) for the owner to run, when what share binds from the pins would differ (the
  binding's `runner_dir` or `image_name` is not the new pins', or a size floor is above the binding's size), when
  this build no longer publishes one of its repositories, or when the binding is from before the key and lends one
  image (it may be an owner's `--image` of Potluck's repository). So are a Mac's Tart bindings: their images are
  tens of GB, pulled when the owner asks.
- On `oci` it pulls the new images with the binding's `oci_cli`, as `potluck host` does, then asks the driver to
  describe the binding with them and keeps the isolation check of a bind: a pull, describe or check that fails
  keeps the old pins.
- It rebinds only while the binding has no live handles, stopped boxes among them, and no acquire or start has come
  in since it began listing them (an acquire counts itself in before it reads the binding), so no box is dropped and
  none boots the old pin unseen. Warm boxes of the old pin are ended, never handed out. The next heartbeat (asked for
  at once) advertises the new pins.
- Until then the binding lends the pins it has. The owner's host list carries `images_behind` (what it lends, what
  this build pins, and why: pulling, waiting for its boxes, a failed pull, or the command for its owner), and
  `potluck host status` prints a line for each such size.

Conformance does not run again for the move: the driver, layer, runtime and every config key stay as the owner's
share checked them, and only the digests of Potluck's own images change. `potluck host` run again checks it all.
The block test's record keys on the images, so after a move `potluck isolation` reports it out of date until the
next `potluck host` runs it again.

### Named Mac images

`potluck host --with xcode` lends the pinned Cirrus macOS Tahoe Xcode 26.3 image,
with its `actions-runner` folder and passwordless sudo inside the throwaway VM.
No `--image` or `--runner-dir` is needed. The base Mac image also defaults to root
inside the VM; `--root=false` disables it and `--runner-dir none` disables Actions.
The driver refreshes an outdated runner before starting an Actions job.

Named pins use `name=NAME` and may carry `root=on`. They are separate from
sandbox templates and are never added to the default image list. `--with` accepts
one name and refuses `--image`. Each binding lends one named image.
Use `runs-on: [friends, macos, xcode]` (replace friends with the pool label)
for Xcode jobs. The extra `xcode` label requires the binding's default image to
advertise the pin name `xcode`; its digest may differ from the broker's version. Base Macs never take that job; if no Xcode Mac
is available it waits. A size label such as `friends:c2m4` can be used with
`macos, xcode` too, and an explicit size-to-binding mapping still must match
the image. Jobs without `xcode` prefer base Macs, then may use an Xcode Mac.
The host persists the selected pin name on the binding and stamps it into describe and the pool heartbeat, so the
same rule applies across machines. Older hosts without that fact cannot take
an Xcode job.

`potluck host --with small-linux` lends Potluck's small Linux image on a Mac (about 1 GB, no
root, no GitHub runner), what `potluck try` lends there first ([docs/try.md](try.md)).

Running host again without `--with` refuses to replace an earlier named image;
repeat `--with xcode` to keep it, or use an explicit `--image` to replace it.
An earlier image this version cannot name (the owner's own `--image`, or one from a repository Potluck no longer
publishes) does not stop a re-run with no flags: host lends the default in its place and says so, naming the bindings
and the exact flags that keep the old image instead (`--image ...`, and `--runner-dir` when the binding had one).
If earlier bindings cannot be read, including a dry run while the daemon is down,
select an image explicitly or start the daemon before retrying.
Pins also carry `min_free_gb` and `pull_minutes`: the base Mac budgets 60 GB
and 60 minutes; Xcode budgets 200 GB and 180 minutes for its 67.5 GB download,
120 GB disk and box space. Selected-image disk warnings appear in the host plan.
`--pull-timeout` overrides the pin's budget. Cached images may need less room.

Before pulling a Tart image, Potluck lists the local cache. An OCI entry with the
exact full digest reference is reused; a tag or mutable local VM is insufficient.
An unreadable listing falls back to pulling. Conformance still runs before binding.

### Freeing a Mac's disk after a failed or replaced setup

```
potluck host images [--remove [image...] | --forget-home <dir>]
```

`potluck host images` reads Tart's list once and says, for each image and VM, what it is to Potluck, with its size
on disk as Tart measured it, or an estimate from its pin's `pull_gb` where Tart gives none:

- in use: a size this machine shares, in any pool it runs or at machine level, lends it (a sandbox size's images, a
  runner's `image`), or it is a Potluck sandbox's VM (`potluck-<hash>-...`; its driver deletes it when the sandbox
  ends, and keeps a kept sandbox's disk);
- retained: Potluck's published image for this Mac (its macOS and architecture meet the pin's floor), which a
  retry, or `--with` its name, lends without downloading it again; or an image that would be removable but that
  Potluck cannot show is its own and unused (below);
- removable: an image Potluck pulled that nothing here needs: a pin this Mac's macOS cannot lend (the Tahoe image on
  a macOS 15 Mac) or an earlier digest of a pinned image that no size lends;
- absent: this Mac's default image or the small Linux image, which a retry downloads, or a shared size's image that
  Tart does not hold (its sandboxes are refused until it is pulled again), with about what the download is;
- not Potluck's: a tag (Potluck pulls by digest) or a VM Potluck did not make: left alone.

What this machine shares is read from every Potluck daemon running here: this command's, and another program's
built on Potluck (by its machine record, `docs/daemon.md`), in every pool and at machine level.

The pinned repositories (`ghcr.io/cirruslabs/macos-*-base` and others) are public, so a digest in one may be the
owner's own. `potluck host` keeps a record, `potluck-pulls.json` in Tart's directory (`TART_HOME`, else `~/.tart`):
when a share starts, the images it will use and the daemon home it runs on; when it pulls one, Tart's list having been
read and lacked it, and the pull having succeeded, that the image is Potluck's. An image is removable only when the
record says a share pulled it, every home it names had its daemon read, no home's latest share uses it (a retry there
lends it, or that share is still running; each program built on Potluck has its own pins, so this one's say nothing of
another's), and Tart's list holds no tag of its repository: `tart clone
ghcr.io/cirruslabs/macos-tahoe-base:latest mine` leaves a tag and its digest, and Tart drops a tag whose digest is
deleted. An image Potluck found already there, or pulled before it kept the record (the Tahoe image a failed setup
pulled on a macOS 15 Mac before this record existed), and that passes every other check is retained and listed as
unclaimed, with the command that removes it: `potluck host images --remove <image>`.

While any daemon home the record names is not running, no image is removable: its sizes may lend one, even an image a
share there used before the record began. A daemon home whose shares all ran on a build older than the record is not
in it: while that daemon runs, what it shares is read, but a stopped one's sizes are not, so an image it lends can look
removable or unclaimed. Start every Potluck daemon on the Mac before a removal. A recorded home that no longer exists
(an uninstalled program's) still keeps every image, since an unmounted disk comes back, and the listing names the
command that forgets it. If it is gone for good, `potluck host images --forget-home <dir>` takes it out of the record:
its latest share and its mark on each image, which it lists. It deletes nothing (no image, no VM, no file), and an image
only that home used keeps Potluck's claim and is judged again by the homes still here. It refuses, changing nothing, a
home a running daemon uses (by its machine record, read first), and, under the record's lock, a home that exists (a
link to nothing counts as there), one whose existence cannot be read, one whose own directory is missing too or that is
a disk's mount point (`/Volumes/<disk>`: an unmounted disk comes back), and one the record does not name.

It changes nothing and starts no daemon. `--remove` runs `tart delete` on the removable images only, reading
everything again just before each delete (an image a size began lending meanwhile is kept, and says so), then reads
Tart's list again and says for each whether it is gone, was already gone, or was not removed and what Tart said
(then it exits 1), with the free disk before and after. With image names it deletes exactly those, each removable or
unclaimed (naming one says it is not yours; every other check still holds, again just before its delete); when any
named image is neither, it deletes nothing and exits 1 saying why. Each last read and its delete hold the record's
lock, which a share takes when it starts, so a share that starts meanwhile finds the image gone and pulls it. Tart may drop its own leftovers on a delete (a tag whose
download was already gone); those are listed as also gone. An image Tart's list no longer holds loses its claim in
the record when a removal or a share next reads that list (the homes that used it stay). A second run removes nothing
more. When any list of what this machine shares cannot be read (a daemon running here does not answer, or one pool's
list fails), no image is removable; when its macOS version cannot be read, it refuses.

The Cirrus Xcode image does not include Chrome. Workflows needing Chrome must
install it until a Potluck Mac image ships it. Real-Mac acceptance is the S4 runner
probe and worm native on a Mac lent with `--with xcode`, without `--image` or
`--runner-dir`; the farm tests do not replace those runs.

### Templates

A template is a named box image for coding agents, one of Potluck's own (`computeapi.BoxTemplateImage`). Today:
`agent`, the image pinned in `imagepins/images.pin` on its `template=agent` line, built from
`cmd/potluck-driver-tart/images/agent`.

- It is built FROM the default Linux image, so it keeps the runner and the box tools, and adds git, gh, Node 22 with
  npm and corepack, Go 1.27, Python 3 with pip and venv, ripgrep, jq, build-essential and the Claude Code and Codex CLIs.
- Every version is pinned; the Node, gh and Go tarballs also by SHA-256, the npm CLIs by version only, with no integrity
  pin.
- `images.pin` pins it on its own line, `oci linux <pin or -> runner_dir=/home/runner template=agent`. A line with
  `template=NAME` (lowercase letters, digits and `-`; one per layer, OS and name, beside the one plain line) is never
  a layer's default, and its name must be one the API knows (a test fails otherwise).
- `box-images.yml` builds, checks (each platform runs the box tools and `claude`, `codex`, `node`, `git`, `gh`,
  `python3 --version`) and pins it with the default, rewriting only the image of each line.
- `potluck host` without `--image` lends the default first and each published template image after it (one binding,
  its `images` list). An unpublished one, or one whose pull fails (a private package, a full disk, the network), is
  left out of the binding and its conformance, and the result says so. Only the default failing to pull stops it.

The `browser` template adds sandboxed Chromium, Playwright and PNG screenshots to
its built agent image. Its own immutable pin selects the complete published
image; see [browser testing](browser-sandbox.md). It has no Docker capability
requirement. The selected runtime must pass the browser and isolation checks
with Chromium sandboxing enabled.

Using a template (`potluck sandbox create --template agent`) is described in `docs/daemon.md`.

## GitHub runners on a box

### runner_dir

A box serves GitHub Actions jobs (capability `github-jit`, so the runner broker can place a job on it) only with
`runner_dir`:

- `potluck host` binds it from the image's pin (`runner_dir=PATH` on its `images.pin` line: `actions-runner` for
  Cirrus Labs' macOS base, `/home/runner` for Potluck's Linux image; `box-images.yml` rewrites only the digest),
  or from `--runner-dir PATH` for an image Potluck does not pin (`none`: no runner).
- It never guesses: an unknown image without the flag binds no runner, and the result says to pass it.
- A re-run computes the same `runner_dir` from the pin or the flag, so a re-run with the same flags keeps it (a
  hand-edited one is replaced).
- On oci a runner must reach GitHub; the network side of this is in `docs/network.md`.

### How the runner guard works

`jit_guard` (`docs/protocol.md`, "GitHub runners") says which jobs a runner serves. `potluck-driver-tart` enforces it
like this:

- The runner starts with a job-started hook (`ACTIONS_RUNNER_HOOK_JOB_STARTED`, a JavaScript file outside the
  runner's directory, which the runner runs with its own node) that stops a job the guard refuses.
- The values it checks are the ones the hook's environment has (actions/runner: the `github` context's allowlisted
  keys, the payload written before the hook runs; no head repository, visibility or labels).
- The runner runs the hook after "Set up job" and before any other step, and waits for it.
- A hook's non-zero exit only marks the job failed (its `if: always()` steps and actions' `pre:` scripts would still
  run). So on a refusal the hook SIGKILLs its parent first (`Runner.Worker`, which starts the hook's node itself and
  waits for it), which leaves no step to start; then its other ancestors up to `run.sh`, else `run.sh`'s process
  group (found through `/proc`, else `ps`); then, on a VM layer (Tart, Incus), powers the guest off where it may (as
  root or through `sudo -n`; the box then ends `lost`).
- An image where the launch cannot find its process group (no `/proc` and no `ps -p`), or whose runner `.env` names
  a hook of its own, fails the `acquire`.
- What the hook cannot do: when it runs, the runner already holds the job's message (its secrets and token among
  it), and "Set up job" has downloaded the job's actions. No step of a refused job runs, and the hook's own
  environment holds no secret.

- With `jit_guard.run` (a job the secrets door serves, `docs/daemon.md` "Job grants"), the hook also compares
  `GITHUB_RUN_ID`, `GITHUB_WORKFLOW_REF` and `GITHUB_REF` with the run's, then waits (polling every 250 ms, at most
  `hold_s`) for `/dev/shm/potluck-secrets/POTLUCK_READY` to hold the run's id. A job of another run, workflow or
  ref, or one whose marker never comes, is stopped as above. A guest that keeps no box secrets (macOS) gets no
  `github-jit-guard-run` and refuses a run guard.

- With `jit_guard.open_if_private`, a job the events refuse is served when the event payload
  (`GITHUB_EVENT_PATH`) says `repository.private` is true and `repository.visibility` is `"private"`; anything else
  stops it as above. A server whose payloads leave out `visibility` (an old GitHub Enterprise Server) therefore stops every such
  job.

The daemon sends `jit_guard` only to a driver listing `github-jit-guard` (else `bad_request`),
`jit_guard.open_if_private` only to one listing `github-jit-guard-private`, a `jit_guard.run`
only to one listing `github-jit-guard-run` and `box-secrets`, and reusing a `request_id` with another `jit_guard`
is a different request.

## Admission controllers

On a host that runs other work beside Potluck, every VM lent first takes a lease from the host's admission
controller. The checks `potluck host` runs before it binds do not: the owner is running them, and joining comes
first.

### The controller's API

- `POST /leases {vcpu, memory_mib, holder, ttl_s}` answers `201 {lease, expires_at}`, or `409 {reason, retry_s}`
  (the acquire is then `unavailable`).
- `POST /leases/{lease}/renew` renews it; `404` or `410` ends the box as `reclaimed`.
- `DELETE` gives it back.
- The lease is short and kept by the box's supervisor.

### Binding the keys

`potluck host` binds `potluck-driver-tart`'s admission keys on every size from:

- `--admission-url URL` (http or https, no credentials or query);
- `--admission-token-file PATH` (a `0600` file, bound as an absolute path);
- `--admission-ttl-s N` (120 to 3600: `host` binds no `boot_timeout_s`, so the driver's 90 s plus 30).

They are refused as `potluck-driver-tart` refuses them, before anything runs (`Precheck` included). Conformance runs
without them, so its check boxes ask the controller for nothing and a controller with no room cannot hold up or fail
the join; the bindings carry them, so every box lent takes a lease. A controller that counts the containers or VMs it
sees still counts the check boxes.

### Re-runs

- Every run reads the bindings it replaces, in the one scope it binds in.
- A re-run without `--admission-url` keeps the keys of those earlier box bindings `<prefix>-<size>` (bindings that
  disagree are refused until the owner names one), each flag given replacing its own key.
- `--admission-url URL` takes the flags alone, and `--admission-url none` removes them.
- Fail closed: unless `--admission-url none`, a run whose read of those bindings failed, or showed one without its
  config (a non-owner's view), is refused (`could not read this machine's bindings (...)`). Without the flag it would
  drop their keys; with a URL a keyless one could stay live beside the new ones. A dry run reads them only from a
  daemon already running, and notes when it could not.
- When a run with admission keys (the first switch-on included) leaves a size unbound whose earlier binding in that
  same scope has none, `host` unbinds exactly that binding, since its boxes take room the controller does not count,
  and says so. (Also when no size passed and nothing binds, but not for an invite code, which has not joined yet.)
  When that fails, the size's error says it is still bound WITHOUT admission.
- When no size passes on a run for a pool, `host` reads that pool's bindings back after that unbind and says which
  earlier sizes still lend (`nothing changed: the earlier sizes still lend to pool <pool> (...)`), naming the flags
  given that shape the share (`--docker`, `--sizes`, ...) as not having taken effect; with none left it says this
  machine shares nothing with the pool. A run with an invite code says the machine did not join and the invite is
  not spent.

## potluck-driver-tart

`potluck-driver-tart` (`cmd/potluck-driver-tart`, `vmhost/`; its setup guide is `cmd/potluck-driver-tart/README.md`)
passes `potluck-conformance --remote`.

### Layers

- `vm=tart`: throwaway Tart VMs on a Mac (Softnet, no host mounts, APFS clones deleted after every run).
- `vm=incus`: each run a KVM VM in an Incus project reached only through a client certificate restricted to that
  project (whose limits must be set: the owner's share), never the admin socket. Its expanded config is checked
  before boot for host paths, extra devices, `raw.*` keys and NICs without port isolation and an ACL, or whose ACLs
  drop or reject no outbound mail (tcp 25, 465, 587 to anywhere).
- `vm=oci` on a Linux box: each run a container under a runtime that is a VM or sandboxed kernel, Kata or gVisor,
  never plain runc, with no network unless the owner names a filtered one.
- `network=nat`, on any layer, turns the isolated network off and so describes `isolation: "none"`: refused as a box,
  an unisolated run.

Network details for each layer are in `docs/network.md`. Adding a layer is in `docs/vm-layers.md`.

### Leases and reclaim

- It has a reclaim policy for a lent machine (owner HID idle time, a schedule window).
- Its `expires_at` counts from the script's start, since boot is part of acquire.

### Background processes and terminals

- One file serves Tart, Incus and OCI: a detached holder per process runs it through the layer's exec channel and
  writes its output and exit to host files under the state directory (`boxproc`, shared with the reference driver).
  `output` reads those files without asking the guest, and `signal` is a short guest exec on the group id the guest
  wrapper wrote.
- For a terminal the holder runs the command through the layer's terminal exec (`tart exec -i -t`, `docker exec -i
  -t`, `incus exec --force-interactive`) on a host pseudo-terminal it owns, whose size the layer carries into the
  guest.
- `input` hands the holder frames of at most 512 bytes (PIPE_BUF, so each is whole) through a FIFO in the process's
  directory.
- A waiting `output` on a terminal looks for new bytes every 10 ms, any other every 50 ms.

### Kept disks

Layers Tart and OCI; Incus not yet.

- A stop is a `sync` in the guest, then a power-off that keeps the VM (Tart: the VM's own disk; OCI: the same
  container, stopped, with its `HOME` also on a named volume of its own, `<vm>-home`, mounted at the image's `HOME`
  read from its config).
- How long a stopped disk is kept is `persist_keep_s`: 30 days by default, the owner's to lower or raise.
- gVisor's runsc keeps a container's root changes in an overlay of its own by default (`overlay2` `root:self`;
  `root:memory` alike), which a stop drops. Measured on runsc release-20261102.0 under docker 29.8: files written in
  `/etc`, `/usr/local`, `/var/tmp` and `HOME` were all gone after `docker stop` and `docker start`.
- So a kept runsc box is created with the OCI annotation `dev.gvisor.flag.overlay2=none` (Docker's
  `--annotation`; no host config changes), which sends the root's writes through the gofer to the container's own
  writable layer. The same measurement with it kept every file, and so did the driver's own create, stop and start.
- Kata keeps the root as it is.
- runsc takes the flag only where its override allowlist has it. A runsc that refuses the annotation does not fall
  back to `HOME` alone: it fails the kept box's boot, and the driver answers that failure naming the annotation and
  that runsc's version.
- Bounded Kata profiles do not offer persistent restart; a new parent cannot be assigned to a kept Docker container.
- Its supervisor's guard powers the VM off, never deletes it, when the supervisor goes.
- `start` waits for the last supervisor to be done with the VM, then a new supervisor boots it without a clone or a
  checkout.
- At most `persist_max` persistent boxes (stopped or not; default 4; 0 turns persistence off) live in a state
  directory.
- A persist acquire or a `start` is `unavailable` while the layer's disk directory (docker's root dir, podman's graph
  root, `$TART_HOME`) has less free space than `persist_min_free_gb` (default 10; 0: no floor), since kept disks
  live for weeks.
- A stopped box frees its slot. `start` without a free slot, or while the owner's reclaim policy says no, is
  `unavailable` (the disk stays kept).
- A reclaim stops a running persistent box at once (no `sync`) and leaves a stopped disk where it is: it holds no
  CPU or memory.
- The driver deletes a disk past its time on its next call (every verb sweeps; the daemon's own sweep asks `status`
  at `expires_at`, so a bound binding's disks go on time). A binding the owner removed leaves its state directory's
  kept disks until the driver next runs on that directory (the daemon records such boxes `lost`).
- A supervisor marks itself alive every 5 s, so a box whose supervisor died (the host went down) is stopped by the
  next sweep with its time up counted to that last mark, never through the downtime.
- Not yet: kept disks counted by bytes against the host's free disk (they are counted by number), Incus.

### Named volumes

`vm=oci` today; Tart and Incus list no `volumes`.

- A volume is a named volume of the container CLI's, as a kept box's `HOME` is (the runtime's own mount: gVisor's
  gofer, Kata's virtiofs; nothing else of the host's), `volume-nocopy`. `ro` adds `readonly`.
- `cow` is a volume of the box's own: the local driver's `type=overlay` with the named volume as its lower directory
  and two more of the box's as upper and work, all three deleted with the box (rootful Docker; another CLI may refuse
  the overlay, and the box then fails to start).
- Once the guest answers, the image's user is given each writable mount's directory and the directories made for it
  under `HOME` where they are root's.
- A host serving one binding to several projects keys `volume_owner` by the relaying project, as it keys
  `request_id`, so no project names another project's holder.
- A volume lives on the host and binding where it was made, so placement pins to them.

Quotas:

- Bytes are measured on the host, never in the guest.
- An owner's volumes on a binding are capped at `volume_max_gb` (default 20; 0 turns volumes off; `volume_max_mb`,
  when set, is a finer cap).
  Dependency layers (`potluck sandbox create --deps`, `docs/daemon.md`) are the owner's volumes and count against
  this cap, as does each live box's cow copy of a layer (what the box writes over it, its checkout and build output
  included). A different lockfile, path or `--deps-scripts` setting is a different layer. A host that
  lends boxes to an owner running many at once should raise `volume_max_gb`: a host serving a busy CI pool's builds
  wants 60. Past the cap, only the owner's own least recently used layers and volumes that no box holds are deleted
  to make room (a layer is then built again by the next acquire that asks); else the acquire is `unavailable`
  ("volume quota").
- An owner's project shares a cap too, and the binding has one: acquire's `volume_project` (the daemon sets it; a
  host relaying for another pool keys it by that project), `volume_project_max_gb` (default 4x the owner's cap) and
  `volume_binding_max_gb` (default 8x). So one project minting owners cannot fill the host.
- Every acquire that writes (`rw`, `cow`, or a new volume) keeps all three: volumes (one not measured yet counts as a
  quarter of the owner's cap, at most 1 GiB), live boxes' `cow` copies and the new ones. Past one, only the owner's
  own least recently used volumes that no box holds, and that this acquire does not mount, are deleted, never
  another owner's. Else it is `unavailable` ("volume quota").
- While a box writes a volume or a copy, its supervisor measures exactly its own (`rw` volumes, `cow` upper and
  work) every `volume_check_s` (default 60), outside the driver's state lock (taken only to record and sum). It ends
  the box `reclaimed` ("volume quota: ...") once a cap is passed, the host is below `persist_min_free_gb`, or three
  measurements in a row failed. A failed one still checks the floor, and counts the box's volumes and copies as the
  whole cap until one succeeds; a helper past its time is removed by name.
- A `rw` box is measured again when it ends; one whose supervisor died is measured by a detached re-run of the driver
  the sweep starts, never under the lock.
- On OCI a measurement is one throwaway helper container from the box's image under the binding's runtime (no
  network, read-only root, every capability dropped but `DAC_READ_SEARCH`, the volumes read-only) running `du -sxb`:
  0.22 s on a small Linux host.
- No volume is deleted while a container mounts it or a `cow` overlay (labelled `dev.potluck.lower`, formerly
  `dev.zm.lower`) is layered on it, whatever the records say.
- `rw` and `cow` mounts and new volumes keep the host's free-disk floor (`persist_min_free_gb`). A volume no box named
  for `volume_keep_s` (default 30 days) is deleted by the sweep, as kept disks are.

Measured on a small Linux host (runsc release-20261102.0, docker 29.8): create-to-ready with one `rw` volume 0.95 s,
`ro` 0.68 s, `cow` 1.1 s, against 0.5 to 0.58 s for a box without.

### Warm boxes

- A box binding's `warm` (default 0) is how many booted, idle boxes it keeps; describe's `warm` adds `booting`.
- A warm box serves an acquire of the default image, the binding's own network, without `persist`, `volumes` or a
  `jit_config` (a `commit` is checked out at the take), and lives `ttl_max_s` before a fresh one replaces it.
- It holds one of the binding's slots, so warm boxes never add to what the binding runs. A cold acquire that finds no
  slot free ends a warm box (one still booting, else the oldest) and takes its slot, so warm boxes are the first
  reclaimed.
- Lowering `warm` ends the extra ones on the next call. The owner's reclaim policy ends them as any box, and none is
  made while it says no.
- With `admission_url` a warm box holds no lease until it is taken: its supervisor asks for room at the take. A
  refusal ends it and answers the acquire `unavailable`, as a cold acquire would, and the pool does not refill for a
  minute after (no churn into a full host). So a warm box never holds the host controller's room, and costs the host
  idle only its runtime's own memory (gVisor: about 40 MB host RSS, 9 MiB in its cgroup, measured on a small Linux
  host).
- Measured there (driver alone, gVisor, the agent image): acquire 0.15 to 0.18 s from a warm box against 0.49 to
  0.58 s cold. Through the daemon on a larger Linux host a cold box is 0.4 to 0.55 s create-to-ready, so `warm` is
  for slower layers (a Tart VM's boot).

### Root and Docker in a box

So an agent can install packages and run containers: root inside and containers inside, no access to the host.

Keys and flags:

- They are an oci binding's `root=on` and `docker=on` (docker implies root). A Tart or Incus box is a full VM whose
  image decides both, and the keys are refused there.
- `potluck host --docker` binds them. A re-run without the flag keeps what an earlier run bound for those bindings,
  as admission keys are kept (`--docker=false` turns it off).
- Sudo is opt-in: a default box keeps every flag it had and has no root.

Why a second runtime:

- Measured on runsc release-20261102.0 under docker 29 with the Linux image: `sudo` is setuid, and a default box has
  neither. `no-new-privileges` stops setuid, and gVisor ignores setuid bits unless runsc runs with `--allow-suid`.
- So `root=on` drops `no-new-privileges`, also drops `MKNOD` (and `NET_RAW` with no network), and needs a runtime that
  honours setuid: Kata (a guest kernel), or `runsc-root`, a second docker runtime entry, the same runsc with
  `--allow-suid` and nothing else.

Registering `runsc-root` (`potluck host --docker`, once, beside `runsc`):

- It copies `/etc/docker/daemon.json` to `daemon.json.zm-bak-<date and time>` (its own backup; gVisor's
  `daemon.json~` is rewritten by every `runsc install`).
- It runs `runsc install --runtime=runsc-root --clobber --download-sidecars=NEVER -- --allow-suid` with the binary
  docker's `runsc` entry runs.
- It reloads docker (`systemctl reload docker`, else SIGHUP to `/var/run/docker.pid`; running containers keep
  running).
- One new key: `runsc`, which every default box and any container of the owner's own uses, is not changed, and
  doctor's root line leaves the step out.

Checking `runsc-root`:

- It counts only as Potluck makes it. `doctor` and the driver (before every box with root, and every start of a kept
  one: docker resolves the name each time) decode docker's runtimes and require the path of docker's `runsc` entry,
  both resolved (PATH, symlinks; else a binary named `runsc`), and `runtimeArgs` exactly `["--allow-suid"]`.
- A hand-made or stale entry (runc, or runsc with `--net-raw` or `--network=host`) is a problem whose root step
  replaces it, and no box with root is made on it.

What root gets:

- Nothing else changes: Docker's default capability set with `NET_RAW` dropped on a network, no `--cap-add`, no
  device, no host mount.
- Root holds the container's bounding set and no more. Under gVisor every capability is the Sentry's, never one of
  the host kernel's (runsc's processes hold none on the host), so `--allow-suid` changes only which in-sandbox process
  holds the box's own capabilities. That is why the bounding set's `NET_RAW` drop is load-bearing, and conformance
  checks it.
- A kept box keeps the root and docker it was made with (its container is reused at `start`). So a `start` under a
  binding whose root or docker now differ is `conflict` (its disk stays kept until `release` or its time; never
  recreated, which would lose the renter's disk).

Docker inside is rootless:

- `zm-dockerd` (in the Linux image), started by PID 1 when docker is on, runs dockerd in a user namespace of the box
  user's (rootlesskit; `newuidmap` is the setuid part) on a slirp4netns user-mode network. So containers' traffic
  leaves as that user's ordinary sockets through the box's own filtered network.
- Every exec gets `DOCKER_HOST`, and sudo keeps it.
- Measured and refused: a rootful dockerd needs `NET_ADMIN`, and with `NET_ADMIN` root in a gVisor box re-addressed
  its `eth0` (a neighbour's address, a new MAC) and answered for it on the shared bridge. Docker's bridge networking
  inside gVisor (gVisor's docker-in-gvisor guide) needs runsc `--net-raw` and `--allow-packet-socket-write`, which
  let a box forge frames onto that bridge.
- Under gVisor bridge networking inside stays off (per-port anti-spoofing first). So inside a gVisor box `docker run` and
  `docker build` have no network unless given `--network host` (the slirp stack: the internet, through the box's
  filter); there is no container-to-container bridge; `-p` publishes nothing outside the box; and storage is `vfs`
  under `HOME` (overlay inside gVisor needs a memory-backed upper), so large images are slow to unpack. A kept box
  keeps its images.

Conformance group `root`:

- A driver listing neither `root` nor `docker` must not give sudo (`sudo -n true` refused where sudo exists).
- One listing `root`, for every image the binding lends (config `images`), on boxes acquired with `net` `on` where
  the driver lists `net-off`: `sudo -n id -u` answers `0`, and root's `CapBnd` lacks `NET_ADMIN` (bit 12), `NET_RAW`
  (bit 13) and `MKNOD` (bit 27).
- With `docker`: a container runs from an image imported from the box's `docker-init`, so no network or registry is
  needed.
- `potluck host` runs it in its quick groups.

On Kata, `docker=on` selects ordinary rootless Docker bridge networking: user-defined networks, service DNS,
NAT, normal `docker build`, and ports published into the box. It adds no outer capability, host device or host
mount. RootlessKit creates its own user and network namespaces; traffic still leaves through the outer box's
filtered network. Storage remains `vfs`. Lease release destroys the outer box and all nested services with it.
The service mode needs the updated box image (including `iptables`). See
[Docker service setup and acceptance](docker-services.md).

Kata runtime acceptance is still required: the guest must provide `/dev/net/tun`, user namespaces and working
namespaced netfilter. These have not been measured on a Potluck Kata producer. Do not advertise Docker service
networking from a host until its real acceptance passes. Neither fake runtime tests nor a runtime named Kata
prove those prerequisites. gVisor remains the measured no-bridge option.

Not yet: `podman` and `nerdctl` (no `runsc-root`), a bridge network inside a gVisor box, Docker on Tart (a macOS
guest has none).

## potluck-driver-local

`cmd/potluck-driver-local` (`internal/localdriver`; its own `README.md` has config and states): a run on the
machine whose daemon calls it, as that daemon's user, for credentialed jobs that need that user's files.

- No task and no session, so any socket caller's acquire works.
- Class `isolation: "none"`, `credentials: true`.
- Config: `remote` (the binding's repo as a git URL; required), `state_dir` (default `~/.potluck-driver-local`, or
  `~/.zm-run-local` where that exists; one directory per binding), `ttl_max_s`, `default_ttl_s`.
- A bare mirror of `remote` fetches `ref` and refuses a `commit` that ref's history there lacks (`forbidden`).
- The run is a detached checkout of exactly `commit`, the script an executable file in it, run by a detached
  supervisor with the environment allowlist, stdin closed and its output in `log`.
- The supervisor enforces the deadline (`exit: 124`) and removes the checkout. A supervisor gone without a verdict
  makes the run `lost`. Liveness is a lock per run, never a bare PID.
- No `renew`; spend is not reported.

Native Kata resource profiles and live engine verification are documented in [OCI host budgets](oci-host-budget.md). Detection of an installed runtime is preliminary; allocation verifies the active local engine and program identity.

Bounded Kata profiles do not offer outer named volumes or the outer tool-cache mount: measurement requires a separately admitted helper before that capability can be enabled. This limitation does not restrict Docker volumes inside the ephemeral guest.
