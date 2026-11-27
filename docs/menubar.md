# The macOS menu bar item

An item in the menu bar for a person lending their Mac. It shows how much of the Mac is lent and in use,
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

While a host run sets this Mac up and nothing is shared yet, the title says the stage in the setup window's words,
"Setting up: download what jobs need", and the note its last words; a run that stopped says where, "Setup stopped
while testing that jobs run safely", with no note: Set up and check… says why and what to do, and in a terminal
`potluck host status`'s setup lines give the stage, the reason and the command that runs it again
([hosts.md](hosts.md#stages-and-the-setup-checklist)). Once sizes are shared, a later run that stopped is a note, "the
last setup stopped while checking this Mac; what was shared before still lends". The menu names no command.

A closed switch is a paused Mac (`potluck host pause`, or Stop now): the menu says "Paused: lending nothing" and
Keep lending reads "Resume lending" (it runs the same `host set auto`, as `potluck host resume` does). While a
`potluck host --detach` run goes on, the note says so with its last line; `host status --json` carries it as
`background` (`running`, or `ended` with the last run's last line).

A size that lends other images than the daemon's build pins (the daemon moves it after an update,
[hosts.md](hosts.md#after-an-update-the-daemon-moves-a-host-to-the-new-pins)) adds a note, "1 size lends other images
than this version pins"; `host status` prints a line per size with why, and `--json` carries them as
`images_behind`.

## Its icon, and the Dock

The menu bar shows the mark in one colour, which macOS draws dark or light as the menu bar needs. A dot in its corner
says something is wrong, and the menu says what: the last status ask failed, or the menu has `attention` (package
`menubar`): the switch, the limits or what is lent cannot be read, no driver answers, the daemon is down on a Mac where
setup ran, a setup stopped before anything was lent, or the group this Mac lent to is gone. A Mac a person paused,
winds down or stopped (`potluck daemon stop`) has no dot.

The app is in the Dock, with a menu bar of its own, while one of its windows (a minimized one too) or a question is
open; with none it is in the menu bar only. Opening it again (Finder, Launchpad, its Dock icon) brings up what is
open, or the setup window when nothing is (during and after a removal, the uninstall window). ⌘W closes a window and
⌘Q quits the app, the menu bar item with it (a setup step that did not finish is offered again next time). The code is `Dock.swift` and `MainMenu.swift`.

The drawings are `macos/PotluckMenu/icon/AppIcon.svg` (its `@media (max-width: 40px)` and `(max-width: 20px)` rules pick
what the 32 and 16 pixel sizes show) and `MenuBar.svg`; both say `width="1024" height="1024"`. `icon/make.sh` makes `AppIcon.icns` and the two `MenuBar` pictures from them, and
all of these are kept in the repository, so the app ships the pictures that were looked at. To change the mark, change
a drawing, run `make.sh` and commit what it made: `build.sh` and `go test ./menubar` refuse pictures that do not match
their drawings.

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

"Set up and check…" opens the welcome and setup window. The app shows the next step under its welcome.
The checklist comes from `setup.view` of `potluck --json host status --probe`, written for
someone who has never used a terminal (`menubar.SetupView`): a heading that says how this Mac stands, what to do or
expect, the button that does it, a line per thing lending needs, what is optional under its own title (its buttons
are small ones on its lines), and while a setup goes on, or after one stopped, its stages. Every word comes from
potluck; the app only draws. No command, path or inside name is in it. "Check again" asks at once, and "Copy details",
there only when something needs help, copies the terminal's words (`view.details`, never shown) to send to someone.

The window asks with `--probe` when it opens, after an action, on "Check again" and about every 30 seconds, since
`--probe` runs Tart and Softnet. Every 3 seconds it asks plain `host status`, which redraws only the stages; when
that answer's `view.key` is not the last probe's (a setup ended, the daemon stopped), it probes then. It keeps the
last good picture, with a red line saying so, when an ask fails, and asks again; "Copy details" then copies the
error too. `--probe` adds this Mac's own checks first: Apple
silicon, macOS and virtualization (`mac`), Tart, Softnet and the free disk where Tart keeps images. The menu's own
15-second ask leaves them out.

The window's button is `view.do`: the running setup's, else the first required check's that has something to do, unless something
on this Mac stops it that nothing here fixes, which comes first, with no button
(never an optional one's, as the terminal's `next_id` may be,
[hosts.md](hosts.md#stages-and-the-setup-checklist)). A network helper that waits only on Potluck finishing its own
testing for this Mac's macOS does not come first: the steps before it that do not need it (installing Potluck and
Tart) are offered, and it stops only the steps after it. `view.invite_later` is that a step still to come takes an
invite: an invite from a `potluck://` link is kept for it while the field is hidden. A blocked check with nothing to
run shows its words and no button of its own. A `do` with `url` opens that page in the
browser (only an `https` page, or the `file://` URL of an installed app). A `do` with `args` runs potluck with them;
when it has `invite_args`, an invite field shows above the button, and with something pasted there the app runs those
arguments instead (they end in `--invite-stdin`, [hosts.md](hosts.md#stages-and-the-setup-checklist)) and writes what was pasted and a newline to
potluck's stdin. The invite is never an argument or in the environment, so no other user of the Mac sees it in the
process list. The app runs potluck directly, never through a shell, writes nothing of the run to a log, and keeps
the invite in the field, in memory only, until this window shows that setup went through with it or the app quits, so a setup that stops needs no second paste. With the field empty
it runs `args`, with nothing on stdin (`potluck host` then shares with the one pool this machine is already in). A
`do` with `app` is an act only the app can do: turning its login item on (`login_item`) or off (`login_item_off`), or
opening System Settings > General > Login Items (`login_item_settings`); one this build does not know has no button.
A `do` with `auto` the app also runs by itself, once each time it starts, and never with a password step: the one
there is starts the daemon of a Mac whose setup was begun, finished or not, since nothing else starts it after a
restart. It is not set after `potluck daemon stop` or an uninstall until the daemon is started again, nor for another program's daemon.

### An invite link

`potluck://open?invite=<invite>`, the invite (`https://potluck.sh/join#v1.…`) percent-encoded, opens the app and this
window with the invite in the invite field, or keeps it until a step asks for one. Any
web page can open such a link, so it only fills the field: the person presses the button, and Return does not press
it while a link's invite is anywhere in the field. A field that holds other text is replaced only after a question, whose answer on
Return keeps the text. A link that does not hold exactly one invite of that shape does nothing. While a step runs, the
link is not used and the window says to open it again.

An action runs with no time limit (a plain status ask is ended after 25 seconds, one with `--probe` after 60) and shows "<label>…" while it runs.
If potluck fails, the window says so in red, in the error's plain words (`plain` of its `--json` error, written by the
command that failed for someone who has never used a terminal). An error without them shows its
message and hint, unless it is potluck's own failure (code `internal` or `unavailable`) or is not JSON: then the
window says to try again. The message, hint and whatever else the program or macOS said are only in what "Copy details" copies. The button stays
off until a status asked after the action, or after the window opened, is drawn. Closing the window keeps the invite in its field. If the action's `--json` answer holds `admin_step`
(`{"why", "argv"}`, from `--no-sudo`, [softnet.md](softnet.md#without-a-terminal---no-sudo)), the window shows `why` (plain words, "macOS will ask for this Mac's password to install the network helper.";
`potluck` in a terminal says instead what the step puts where and why it needs root) with Continue and Cancel;
Continue runs `argv` as root through macOS's own administrator password dialog (AppleScript `do shell script ...
with administrator privileges`, each word quoted for the shell; the app never sees the password), then runs the
same action once more, with the same program. Cancel, at either question, says what the action's answer (`done`)
says was already done, and "Nothing was changed" only when that list is there and empty; an action that does not
say what it did gets "You cancelled the password step, so this isn't finished". It does not repeat the reason the
person just read; it points to Try again, which runs that action again with the same arguments and stdin (the invite too, held in memory only while that
action is still the next step and the window is open), as often as the person likes. A
failed root step is said in plain words too, with its error text in the details.

A failure stays until the next action, closing the window, or a full check asked after it that finds its step over:
the check whose `do` was run (`setup.checks`) is `done`, `running` or `info`. While that check is open, could not be
read or is missing, and while anything in the window still offers the action, the failure stays, whatever step is
next by then. An action no check has goes by whether the window still offers it.

With a potluck too old for this window (no `setup.view`, or a `do` that takes the invite as an argument: `invite`
without `invite_args`) the window says "Potluck needs updating". Its button there, Update Potluck, is the app's own
and runs `potluck upgrade`; another program's potluck, which has no such command, gets no button. The other lines,
with theirs (turning on daily updates), and the details stay. Setup is not offered: the app never gives an invite as
an argument. Such a potluck's `host status` may not know `--probe`; the app then asks without it. The other way
round, an older app appends what was pasted to `args`, so those end in `--`: a paste is never read as a flag.

The code is `SetupFlow.swift` (what the window shows of an answer, when a failure is old, when a button may be
pressed), `Run.swift` (finding and running potluck, errors, and the action rule), `Admin.swift` (the admin step: its
answer, the quoting, the AppleScript) and `SetupWindow.swift`.

The Go side is `menubar.Setup` (`setup` in the status JSON); the Swift side
is `StatusModel.swift` (the fields it reads) and `SetupWindow.swift`. The window draws the stages from
`view.progress`, so a new stage needs no Swift change. `go test
./menubar` writes nothing, but checks nine sample answers (never set up, setup running, the join waiting for a
tailnet login, blocked at validation, lending) against
`menubar/testdata/status-*.json`; `go test ./menubar -run TestStatusFixtures -update` rewrites them, and
`macos/PotluckMenu/test.sh` compiles the app's decoder structs with `swiftc` and decodes each of them. It also
decodes sample action answers with and without `admin_step` (`macos/PotluckMenu/test/answers`), runs the action rule
against a fake potluck with a stand-in for the password dialog, checks that `ps` and the environment show the invite
in no run of it (the re-run after an admin step and Try again too), and checks that an
argv with spaces, quotes, `$`, backticks and a newline reaches a program word for word through the same shell
quoting and AppleScript the admin step uses (without administrator privileges).

## Uninstall Potluck

"Uninstall Potluck…" opens a window drawn from what `potluck --json uninstall --dry-run` answers
([install.md](install.md#removing-it)). Its words are the answer's `view` (`cli/uninstall_view.go`), written for
someone who has never used a terminal: what will happen, what stays, and, in red, what is lost for good (this Mac's
identity, and the groups it runs, which end for everyone in them; when Potluck cannot say which those are, the
warning says that). Opening it changes nothing. Copy details copies the same answer in the terminal's words, for
whoever helps. An answer with no `view` (an older potluck) removes nothing: the window says to update Potluck.

When there is something to take, a box, not ticked, offers to also remove Tart, if Potluck installed it, and the
images Potluck pulled (`view.choice`; what it takes and never takes is in
[lending.md](lending.md#tart-and-its-images-uninstall---with-tart)). Pressing it asks for the plan again with
`--with-tart`, so the steps, the question and the arguments Remove… runs all come from that plan. Opening the window
again starts with the box not ticked.

Remove… asks for the plan again, then asks for a yes with that plan's warning, and runs the plan's own arguments
(`remove.args`: `potluck --json uninstall --yes --no-sudo`), with the admin step rule above. Return never removes.

- Cancelled at the admin step, or a failed step: where it stopped, what was done before it, and what is still to do.
  Remove… goes on. A failed step may be partly done.
- Work still running: potluck's refusal says so and what each button does. The app never forces by itself; Remove
  anyway… shows what forcing does (`force`), with the plan's warning as it is then, and asks once more. It removes
  what the refused run would have: the box cannot be changed meanwhile.
- Removed: what was done, what stays, and "Move to Trash and Quit", which moves only this app's own
  bundle, to the Trash, when clicked. The app takes itself off the login items first.

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
open ~/Applications/Potluck.app
```

`--menubar` is off unless you ask for it. It installs potluck as usual, then takes `Potluck-Menu.zip` out of the same
signed tarball (its hash is in the signed manifest, and install.sh checks it again), unpacks it into
`~/Applications` (`POTLUCK_MENUBAR_DIR` changes that) and replaces an older copy there, asking a running one to quit
first. The app was called `Potluck Menu.app` before it was `Potluck.app` (its program, `PotluckMenu`, and bundle id,
`sh.potluck.menu`, did not change): the installer takes either name from a release, and replaces a copy under either.
A replaced copy goes to the Trash. An app under one of those names that is not Potluck's (another bundle id) is left
alone, and the installer stops rather than replace it.
A release built without the app makes `--menubar` stop after installing potluck and say so. The app is a universal
binary (Apple silicon and Intel), macOS 13 or newer.

**A release made without Apple's keys carries an app that is not Developer ID signed or notarized.** It is signed ad
hoc. Installed this way (curl, not a browser download) macOS runs it. If you unzip a copy you downloaded in a browser
and macOS refuses it, right-click it in Finder and choose Open once. The installer says "not Developer ID signed" when
that is so.

To start it at login, choose "Set up and check…" in its menu, then "Start at login" ("Turn off" there undoes it). If
it was turned off in System Settings > General > Login Items, macOS lets only the person turn it on again, there, and
the button opens those settings. Quit it from its menu. To update the app,
run the installer with `--menubar` again; `potluck upgrade` and the daily update do not touch it yet.

Opened outside an Applications folder (from Downloads, say), the app asks to move itself to `/Applications`, or
`~/Applications` when it may not write there. It copies itself, takes the download quarantine off the copy, opens the
copy and puts the one it came from in the Trash; macOS's temporary copy of a download (Gatekeeper's translocation)
stays where macOS keeps it, and the download with it. When Potluck is installed there already, the app asks whether to
open that one (Return's answer) or replace it with this one. A `Potluck.app` already there is replaced (the old one to the
Trash) only when it has Potluck's bundle id and the app was opened from there before; else the app says so and moves
nothing. "Not now" is remembered for that place. Without `POTLUCK_MENUBAR_DIR` the installer replaces an app of
Potluck's in `/Applications` when `~/Applications` has none, so there is one app.

The renamed app, in an Applications folder, moves a `Potluck Menu.app` that asked potluck before (below) and has
Potluck's bundle id to the Trash when it starts, after it quit, so one item shows in the menu bar. It then offers to
start at login, as the old one may have.

### First run, from an app that carries a release

An app built with a release in it (`POTLUCK_BUNDLE_RELEASE`, below; the release's `Potluck.zip` is one, its `Potluck-Menu.zip` is not)
can install Potluck on a Mac that has none. When it starts and finds no potluck, nor any record of a
Potluck daemon whose program is still there, it runs the `install.sh` in it on the
release in it (`--from <release> --no-modify-path`): the terminal's installer, which checks the manifest's signature
and the tarball before anything of the release runs. The programs go in the app's cache directory
(`~/Library/Caches/sh.potluck.menu/potluck.<pid>`, one for each running app), not in the install directory, and no home, daemon or setting of this
Mac's takes part. Then the setup window opens, drawn by that potluck, whose first step is "Install Potluck":
`potluck upgrade --release <release> --dir <install dir>`, which checks the release again and refuses one older than
this Mac installed. The app tells that potluck of the release in `POTLUCK_BUNDLED_RELEASE`; told so, `upgrade` never
replaces a potluck already in the install directory or installs beside a daemon that runs, and the checklist offers no daily update and no start by itself.
Once a potluck is installed the app runs that one, never has it install the carried release, and empties the cache directory. When Potluck goes while the app is open (a removal in Terminal, a cleared cache), the setup window does the same again. A release that does
not pass, or cannot be unpacked, is not used: the window says to press "Check again", which checks it once more, or to download it again, and "Copy details" copies the installer's words.
An app moved or deleted while it was open says so. Where there is no potluck to run at all, the window's button is "Get Potluck", which opens potluck.sh.

Whether or not it carries a release, the app opens the setup window by itself once, the first time it runs on a Mac
that was never set up.

### What host status knows of the app

`potluck host status` lists the app in its setup checklist. Installed is a `Potluck.app` (or a
`Potluck Menu.app`, its name before) in the last place it ran from, `$POTLUCK_MENUBAR_DIR`, `~/Applications` or
`/Applications`. Running is only the app's own word: when the
program that runs `host status` is the app (its parent process is `Contents/MacOS/PotluckMenu` in an app, under either name),
host status notes the app and the time in the home's `menubar-seen.json`, and the app counts as running while that
time is under a minute old (it asks every 15 seconds). `open` exiting 0 is no proof that it shows. The app
tells it its login item too (`on`, `off`, or `refused` in System Settings), in `POTLUCK_LOGIN_ITEM`, which counts only
from the app; an app from before that says nothing, and the checklist says how to add it by hand. The note keeps the
other places the app asked from that are still there: those under the old name are `setup.old_apps`.

The app runs `$POTLUCK_BIN`; else `potluck` from `~/.local/bin`, `/opt/homebrew/bin`, `/usr/local/bin` or
`~/go/bin`; else the program of the machine's Potluck daemon, from its record
([daemon.md](daemon.md#one-daemon-per-machine)). That program must have potluck's `host status` and `host set`
commands; when it has not, the menu shows the error it prints. The app uses a record only in a directory and file
that are yours alone: the only one of a program other than potluck that names a running process of yours, else
potluck's own, whether or not its daemon runs. Unlike
the `potluck` command, it does not ask that daemon's socket whether it answers.

Setting the switch writes `/Users/Shared/Potluck/host-override.json` as you, so run the app as the user who owns
the host (the one who ran `potluck host`).

## Build it from source

On a Mac with the Xcode command line tools:

```sh
macos/PotluckMenu/build.sh ~/Applications   # writes ~/Applications/Potluck.app (ad hoc signed, this Mac's CPU)
open ~/Applications/Potluck.app
```

`POTLUCK_MENU_UNIVERSAL=1` builds both CPUs, as the release does. `POTLUCK_BUNDLE_RELEASE=<dir>` puts a signed release
in the app ("First run", above): that directory's manifest, its two signatures and both Mac tarballs, and the tree's
`install.sh`.

## Signing and notarizing

`build.sh` signs ad hoc, which runs on the Mac that built the app and nowhere else. `macos/PotluckMenu/sign.sh <app>`
turns that into a Developer ID signature, and notarizes it, when the environment names them:

| variable | meaning |
| --- | --- |
| `POTLUCK_SIGN_IDENTITY` | a "Developer ID Application" identity in the keychain (its name or SHA-1) |
| `POTLUCK_NOTARY_PROFILE` | a keychain profile made with `xcrun notarytool store-credentials` |
| `POTLUCK_NOTARY_KEY`, `POTLUCK_NOTARY_KEY_ID`, `POTLUCK_NOTARY_ISSUER` | instead of a profile: an App Store Connect API key (path to `AuthKey_*.p8`, its key id, its issuer id) |

- `sign.sh --programs <dir>` signs every Mach-O file under a directory instead of an app, and does not notarize.
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

When Apple's keys are set up ([release-keys.md](release-keys.md#apples-signing-keys)) a release made by a push also:

- signs that app and the Mac programs in the tarballs with the Developer ID (not notarized: the release would wait for
  Apple, and what `curl` downloads is not quarantined). `sign.sh --programs` signs the programs. They are signed
  because Apple's notary service looks inside the archives of an app it is sent.
- after the release is out, starts `.github/workflows/app.yml`, which builds the app again with that release in it, signs, notarizes and staples it, and adds it to
  the release as `Potluck.zip`. Until then (Apple's answer usually takes minutes, and `sign.sh` waits 30 at most) the
  newest release has no `Potluck.zip`, and one whose signing or notarizing failed never gets it.

`scripts/ci/publish-app.sh` uploads it under another name and renames it only when GitHub holds all of it. A failure in
any of this leaves the release as it is and opens an issue. While the keys are not set up, or signing fails, the app is
ad hoc signed as `build.sh` left it.
