# Drivers

A driver is a separate program, `potluck-driver-<name>` (old name `zm-run-<name>`), that the daemon starts with one
JSON request on stdin (`docs/protocol.md`). It makes and ends the machines, VMs and jobs that renters use.

## What a driver can do to you

- It runs as you: the daemon's user, with your files, keys and network.
- It sees renters' secrets: the daemon hands it what a sandbox or job needs.
- Nothing in Potluck limits what it does with either. Run only drivers you trust as much as you trust Potluck.

## Where the daemon finds a driver

Only in these directories, in this order, never on PATH:

1. the directory of the running daemon program (symlinks resolved), where a release installs its drivers;
2. `$POTLUCK_HOME/drivers` (default `~/.potluck/drivers`; zm's daemon: `$ZM_HOME/drivers`).

In each, `potluck-driver-<name>` is used, else `zm-run-<name>`. A driver name is lowercase letters, digits, `.`, `_`
and `-`, starting with a letter or digit.

The daemon refuses a driver file, its directory or any directory above them whose mode bits make it writable by group
or others, or that is owned by anyone but you or root. A directory above the driver's own may be writable by others
only if it is sticky (like `/tmp`); the driver's own directory never. A driver that is a symlink must itself be
owned by you or root, and both the directory it sits in and the file it leads to (with their directories) are held
to these rules. Fix with `chmod go-w <path>`.

## Pinning

Each driver is pinned in `$POTLUCK_HOME/driver_pins.json`: its absolute path and the sha256 of the file. Before every
start the daemon checks the file against its pin and refuses it, naming the run target, the path, the expected and
found hash and the command to re-pin, if the file changed, moved or is missing.

- **First use.** A driver with no pin is pinned the first time the daemon runs it: when you share a machine (the bind
  asks the driver to describe itself), or on the first call after upgrading from a version without pins. The daemon
  logs one line when it does.
- **After a deliberate upgrade**, pin it again: `potluck driver pin <name>` (it prints the path and sha256 it
  recorded). Removing `driver_pins.json` makes every driver pinned again on its next use.
- **Self-update.** `potluck upgrade` (and `install.sh`, which runs it) re-pins the drivers it placed: `kit/swap.Place`
  records the sha256 of each file it copies (`Placed.Sums`), and `runtarget.RepinPlaced(home, dir, sums)` re-pins a
  driver already pinned in that directory, only to a file the updater placed, and only while that file still has the
  sum it was placed with. A driver changed by anything else stays refused.
- **A missing pins file.** If `driver_pins.json` is missing or holds no pins while run targets exist, the daemon
  logs a warning at start ("driver pins file missing: re-trusting every driver on next use"). That is expected once
  after upgrading from a version without pins; any other time, someone removed or emptied the file.
- **Speed.** A file's hash is cached by its device, inode, size, mtime and ctime, so a call does not hash a 30 MB
  driver again. Replacing a file changes its inode or ctime; only root can set a ctime back.
- **The file that runs is the file checked.** On Linux the daemon runs the driver through the descriptor it hashed
  (`/proc/self/fd/N`), so a file renamed over the path afterwards is not run. A `#!` script driver gets that
  descriptor as its fd 3 and runs as `/proc/self/fd/3`: its `$0` is that path, not its file name. On macOS, which cannot do that, it checks the file's
  identity again just before starting it; a window of microseconds remains, open only to you and root.

## What pinning does not protect against

- A malicious driver you pinned. A pin says the file did not change since you trusted it, not that it deserves trust.
- A driver that runs other programs: only the driver file is pinned (a shell script driver's `exec` target is not).
- Root on the machine, or any program already running as you: it can change the pins file, or do what a driver
  does without one.
- Access control lists. Only mode bits and owners are read. On macOS, access control lists are not read: a
  directory whose ACL lets others write is not detected.
- The conformance command: `potluck-conformance <path>` runs the driver you name and is never pinned. It is how you
  check a driver before you trust it.

## Setups that need a step

- **Homebrew prefixes** (`/usr/local` on some Macs, Linuxbrew) are often writable by the admin group, so drivers
  there are refused. Put drivers in `$POTLUCK_HOME/drivers` instead.
- **Versioned install paths** (a Homebrew Cellar, the Nix store) change on every upgrade, so the pinned path goes
  away: run `potluck driver pin <name>` after upgrading.
- **Ubuntu's default umask 002** makes a directory you create by hand group-writable: `chmod go-w` it (and the
  drivers in it).
- **A driver you build yourself** needs `potluck driver pin <name>` after every rebuild.

## Reviewed drivers

| driver | program (old name) | what it does |
| --- | --- | --- |
| tart | `potluck-driver-tart` (`zm-run-tart`), `cmd/potluck-driver-tart` | each job or sandbox in a throwaway VM: Tart on a Mac, Incus or an OCI runtime on Linux (`vmhost/`) |
| local | `potluck-driver-local` (`zm-run-local`), `cmd/potluck-driver-local` | jobs on this machine, as the daemon's user (`internal/localdriver`) |

An out-of-tree driver is added to this table by pull request, with its report from the conformance suite run with
`--strict` (see `docs/conformance.md`).
