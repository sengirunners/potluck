# The macOS menu bar item

A small red pot in the menu bar for a person lending their Mac. It shows how much of the Mac is lent and in use,
and lets them take it back without a terminal:

```
Lending 6 of 12 cores
6 cores in use by 2 jobs
●●●●●●······
Memory pressure normal; Potluck's jobs may use up to 12 GB of 48 GB (25%); swap 1.2 GB used of 2 GB
─────────────
✓ Keep lending
  Wind down        finish jobs, take no new ones
  Stop now         end jobs now
─────────────
  Cores lent: all shared, 6 cores                 ▸
  Memory lent: all shared, 12 GB                  ▸
  Disk kept free for you: none set (300 GB free)  ▸
─────────────
  Set up and check…
  Jobs this Mac ran…
  Uninstall Potluck…
```

| item | runs | what happens |
| --- | --- | --- |
| Keep lending | `potluck host set auto` | the sizes' own rules again (schedule, idle time) |
| Wind down | `potluck host set drain` | no new jobs; running jobs finish, and are cut at the drain deadline |
| Stop now | `potluck host set closed` | no new jobs; running jobs end now (it asks first while jobs run) |

Keep lending is checked in `auto` and in `open`. The modes are the host switch's
([cmd/potluck-driver-tart/README.md](../cmd/potluck-driver-tart/README.md#the-host-switch-open-drain-closed-auto)).

While a host run sets this Mac up and nothing is shared yet, the title says the stage, "Setting up: images (2 of
5)", and the note its last words; a run that stopped says "Setup blocked at validation (3 of 5)" with why and the
command that runs it again. Once sizes are shared, a later run that is blocked is a note, "the last setup run was blocked at
preflight (1 of 5); what was shared before still lends", since what was shared still lends. After a flag this Mac's layer does not take, the command it gives leaves that flag out ([hosts.md](hosts.md#stages-and-the-setup-checklist)). These are the stages `potluck host` prints and `potluck host status` lists
([hosts.md](hosts.md#stages-and-the-setup-checklist)).

A closed switch is a paused Mac (`potluck host pause`, or Stop now): the menu says "Paused: lending nothing" and
Keep lending reads "Resume lending" (it runs the same `host set auto`, as `potluck host resume` does). While a
`potluck host --detach` run goes on, the note says so with its last line; `host status --json` carries it as
`background` (`running`, or `ended` with the last run's last line).

A size that lends other images than the daemon's build pins (the daemon moves it after an update,
[hosts.md](hosts.md#after-an-update-the-daemon-moves-a-host-to-the-new-pins)) adds a note, "1 size lends other images
than this version pins"; `host status` prints a line per size with why, and `--json` carries them as
`images_behind`.

## Memory, and the dials

The memory line is macOS's own memory pressure (`sysctl kern.memorystatus_vm_pressure_level`: normal, high or
critical), the most memory Potluck's running jobs may use (each running box's size; a box may use less) as a share
of the Mac's, and swap in use (`sysctl vm.swapusage`). At high or critical pressure it says whether lending is part
of it: with Potluck jobs running, "Lending is part of it: turn Memory lent down, or Wind down, to give memory back";
with none, "Lending is not the cause". Go package `hostmem` reads the pressure and swap.

The three dials are submenus; each choice runs `potluck host lend`, and the checked one is in force:

| dial | runs | what happens |
| --- | --- | --- |
| Cores lent | `potluck host lend --cores N` (or `all`) | Potluck's boxes hold at most N cores at once |
| Memory lent | `potluck host lend --memory-gb N` (or `all`) | they hold at most N GB of memory at once |
| Disk kept free for you | `potluck host lend --keep-free-gb N` (or `0`) | no box starts, and no kept disk, volume or shelf grows, while the disk has less than N GB free |

The cores and memory choices go up in steps of the smallest size shared, up to all that was shared; a choice too
small for a larger size says so under it ("too small for size c8m32: its jobs do not start"), and a job of that size
is refused at once rather than left waiting. The disk choices are 10, 25, 50, 100, 200, 500 and 1000 GB, those
smaller than the disk. A dial goes no higher than what `potluck host` shared: lending more needs that again
(`--share`, `--linux-slots`). The disk dial holds boxes whose disks are on this machine (Tart and OCI); while the
free disk cannot be read, it holds them too.

A dial takes effect at once, for the next box, with no setup run again and no deadline to choose. Jobs running when
a dial goes down are handled as Wind down handles them, without its deadline: they finish (each within its own time
limit), no new job starts until the ones running are under the dial, and the menu says so ("running jobs hold 8
cores (4 lent now): they finish, and no new job starts until they are under it; Stop now ends them"). Stop now ends
them at once. The title counts what is lent under the dials ("Lending 4 of 12 cores").

The dials live in one file beside the host switch's, `/Users/Shared/Potluck/host-lend.json` (`/etc/potluck` on
Linux), with the same rules: it and its directory belong to the Mac's owner and no one else may write them. Every
size's driver reads it when it starts a box (Go package `vmhost`, `lend.go`); a driver that cannot read it starts
nothing and says why, and so does the menu. A dial bounds each driver state directory (and OS) on its own; the
sizes `potluck host` shares on a Mac all use one, and when there are more the dial's title says "each of N groups
of sizes".

A driver that obeys the dials says so: it lists the capability `host-lend` in `describe`
([protocol.md](protocol.md#describe)). A driver from before the dials lists none and ignores the file, which happens
where the daemon is another program's (zm's, say) built on an older Potluck: it runs that program's drivers, not the
ones `potluck` was installed with. Then `potluck host lend` still writes the dials, and it, `host status` and the
menu's note say which sizes they do not limit ("the dials (cores, memory and disk kept free) limit no size this
machine shares (...)"); `host status --json` lists them as `lending.dials_ignored_by`. They hold those sizes once
the daemon runs a driver that lists `host-lend`.

## Set up and check

"Set up and check…" opens a window of what `potluck --json host status --probe` says about setting up:
the title, the one next step, the setup checklist (each check's mark, its words and the command it names), the five
stages of the last host run (preflight, images, validation, join, usable capacity: a mark and the stage's last words,
a dot for one not reached) and, when a `host --detach` run has gone on, the last line it wrote (`background.last`, also for one that ended). The window runs that status
when it opens and again every 3 seconds while it is open, and keeps the last good picture, with a red "not current"
line, when an ask fails. The commands under each check are for a terminal. Each check that has a command also
carries `do`, what the app may run (potluck's arguments, a page to open, or an act of the app's own), and `next_id`
names the check the next step is from, or a blocked check with nothing to run
([hosts.md](hosts.md#stages-and-the-setup-checklist)); the app reads those, never the words. `--probe` puts this
Mac's own checks first: Apple silicon, macOS and virtualization (`mac`), Tart, Softnet and the free disk where Tart
keeps images. The menu's own 15-second ask leaves them out, since reading them runs Tart and Softnet.

The window has one button: the `do` of the `next_id` check, with its label. A `do` with `url` opens that page in the
browser (only an `https` page, or the `file://` URL of an installed app). A `do` with `args` runs potluck with them;
when it has `invite`, an invite field shows above the button and what is pasted there is added as the last argument,
after the `--` potluck puts in those arguments, so it is never read as a flag. The app runs potluck directly, never
through a shell, writes nothing of the run to a log, and empties the field when the run ends (while it runs, the invite is among potluck's process arguments, as it is when
typed in a terminal). With the field empty
it runs the arguments as they are (`potluck host` then shares with the one pool this machine is already in). A
`do` with only `app` (an act only the app can do) has no button yet, and `auto` is read but not acted on yet.

An action runs with no time limit (the status asks are ended after 25 seconds) and shows "<label>…" while it runs.
If potluck fails, the window shows its error in red (the message and hint of its `--json` error). The button stays
off until a status asked after the action, or after the window opened, is drawn. Closing the window empties the invite field. If the action's `--json` answer holds `admin_step`
(`{"why", "argv"}`, from `--no-sudo`, [softnet.md](softnet.md#without-a-terminal---no-sudo)), the window shows `why` with Continue and Cancel;
Continue runs `argv` as root through macOS's own administrator password dialog (AppleScript `do shell script ...
with administrator privileges`, each word quoted for the shell; the app never sees the password), then runs the
same action once more, with the same program. Cancel, at either question, says what the action's answer (`done`)
says was already done, and "Nothing was changed" only when that list is there and empty; an action that does not
say what it did gets "The admin step did not run". Then potluck's reason again, and
Ask again, which runs that action again with the same arguments (the invite too, held in memory only while that
action is still the next step and the window is open), as often as the person likes. A
failed root step shows its error text. A failure stays until the next action, closing the window, or the next step
changing after the failure was shown.

The code is `Run.swift` (finding and running potluck, and that action rule), `Admin.swift` (the admin step: its
answer, the quoting, the AppleScript) and `SetupWindow.swift`.

The Go side is `menubar.Setup` (`setup` in the status JSON) and `menubar.Background` (`background`); the Swift side
is `StatusModel.swift` (the fields it reads) and `SetupWindow.swift`. The window draws the stages from the
answer's `stage_order` (`hostsetup.Order` with each stage's label), so a new stage needs no Swift change. `go test
./menubar` writes nothing, but checks five sample answers (never set up, setup running, the join waiting for a
tailnet login, blocked at validation, lending) against
`menubar/testdata/status-*.json`; `go test ./menubar -run TestStatusFixtures -update` rewrites them, and
`macos/PotluckMenu/test.sh` compiles the app's decoder structs with `swiftc` and decodes each of them. It also
decodes sample action answers with and without `admin_step` (`macos/PotluckMenu/test/answers`), runs the action rule
against a fake potluck with a stand-in for the password dialog, and checks that an
argv with spaces, quotes, `$`, backticks and a newline reaches a program word for word through the same shell
quoting and AppleScript the admin step uses (without administrator privileges).

## Uninstall Potluck

"Uninstall Potluck…" opens a window of what `potluck --json uninstall --dry-run` answers
([install.md](install.md#removing-it)): the steps in order (`left`), what stays (`not_removed`: Tart's images,
Tailscale and so on), and, in red, what is lost for good (`warning`: this Mac's identity, and the pools it runs,
which end for all their members; when its daemon does not say which those are, the warning says that). Opening it
changes nothing.

Remove… asks for the plan again, then asks for a yes with that plan's warning, and runs the plan's own arguments
(`remove.args`: `potluck --json uninstall --yes --no-sudo`), with the admin step rule above. Return never removes.

- Cancelled at the admin step: the window lists what was already done (lending has stopped, the pools were left)
  and offers Continue removing….
- A failed step: potluck's error, what was done before it, and that the step may be partly done.
- Sandboxes running: potluck's refusal. The app never forces by itself; Remove anyway… shows what forcing does
  (`force`), with the plan's warning as it is then, and asks once more.
- Removed: what was done, what is left for you to remove, and "Move the app to the Trash and quit", which moves only
  this app's own bundle, to the Trash, when clicked. It does not change Login Items.

While a removal runs the app asks potluck nothing else (the 15-second status included), the menu offers nothing, the
window stays open and Quit is refused; it starts once the status asks already under way have ended. A `potluck host`
run going on is stopped first, and none can start while its steps run (one started while the admin step waits is stopped by the run after it). A removal does not start while a setup step or
a menu choice runs. The window also stays open while a question is up. After a removal, the app asks nothing again. `host status` never makes a home to note the app in, so none comes back.

The code is `UninstallWindow.swift`, and `Trash.swift` for the move.

## Jobs this Mac ran

"Jobs this Mac ran…" opens a window that lists every job this Mac's sizes have run, newest first: when it
started, how long it was up, its size (cores and memory), the pool it came from and how it ended (finished,
reached its time limit, ended early to give the Mac back, lost, did not start, or running). Above the list:

- how many are running now, how many started in the last 12 hours, and how many in all;
- how many started on each of the last 14 days;
- an electricity estimate for the running jobs, the last 12 hours and all of them, with how it is worked out.

The estimate is each job's cores × the hours it was up × 5 W a core (about what one busy Apple silicon core draws).
It assumes the job kept its cores busy the whole time, so real use is likely lower, and it leaves out what the Mac
draws anyway. A job whose size keeps the image's own core count has no estimate, and the window says how many those
are. A job counts on the day, and in the 12 hours, it started in.

The window runs `potluck --json host jobs` when it opens and on Refresh (`potluck host jobs` prints the same as
text). That command reads each box size's state directory, from this Mac's bindings file (`run_targets.toml` in the
home) and, when the daemon answers, from the pools this Mac runs; it needs no daemon. A driver keeps a job's record
for a week after it ends; when it forgets the record it writes the job's line (when, how long, size, how it ended,
never what the job was sent or printed: a start that failed keeps only the path of its redacted report) to
`jobs.jsonl` in the state directory, which keeps about 8 MiB (some tens of thousands of jobs) and drops the oldest
half past that. A job whose driver's supervisor is gone (say the Mac restarted) is listed as lost, counted to the
last its files show of it, even before a daemon's sweep records that. The pool's name comes from the host's handle
ledger (`host_handles.json`, which forgets a handle 30 days after a call last used it) and the daemon's list of
pools, which it asks for at most 8 seconds; a job started on this Mac itself says "this Mac", and an older job the
ledger no longer holds says "not known". The words, counts and estimate are Go package `menubar` (`BuildJobs`, with
tests).

## Where it gets its facts

Every 15 seconds, and when the menu opens (at most every 5), it runs `potluck --json host status`; when that fails it
keeps the last answer and says it is not current. That command reads, without
starting the daemon:

- the sizes this Mac shares (with pools it joined, and in pools it runs), each with its describe and the boxes
  running on it, from the daemon;
- the host switch, as a size's driver reads it (so "Paused by your lending rules" when the schedule or idle rule
  says no), or from its file when no size answers;
- on a host that shares its room through an admission controller, the controller's last "no room" answer while it
  stands (until its retry time, or until it grants a lease to the same size's state directory): the menu then says "Refusing new work now" with the
  controller's reason, though the switch says accepting (`refused` in the JSON);
- the Mac's cores and memory, its memory pressure and swap, and the free and total size of the disk its home is on;
- the lender's dials (`host-lend.json`, above), as a size's driver reads them.

Shared cores are, per driver state directory and OS (which share their slots), the slots times the largest size's
cores; lent cores are those under the cores dial. Memory is counted the same way. Cores in use are the running boxes' cores; a stopped box with a kept disk holds none. The menu's words,
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
run the installer with `--menubar` again; `potluck upgrade` and the daily update do not touch it yet.

### What host status knows of the app

`potluck host status` lists the app in its setup checklist. Installed is a `Potluck Menu.app` in the last place it
ran from, `$POTLUCK_MENUBAR_DIR`, `~/Applications` or `/Applications`. Running is only the app's own word: when the
program that runs `host status` is the app (its parent process is `Potluck Menu.app/Contents/MacOS/PotluckMenu`),
host status notes the app and the time in the home's `menubar-seen.json`, and the app counts as running while that
time is under a minute old (it asks every 15 seconds). `open` exiting 0 is no proof that it shows. Potluck cannot
read your Login Items, so the checklist says how to add it there instead of guessing.

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

## Signing and notarizing

`build.sh` signs ad hoc, which runs on the Mac that built the app and nowhere else. `macos/PotluckMenu/sign.sh <app>`
turns that into a Developer ID signature, and notarizes it, when the environment names them:

| variable | meaning |
| --- | --- |
| `POTLUCK_SIGN_IDENTITY` | a "Developer ID Application" identity in the keychain (its name or SHA-1) |
| `POTLUCK_NOTARY_PROFILE` | a keychain profile made with `xcrun notarytool store-credentials` |
| `POTLUCK_NOTARY_KEY`, `POTLUCK_NOTARY_KEY_ID`, `POTLUCK_NOTARY_ISSUER` | instead of a profile: an App Store Connect API key (path to `AuthKey_*.p8`, its key id, its issuer id) |

- With an identity it signs nested code first and the app last (hardened runtime, secure timestamp, never `--deep`),
  then runs `codesign --verify --deep --strict`.
- With a notary profile or key as well it zips the app with `ditto`, runs `notarytool submit --wait`, staples the
  ticket, and checks `spctl --assess -t exec`. If Apple rejects it, the script prints Apple's log and fails.
- With neither, it leaves the ad hoc build as it is and says so.
- It never falls back to ad hoc when a signature was asked for. An identity the keychain does not hold, notary
  settings without an identity, notary key variables with one missing, and a profile together with a key all fail.
- It prints no secret. The notary key file is read by `notarytool` only.

Only the signing path is tested so far (with an Apple Development identity); notarizing has not been run against
Apple's service yet.

## How the release builds it

`.github/workflows/release.yml` builds the app in its own job on a GitHub-hosted Mac (`macos-latest`, never a
self-hosted runner), zips it (`ditto`), and `scripts/release -menubar` puts the zip into each darwin tarball. If that
job fails, the release still ships, without the app.
