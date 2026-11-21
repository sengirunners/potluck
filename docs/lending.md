# Lending your Mac

For someone who wants to lend a Mac to a pool and is not a programmer. It says what Potluck puts on your Mac,
what the other people in the pool can and cannot do with it, and how to pause, leave, remove a pool you created and
remove everything. It describes Apple-silicon Macs. For a Linux machine, [hosts.md](hosts.md) has the same steps in
more detail.

The facts below were read from the code as it is now; the last section says where each one comes from. Where something
has not been proven, this page says so (see [What is not proven](#what-is-not-proven)).

## What lending means

You lend a share of your Mac, never the whole of it. A person in the pool asks for a sandbox: a fresh, throwaway
virtual Mac or Linux machine that runs inside your Mac. They run commands in it. When it ends, Potluck deletes it. By default Potluck shares at most half of your cores and memory (`--share`, default 0.5).

A sleeping Mac runs nothing. While a sandbox runs or waits warm, Potluck keeps the Mac from idle sleep (the display may still
sleep); with none running it does not, and `potluck doctor` warns when the Mac goes to sleep and suggests
`sudo pmset -a sleep 0` (undo it when you stop lending).

On a Mac, a sandbox ends within about a second of you touching the keyboard or the pointer, and none starts until the
Mac has been idle for five minutes (`--when away`, the default; `--when any` lends all the time).

## What setup puts on your Mac

This is the whole list, in the order things appear.

| Step | What | Where | Why |
| --- | --- | --- | --- |
| Install | The programs `potluck`, `potluck-driver-tart`, `potluck-driver-local`, `potluck-conformance` and `potluck-e2b` | `~/.local/bin` | `potluck` is the command you type. `potluck-driver-tart` makes and ends the virtual machines. The others are for testing and for E2B's tools. After an update each has a `.prev` copy beside it, so an update can be undone. |
| Install | One line in `~/.zshrc` (or the startup file of your shell), after a comment that starts `# Potluck (install.sh)` | your home folder | Only when `~/.local/bin` is not already on your PATH. It lets a new Terminal window find `potluck`. `--no-modify-path` skips it. |
| First command | A folder `~/.potluck` (the "home") | your home folder | Your Potluck identity, the pools you joined with their tokens, the pools you run, the isolation test results, and the daemon's log. |
| First command | The **daemon**: a background program, `potluck daemon` | runs until you stop it or the Mac restarts | It does the work: it talks to the pool and starts and ends sandboxes. Potluck starts it the first time a command needs it. On a Mac it is started through a one-time launchd job that removes its own files; nothing starts it at login or after a restart, and `potluck daemon --detach` starts it again. |
| `potluck create`, `join` or `host` | A Tailscale node in your Tailscale account, named `potluck-` followed by your Mac's host name and `-potluck` | Tailscale's admin console | How the pool's machines reach each other. It is Potluck's own node inside the daemon, separate from the Tailscale app, and the first command prints a login link: you need a Tailscale account to sign in. With `--via tunnel` there is none. |
| `potluck host` | A macOS image, about 27 GB (60 GB free are needed) | Tart's folder, `~/.tart` (or `$TART_HOME`) | Every sandbox starts as a copy of it. A file there, `potluck-pulls.json`, records which images Potluck pulled, so that `potluck host images --remove` deletes only those. |
| `potluck host` | A record of the sandboxes the driver made, and a report for each one that failed to start | `~/.potluck-driver-tart` | The driver's own state: it ends a sandbox at its deadline even when the daemon is down. |
| `potluck host pause` or `potluck host set` | The file `/Users/Shared/Potluck/host-override.json` | outside your home folder | The one switch every part of Potluck reads: open, wind down, closed. The menu bar app writes it too. |

Things Potluck does **not** put on your Mac:

- It does not install Tart or Softnet. You install those yourself (`potluck doctor` says where to get them and
  prints what is missing). It does not install the Tailscale app either; the node above lives in Potluck.
- `install.sh` never asks for your password and never uses `sudo`.
- No login item or LaunchAgent unless you add one. The two optional ones are below.

### The one step that needs your password

Every sandbox's network goes through **Softnet**, a small program Tart starts for each sandbox. It must run as root
(it is "setuid root") to build the network, and it is what keeps a sandbox away from your Mac and your home network.
So there is one step that needs an administrator:

- **macOS 26 or newer:** you install the upstream Softnet release yourself. If it is not set up as root,
  `potluck host` prints the two commands that do it (`chown root` and `chmod u+s,go-w` on the Softnet file) and runs
  them with `sudo`, which asks for your password at the Terminal. Potluck never reads, stores or passes the
  password on. `--no-sudo` prints the line instead and runs nothing.
- **macOS 15:** `potluck softnet install --candidate` downloads Potluck's own build, checks its size and SHA-256
  against a table inside `potluck`, asks for your password, and as root copies it to
  `/Library/Application Support/Potluck/Softnet`, owned by root and setuid. Nothing else outside that folder is
  changed, and another Softnet on your PATH is left alone. That build is a candidate: see
  [What is not proven](#what-is-not-proven). The details are in [softnet.md](softnet.md).
- **macOS 14 and older:** no Softnet that enforces Potluck's rules exists, so the Mac cannot lend.

`potluck host` checks the machine before it lends anything, and `--dry-run` prints the plan and changes nothing.

### Two things you can add

- **A daily update job** (`potluck upgrade enable`): a LaunchAgent, `~/Library/LaunchAgents/sh.potluck.update.plist`,
  that runs `potluck upgrade` at about 03:00. Only signed releases are installed. `potluck upgrade disable` removes it
  ([install.md](install.md)).
- **The menu bar app** (`sh install.sh --menubar`): `Potluck Menu.app` in `~/Applications`, a switch to pause and
  resume lending. You add it to Login Items yourself, in System Settings. It is not signed by an Apple developer
  yet; if macOS refuses to open it, [menubar.md](menubar.md) says what to do.

## What other people can and cannot do

There are three kinds of people: members of the pool, the person who owns the pool (the machine that runs it), and
you.

### The pool's members

They can:

- ask for sandboxes of the sizes you share and run commands in them, copy files in and out of their own sandboxes,
  and read what the commands print;
- use a share of your Mac's cores and memory, up to the sizes you lend, and your internet connection: a sandbox on a
  Mac reaches the internet, so what it does there comes from your IP address (`docs/network.md`);
- see that your machine is in the pool, by its name, and which sizes it lends;
- use at most two macOS sandboxes at a time on one Mac (Apple's licence), and Potluck sets no cap on how much
  they download: a sandbox's traffic and any complaint about it reach your internet connection.

They cannot, by the design and the checks below:

- reach your Mac itself on any port but DNS (the one door Softnet leaves open), your home network (router, NAS, printers), your Tailscale network, or another
  sandbox running at the same time;
- send mail (ports 25, 465 and 587 are blocked);
- read your files: a sandbox is started with no shared folder, window, audio or clipboard
  (`vmhost/tartboot.go`: `--no-graphics --no-audio --no-clipboard`, no `--dir`), and holds none of your credentials;
- use your USB devices (an audio interface, a drive, a phone): Apple's virtualization framework, which Tart uses,
  has no way to hand a Mac's USB device to a virtual machine, and Tart attaches none of its own. Tart's "USB
  accessories" are only a virtual USB keyboard and pointer it makes itself, which no one can type into on a sandbox
  with no window; Potluck turns those off too with `--no-usb-accessories` (`vmhost/tartboot.go`) when your Tart is
  2.38.0 or newer. An older Tart still lends, and `potluck host` warns that the sandbox keeps the virtual USB
  keyboard and pointer;
- keep anything on your Mac for good: a sandbox ends at its deadline and its disk is deleted. The one exception is a
  sandbox made with `--persist`, which keeps its disk while it is stopped. That is allowed only on a machine marked
  trusted. The disk stays until its holder removes the sandbox or, by default, 30 days after it was stopped, and by default at
  most four such sandboxes are kept.

### The person who owns the pool

The owner's machine coordinates the pool. It can:

- invite people and take them out; mark your machine **trusted** (a friend: sandboxes are placed on it
  unasked, and it may run GitHub Actions jobs and receive members' secrets) or **public** (a stranger: only
  sandboxes a member names, never Actions jobs or secrets);
- see which sizes you lend, whether they are free, and your Mac's swap state (the pool gives secrets only to a
  machine whose swap is safe, and a Mac's is);
- read every command, input, output and file a member sends to a sandbox on your Mac, because those calls pass
  through the owner's machine in the clear (`docs/daemon.md`, "Who can read a secret in transit"). Secrets are sealed hop by hop: the
  owner's machine opens them, checks them and seals them again for your Mac, so the owner can read them as they pass
  and your Mac can read them when they arrive.

It cannot run anything on your Mac outside a sandbox: the calls it passes to your Mac are only the sandbox
verbs (making a sandbox, running a command in it, reading its output, sending it input or a signal, copying files in
and out, opening a port on it, stopping, starting and ending it), and your daemon refuses a call for a size it has
not shared with that pool.

### You

You are the one who can see inside a sandbox: your Mac runs the virtual machine, so as the Mac's owner you can read
everything in it, including what members put there. For a **trusted** machine that includes members' secrets, which
is why the pool only sends them to machines it trusts. Do not look.

## What is not proven

Potluck's claim is that a sandbox cannot reach your Mac, your home network, your tailnet or another sandbox
([isolation.md](isolation.md)). The honest state of the proof:

- **No recorded result from a real Mac.** `potluck host` runs the blocking checks on your Mac before it shares a
  size, and `potluck isolation test <size>` runs them again and keeps the result. The results table in
  [isolation.md](isolation.md) lists the first recorded run on a lent Mac as "owed". Run `potluck isolation test <size>`
  yourself after you start lending, and again when your network changes.
- **The check is by sample.** It tries your Mac's addresses, the router, Tailscale's resolver, any host you name
  with `--net-forbid`, and a second sandbox. It does not try every device on your network.
- **On macOS 15, Softnet is a candidate.** Potluck's own build has not passed the isolation checks on a real
  macOS 15 Mac ([softnet.md](softnet.md)). A Mac that installs it still runs every check before it lends.
- **Tests do not start real VMs.** CI covers the Tart driver through a stand-in VM layer, so the checks that need a
  real virtual machine are run by hand. The one-machine path has been run by hand on an Apple-silicon Mac; the
  two-machine path has not.
- **USB is read from source, not tested.** That no USB device of yours reaches a sandbox comes from Tart's source
  (read at 2.38.0 and 2.41.1: `Sources/tart/Platform/Darwin.swift`, `keyboards` and `pointingDevices`, and
  `Sources/tart/VM.swift`, `configureInputDevices`, are the only USB devices it configures; disks it attaches are virtio, not USB) and from Apple's
  framework having no USB passthrough. No one has yet plugged a device into a lending Mac and looked from inside a
  sandbox (`ioreg -p IOUSB` in a macOS sandbox, `lsusb` in a Linux one).
- **Nothing here covers a flaw in Apple's virtualization or in Tart.** Potluck relies on both.

## Pause, leave and remove everything

Look first, and change nothing:

```sh
potluck host status     # what you lend, what runs now, and whether new work starts
```

### Pause

| You want | Type | What happens |
| --- | --- | --- |
| Stop for now, keep the setup | `potluck host pause` (`--for 2h` or `--until 23:00` for a while) | Nothing new starts; what you share stays shared. Refused while a sandbox runs, since it would end it. `potluck host resume` lends again. |
| Wind down | `potluck host set drain` | Nothing new starts; running work finishes (cut after 30 minutes by default). |
| Stop at once | `potluck host set closed` | No new work, and running work is ended now. `potluck host set auto` returns to your own rules. |
| Stop with the menu bar | the switch in Potluck Menu | The same switch file as `host set`. |

Stopping the daemon (`potluck daemon stop`) does not end a sandbox that is already running: each one ends at its
own deadline even with no daemon. Pause or close first.

### Stop lending

```sh
potluck host stop       # remove every size this machine shares
```

A size with a sandbox still running is refused and named: wait for it, or the member who made it removes it.
`--force` removes a size shared with a pool you joined anyway; its running sandboxes then go on until their lease
ends, out of their holders' reach.

### Leave a pool

```sh
potluck remotes         # the pools this machine joined
potluck host stop       # stop sharing first
potluck leave friends   # delete this machine's token and ask the pool's machine to revoke it
```

If the pool's machine does not answer, `leave` says so, and the owner can revoke your token there.
`potluck leave` is for pools you joined.

### Remove a pool you created

```sh
potluck disband friends --dry-run   # what it would do; changes nothing
potluck disband friends             # does it, after you type yes (--yes does not ask)
```

`potluck disband` removes one pool this machine runs (one you made with `potluck create`) and keeps the rest of
Potluck: this machine's identity, its other pools, the pools it joined and its daemon. It prints its plan in
words first and asks. It does, in this order:

1. stops sharing the sizes this machine shares with the pool (a size shared only with it is removed from this
   machine's shared sizes; one shared with other pools stays, for them);
2. cuts off every machine that joined the pool. Each one's token ends, and the next time that machine reaches yours
   (at its next heartbeat, every 15 seconds or so) it is told "the pool's owner removed pool friends": `potluck remotes` there shows the pool as
   `purged` with those words, and `potluck leave friends` forgets it;
3. cancels the invites nobody used yet;
4. removes the pool and everything it holds on this machine: its settings, grants, invites, the records of its
   sandboxes and its usage.

A sandbox still running in the pool, or kept stopped with its disk (`--persist`), stops it, and the answer names how
many: end them first (the member who made one uses `potluck sandbox rm`). The daemon refuses the same way when asked
directly (`DELETE /v0/projects/<pool>`), so a sandbox started after the plan stops it too. With `--force` (`?force=1`)
the pool goes anyway and those sandboxes run until their lease ends, out of their members' reach, as with
`potluck host stop --force`. A pool that
GitHub Actions jobs run on is refused until its GitHub accounts are unlinked (`potluck runners unlink`) and the
runners block is out of `config.json`; no command turns the broker off yet.

A machine that lends a size to the pool keeps that share (it is that machine's own setting) until its owner runs
`potluck host stop` there. If the pool is removed but a step after that fails (a shared size that could not be rewritten, a folder that
stays), `disband` exits with an error that begins "pool friends was removed, but not everything it held" and lists what
is left; running it again cannot finish that, because the pool is gone. A pool you only joined is left, not removed: `disband` says so and names `leave`. The pool's
name is free again afterwards, and a new pool of that name starts empty: nobody from the old one is in it. A GitHub
name (`@login/name`) is the exception: it stays tied to the removed pool, so no pool takes it again.

### Remove everything

```sh
potluck host stop                  # 1. stop lending (uninstall does this too)
potluck host images                # 2. what Tart holds, and what Potluck would remove
potluck host images --remove       #    delete the old images Potluck pulled and no longer lends
potluck uninstall --dry-run        # 3. what it would do, in order; changes nothing
potluck uninstall                  #    does it, after you type yes (--yes does not ask)
```

Step 2 comes before step 3 because it asks the daemon what is shared, and `uninstall` stops the daemon.
`--remove` deletes only images Potluck's own record says it pulled and no longer needs. **It keeps the current macOS
image, the big one (about 27 GB):** Potluck keeps it so a retry does not download it again. To get that space back,
run `tart delete <name>` with the name `potluck host images` prints.

`potluck uninstall` does these, in this order, and stops at the first one that fails (run it again to carry on; every
step is safe to repeat):

1. stops sharing every size your Mac lends (refused while a sandbox runs, unless `--force`);
2. leaves every pool you joined and asks that pool's machine to revoke your token;
3. on a Mac with the managed Softnet, removes it (this is the step that asks for your password);
4. stops the daemon;
5. removes the daily update job, the host override file and the lines `install.sh` added to your shell's startup file
   (they put the install directory, `~/.local/bin` by default, on PATH; if you have put other tools there, add the
   directory back). It removes only the exact line `install.sh` wrote; a line you changed stays. A file it cannot
   write is named, and the run goes on;
6. removes the home folder, `~/.potluck`: this Mac's identity, the pools it runs and everything they hold.
   `--keep-home` keeps it;
7. removes the programs from the folder you ran `potluck` from, with their `.prev` copies, last.

It never starts a daemon. With none running (the usual state after a restart), it skips steps 1 and 2 and says so:
the pools you joined are not told, so start the daemon (`potluck daemon --detach`) and run `uninstall` again, or ask
each pool's owner to revoke your token. A daemon that is there but does not answer, or whose lock on the home is
taken, stops the run before anything is removed. It refuses a home folder that does not hold a registry file (`registry.db`) or
a `program` file naming `potluck`, and your own home folder. It does not remove files it did not make: a file such as
`potluck-mine` beside the programs stays, and a symbolic link to `potluck` is removed without touching where it
leads (it takes the programs from the folder of the `potluck` on your PATH when that is the one running). `potluck softnet uninstall` removes only the managed Softnet.

### What is left, and how to remove it

`potluck uninstall` ends by naming what it did not touch:

- **Tart, its images and any VM it still holds.** Tart is yours, and an image or VM may be too. `tart list` shows
  them; `tart delete <name>` removes one. Sandboxes that were still running when you uninstalled, and kept
  (`--persist`) disks, are Tart VMs, and nothing removes them after the driver is gone. `potluck-pulls.json` in
  Tart's folder (`~/.tart`) is Potluck's record of its pulls and is left too.
- **Softnet, if you set it up yourself** (Homebrew's, for example). Potluck's root step made it setuid root. If you
  do not use Tart for anything else: `sudo chmod u-s <path to softnet>`.
- **The Tailscale node** in your [Tailscale admin console](https://login.tailscale.com/admin/machines), named
  `potluck-` followed by your Mac's host name and `-potluck`.
- **Potluck Menu**, if you installed it: quit it from its menu, remove it from System Settings > General > Login
  Items, and delete the app ([menubar.md](menubar.md)).
- **The Tart driver's records**, `~/.potluck-driver-tart`, and a `--state-dir` you chose. They say which Tart VMs are
  Potluck's sandboxes, so uninstall never removes them: when no sandbox runs, `rm -r ~/.potluck-driver-tart`.

## Where each fact comes from

| Claim | Where |
| --- | --- |
| What install.sh writes | `install.sh`, [install.md](install.md) |
| What `potluck uninstall` does, and its tests | `cli/uninstall.go`, `cli/uninstall_test.go` |
| What `potluck disband` does, and its tests | `cli/pool_remove.go`, `kit/project/remove.go`, `cli/pool_remove_test.go` |
| What Softnet is and why it is root | [softnet.md](softnet.md), `hostprobe/tartmac.go` |
| Sandbox network, no shared folder | [network.md](network.md), [isolation.md](isolation.md), `vmhost/vm.go` |
| Conformance's blocking checks | [conformance.md](conformance.md), [isolation.md](isolation.md) |
| What the pool's machine can read | [daemon.md](daemon.md) |
| Trusted and public machines | [daemon.md](daemon.md), `cli/pool.go` |
| Pause, stop, images | [hosts.md](hosts.md) |
