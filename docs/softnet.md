# Softnet on a Mac

Softnet is the helper Tart starts for each box's isolated network. It runs as root (setuid), keeps the box off this
Mac and its LAN, lets it reach this Mac only for DNS, and blocks outbound mail ports. Potluck needs a Softnet that
accepts protocol and port rules (`out proto udp @host port 53`); `potluck doctor` and `potluck host` run it on the
exact rules a box boots with before lending ([hosts.md](hosts.md)).

## Which Softnet a Mac uses

One table decides this, `softnet.For` (`softnet/builds.go`); preflight, boot failures and `potluck softnet` all
read it.

| macOS | Softnet |
| --- | --- |
| 26 or newer | upstream's release `0.26.0`, through `potluck softnet install`; or an upstream release 0.25.0 or newer you set up yourself, owned by root and setuid |
| 15 | Potluck's build `0.23.0-potluck-pr227-candidate.1`, a candidate (below), through `potluck softnet install --candidate` |
| 14 and older | none that enforces Potluck's rules: this Mac cannot lend isolated boxes |

Upstream 0.23.0 runs on macOS 15 but refuses protocol and port rules; 0.24.0 and newer use vmnet calls that need
macOS 26. Potluck's macOS 15 build is upstream 0.23.0 with upstream's rule change
([openai/softnet#227](https://github.com/openai/softnet/pull/227)) carried onto it, using only vmnet calls macOS
15 has.

On macOS 26 or newer, Potluck pins upstream's own release, unchanged: the `softnet.tar.gz` asset of
[0.26.0](https://github.com/openai/softnet/releases/tag/0.26.0), by the archive's size and SHA-256 (the same
SHA-256 upstream publishes in `softnet_0.26.0_checksums.txt`) and the executable's. It is the Softnet these Macs
were already sent to, so it needs no `--candidate`. A Softnet you set up yourself from upstream's releases, owned by
root and setuid, keeps working: Potluck's boxes use the managed copy only when one is installed, else the
`softnet` on PATH or, with none there, in `/opt/homebrew/bin` or `/usr/local/bin`, and `potluck host` still offers the `chown`/`chmod` root step for one that is not yet root
owned.

The macOS 15 build is a **candidate**: it has not yet passed Potluck's isolation checks on a real macOS 15 Mac. Until it
does, preflight does not tell people to install it, and `potluck softnet install` needs `--candidate`. A Mac that
installs it still runs every preflight check before it lends.

`potluck softnet status` says this Mac's macOS, what the managed install holds, which Softnet Potluck's boxes run
and, when that is not this Mac's row, how to get it.

## Install, update, roll back

```sh
potluck softnet install                 # download the build for this macOS, check it, install it
potluck softnet install --from <file>   # the same from a copy of the file you already have
potluck softnet rollback                # put back the build in use before the last install
potluck softnet uninstall               # disarm and remove every managed build
```

Run them as yourself, not with sudo. `install`:

1. checks this is an Apple-silicon Mac on a macOS that has a build, and that Tart is installed and has the run
   options Potluck boots a box with;
2. downloads the build (or reads `--from`, which is the executable itself) and checks its size and SHA-256 against
   the build this potluck pins (`softnet.Builds`). A build published as an archive (upstream's `softnet.tar.gz`)
   is checked against the archive's pinned size and SHA-256 before anything in it is read; then only its
   `softnet` file is taken, and checked the same way. A potluck from a signed release carries those pins, so a
   file from anywhere is accepted only if it is that exact build;
3. runs the admin step through `sudo -k`, which asks for your Mac password (always: a recent sudo sign-in is not
   reused). As root it copies the file into a
   private directory (never reading the original again), checks the copy's SHA-256 again, makes it owned by root
   and setuid, runs it once to see it starts on this macOS and accepts Potluck's rules, and only then puts it in
   use;
4. checks that Potluck's boxes now run that copy.

Why root: Tart starts Softnet for every box, and Softnet needs root to set up the network. Setuid root lets it do
that without a password each time. The admin step runs this potluck program as root; it reads only the Mac's
release (`sysctl`, `sw_vers`) and runs only the build it has checked.

Running `install` again with the build in use changes nothing and asks for no password. Installing a newer build
keeps the one it replaces; `rollback` puts that one back, after the same checks, and keeps the newer one in its
place. Only those two are kept: any other build is disarmed (every mode bit cleared, so a hard link to it is no
longer setuid either) and removed. A build this potluck no longer pins, or has withdrawn, is never put back in use.

A build is never removed from `softnet.Builds` once a potluck has pinned it. One that must stop running gets a
`Withdrawn` reason. Once `potluck upgrade` brings that potluck to a Mac, every boot checks the managed build again
before it starts it, so no isolated box starts on a withdrawn build; preflight names the reason. When this macOS
has another build, `install` puts it in use and the withdrawn copy is disarmed and removed; when it has none,
preflight refuses to lend until the withdrawn copy is removed with `potluck softnet uninstall` (it is still a
setuid root file).

### Without a terminal: `--no-sudo`

An app has no terminal for sudo to ask on, so it runs the root step itself, through macOS's administrator
password dialog. `install`, `rollback` and `uninstall` take `--no-sudo`: each does its part as you (for `install`,
the checks above and the download), then, in place of running sudo, says the root command and exits 0. With
`--json` the answer is data:

```json
{"admin_step": {"why": "macOS will ask for this Mac's password to install the network helper.", "argv": ["/Users/you/.local/bin/potluck", "softnet", "install", "--admin-step", "--from", "/Users/you/Library/Caches/potluck/softnet/softnet-<build>-darwin-arm64"]}}
```

`argv[0]` is the absolute path of this potluck program. Run `argv` as root as a list of words: an app that hands it
to a shell (AppleScript's `do shell script ... with administrator privileges`) must quote every word. `why` is for
the person, in plain words; the other lines go to stderr. A command that needed no root step answers `{}`.

`install --no-sudo` keeps the download in your cache directory (`~/Library/Caches/potluck/softnet`), since the
root step runs after the command has ended. The root step trusts nothing from that file but its bytes: as root it
checks that it runs as root, copies the file and checks the copy's size and SHA-256 against the pinned build, as
above. Run `install` again after the root step: with the build in use it says nothing to do, checks which Softnet
the boxes run, and removes the kept download.

Nothing outside the managed directory changes. Another `softnet` on PATH (Homebrew's, or one in
`/usr/local/bin`) stays where it is; Potluck's boxes use the managed copy, and preflight warns that the one on
PATH is not the one they run.

### Where it goes

```
/Library/Application Support/Potluck/Softnet/
  <build>/softnet           root:wheel, setuid, 4755
  <build>/LICENSE           the upstream license
  <build>/provenance.json   the build's source, SHA-256 and license
  current  -> <build>       what Potluck pins for Tart: .../current/softnet
  previous -> <build>       what rollback puts back
```

Every directory from `/` down is checked to be a real directory owned by root and writable by no one else, with no
ACL entry that lets anyone write to it, add to it or change its owner or permissions (deny entries are fine; an
entry Potluck cannot read counts as unsafe), so no other user can swap the file root runs. The build's own files
get the same check each time preflight reads them. Preflight pins `.../current/softnet`, so an install or rollback takes
effect for the next box. Run `potluck host` after the first install so a Mac that already lends pins it.

To remove it: `potluck softnet uninstall`. A box already running keeps its Softnet process. A Mac that lends still
pins the removed path, so its isolated boxes do not start until `potluck host` runs again and pins the `softnet`
on PATH (if one passes its checks). On a Mac upgraded from macOS 15 to 26 or newer, preflight stops using the managed
macOS 15 build, warns that it is not for this macOS and checks the `softnet` on PATH instead (a release you set up
yourself). Run `potluck softnet install` to put the upstream release in use (the macOS 15 build is kept as the
previous one, which `rollback` refuses on this macOS), or remove the managed copy with `potluck softnet uninstall`;
then `potluck host` again.

## Updates

`potluck upgrade` (and its daily run, `potluck upgrade enable`) keeps potluck current without a password, so it
cannot replace a root-owned setuid file. When a newer potluck pins a newer build for this macOS, `potluck doctor`,
`potluck host` and `potluck softnet status` warn that the managed Softnet is not this potluck's build and name
the command: run `potluck softnet install` to update (with `--candidate` for a candidate build). The build in use
keeps working until then.

## License and provenance

Softnet is under the Functional Source License 1.1, ALv2 Future License (FSL-1.1-ALv2, copyright OpenAI), not
Potluck's Apache-2.0. Its text is `softnet/UPSTREAM-LICENSE`; it is installed beside every build and published
with it. Each build's source (upstream commit, the change carried onto it, the commit and tree built, the build
recipe) is in `softnet.Builds` and in the `provenance.json` beside it. The upstream release Potluck pins for macOS
26 and newer is under the same license: the `LICENSE` in its archive is byte for byte `softnet/UPSTREAM-LICENSE`,
the copy an install places beside it. Potluck does not republish it; `install` downloads it
from upstream's release.

## Rehearsing a build in a macOS VM

`scripts/softnet-sim` checks a Softnet build on a macOS version without a Mac that runs it: in a macOS VM (Tart, a
`macos-<version>-vanilla` image by digest), where a guest cannot start VMs of its own, it runs the real executable
on that macOS's real vmnet with a fake VM on its socket. The fake VM speaks what Tart passes, raw Ethernet frames
in a Unix datagram socket pair, and asks for its address with DHCP. The harness makes every endpoint (this Mac's
own ports, a LAN, a tailnet range, a mail server, a second VM) and shows frame by frame what left Softnet, what was
answered and what came in. Run it as root inside the VM; it refuses to run outside one:

```sh
CGO_ENABLED=0 go build ./scripts/softnet-sim     # then copy it into the VM
sudo ./softnet-sim -softnet "/Library/Application Support/Potluck/Softnet/current/softnet" \
  -mutant <softnet built to filter nothing> -forbid <a real LAN address:port> -md report.md -json report.json
```

A probe counts as stopped only when the same probe, with no rules in the way, left Softnet and its endpoint
answered; where nothing can answer (a spoofed or malformed frame) the mutant, a Softnet built with its filter
always saying yes, shows it would have left. Each deliberately broken rule set must fail, in each group of probes it
breaks. It checks the destinations in `probes.go` and no others. The run passes (exit status 0) only when all of that
ran; what it could not show is listed as "not shown".

It is a rehearsal and does not set `Validated`: no guest kernel, no Tart and no conformance run take part, so a
build is validated on real hardware all the same.

## Publishing a build

An upstream release (`UpstreamRelease`) is published by upstream; pinning a newer one is step 1 alone: its
release asset's URL, the archive's size and SHA-256 (computed from the downloaded file and compared with
upstream's checksums file), the executable's size, SHA-256 and exact `--version`, and its source commit and tree.
Add it before the release it replaces, so `softnet.For` offers it first. Steps 2 and 3 are for Potluck's own builds.

Publishing a build of Potluck's own is an owner's decision (it distributes third-party code under its license). The steps:

1. **Pin it.** Add the build to `softnet.Builds` (ID, the exact `--version`, SHA-256, size, the macOS versions
   it covers, its source) and merge. `Validated` stays false until real-Mac validation passes; then a second
   change sets it, and preflight starts telling Macs on that macOS to install it.
2. **Assemble its files** from the reviewed executable:
   `go run ./scripts/softnet-release -binary <softnet> -out <new dir>`. It refuses a file that is not the pinned
   build, and writes the executable (`softnet-<build>-darwin-arm64`), `LICENSE`, `provenance.json`,
   `SHA256SUMS` and the release notes.
3. **Publish them** as the release `softnet-<build>` of the public repository, with the command step 2 prints
   (`gh release create ... --latest=false`, and `--prerelease` while the build is a candidate). The tag never
   starts with `potluck-`, so `install.sh` and `potluck upgrade` never take it for a Potluck release. The public
   repository accepts new tags only from its release workflow, so this needs an admin of it.

Until step 3, `potluck softnet install` says the build is not published yet and installs only `--from` a copy of
the file.
