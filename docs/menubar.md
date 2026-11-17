# The macOS menu bar item

A small red pot in the menu bar for a person lending their Mac. It shows how much of the Mac is lent and in use,
and lets them take it back without a terminal:

```
Lending 6 of 12 cores
6 cores in use by 2 jobs
●●●●●●······
─────────────
✓ Keep lending
  Wind down        finish jobs, take no new ones
  Stop now         end jobs now
```

| item | runs | what happens |
| --- | --- | --- |
| Keep lending | `potluck host set auto` | the sizes' own rules again (schedule, idle time) |
| Wind down | `potluck host set drain` | no new jobs; running jobs finish, and are cut at the drain deadline |
| Stop now | `potluck host set closed` | no new jobs; running jobs end now (it asks first while jobs run) |

Keep lending is checked in `auto` and in `open`. The modes are the host switch's
([cmd/potluck-driver-tart/README.md](../cmd/potluck-driver-tart/README.md#the-host-switch-open-drain-closed-auto)).

A closed switch is a paused Mac (`potluck host pause`, or Stop now): the menu says "Paused: lending nothing" and
Keep lending reads "Resume lending" (it runs the same `host set auto`, as `potluck host resume` does). While a
`potluck host --detach` run goes on, the note says so with its last line; `host status --json` carries it as
`background` (`running`, or `ended` with the last run's last line).

## Where it gets its facts

Every 15 seconds, and when the menu opens (at most every 5), it runs `potluck --json host status`; when that fails it
keeps the last answer and says it is not current. That command reads, without
starting the daemon:

- the sizes this Mac shares (with pools it joined, and in pools it runs), each with its describe and the boxes
  running on it, from the daemon;
- the host switch, as a size's driver reads it (so "Paused by your lending rules" when the schedule or idle rule
  says no), or from its file when no size answers;
- the Mac's cores.

Lent cores are, per driver state directory and OS (which share their slots), the slots times the largest size's
cores. Cores in use are the running boxes' cores; a stopped box with a kept disk holds none. The menu's words,
which item is checked and what a click runs all come from that command (Go package `menubar`, with tests); the
app only draws them.

## Install it

On a Mac, with the release's installer:

```sh
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh -s -- --menubar
open ~/Applications/"Potluck Menu.app"
```

`--menubar` is off unless you ask for it. It installs potluck as usual, then takes `Potluck-Menu.zip` out of the same
signed tarball (its hash is in the signed manifest, and install.sh checks it again), unpacks it into
`~/Applications` (`POTLUCK_MENUBAR_DIR` changes that) and replaces an older copy, asking a running one to quit first.
A release built without the app makes `--menubar` stop after installing potluck and say so. The app is a universal
binary (Apple silicon and Intel), macOS 13 or newer.

**This build is not Developer ID signed or notarized yet.** It is signed ad hoc. Installed this way (curl, not a
browser download) macOS runs it. If you unzip a copy you downloaded in a browser and macOS refuses it, right-click
it in Finder and choose Open once. The installer says "not Developer ID signed" while that is true.

To start it at login, add it in System Settings > General > Login Items. Quit it from its menu. To update the app,
run the installer with `--menubar` again; `potluck upgrade` does not touch it yet.

It runs `$POTLUCK_BIN`; else `potluck` from `~/.local/bin`, `/opt/homebrew/bin`, `/usr/local/bin` or
`~/go/bin`; else the program of the machine's Potluck daemon, from its record
([daemon.md](daemon.md#one-daemon-per-machine)). That program must have potluck's `host status` and `host set`
commands; when it has not, the menu shows the error it prints. The app uses a record only when exactly one, of a
program other than potluck, names a running process of yours, in a directory and file that are yours alone. Unlike
the `potluck` command, it does not ask that daemon's socket whether it answers: a live process is enough.

Setting the switch writes `/Users/Shared/Potluck/host-override.json` as you, so run the app as the user who owns
the host (the one who ran `potluck host`).

## Build it from source

On a Mac with the Xcode command line tools:

```sh
macos/PotluckMenu/build.sh ~/Applications   # writes ~/Applications/Potluck Menu.app (ad hoc signed, this Mac's CPU)
open ~/Applications/"Potluck Menu.app"
```

`POTLUCK_MENU_UNIVERSAL=1` builds both CPUs, as the release does.

## How the release builds it

`.github/workflows/release.yml` builds the app in its own job on a GitHub-hosted Mac (`macos-latest`, never a
self-hosted runner), zips it (`ditto`), and `scripts/release -menubar` puts the zip into each darwin tarball. If that
job fails, the release still ships, without the app.
