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
| 26 or newer | the [latest upstream release](https://github.com/openai/softnet/releases/latest), 0.25.0 or newer |
| 15 | Potluck's build `0.23.0-potluck-pr227-candidate.1`, through `potluck softnet install` |
| 14 and older | none that enforces Potluck's rules: this Mac cannot lend isolated boxes |

Upstream 0.23.0 runs on macOS 15 but refuses protocol and port rules; 0.24.0 and newer use vmnet calls that need
macOS 26. Potluck's macOS 15 build is upstream 0.23.0 with upstream's rule change
([openai/softnet#227](https://github.com/openai/softnet/pull/227)) carried onto it, using only vmnet calls macOS
15 has.

That build is a **candidate**: it has not yet passed Potluck's isolation checks on a real macOS 15 Mac. Until it
does, preflight does not tell people to install it, and `potluck softnet install` needs `--candidate`. A Mac that
installs it still runs every preflight check before it lends.

`potluck softnet status` says which row this Mac is in, what the managed install holds, and which Softnet
Potluck's boxes run.

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
2. downloads the build (or reads `--from`) and checks its size and SHA-256 against the build this potluck pins
   (`softnet.Builds`). A potluck from a signed release carries those pins, so a file from anywhere is accepted
   only if it is that exact build;
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
on PATH (if one passes its checks). On a Mac upgraded to a macOS that upstream Softnet covers, preflight stops
using the managed copy and warns that it is not for this macOS: install upstream's release, remove the managed
copy the same way, and run `potluck host` again.

## Updates

`potluck upgrade` (and its daily run, `potluck upgrade enable`) keeps potluck current without a password, so it
cannot replace a root-owned setuid file. When a newer potluck pins a newer build for this macOS, `potluck doctor`,
`potluck host` and `potluck softnet status` warn that the managed Softnet is not this potluck's build and name
the command: run `potluck softnet install` to update. The build in use keeps working until then.

## License and provenance

Softnet is under the Functional Source License 1.1, ALv2 Future License (FSL-1.1-ALv2, copyright OpenAI), not
Potluck's Apache-2.0. Its text is `softnet/UPSTREAM-LICENSE`; it is installed beside every build and published
with it. Each build's source (upstream commit, the change carried onto it, the commit and tree built, the build
recipe) is in `softnet.Builds` and in the `provenance.json` beside it.

## Publishing a build

Publishing a build is an owner's decision (it distributes third-party code under its license). The steps:

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
