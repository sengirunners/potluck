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

## Build and run it (from source)

It is not in the release tarball yet. On a Mac with the Xcode command line tools:

```sh
macos/PotluckMenu/build.sh ~/Applications   # writes ~/Applications/Potluck Menu.app (ad hoc signed)
open ~/Applications/"Potluck Menu.app"
```

It looks for `potluck` in `$POTLUCK_BIN`, then `~/.local/bin`, `/opt/homebrew/bin`, `/usr/local/bin` and
`~/go/bin`. To start it at login, add it in System Settings > General > Login Items. Quit it from its menu.

Setting the switch writes `/Users/Shared/Potluck/host-override.json` as you, so run the app as the user who owns
the host (the one who ran `potluck host`).
