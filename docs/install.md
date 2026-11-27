# Installing Potluck

The one-line install is in the [README](../README.md#install). This page has the options.

## From a release (macOS or Linux, no Go)

```sh
curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh
```

It puts `potluck`, the drivers (`potluck-driver-local`, and `potluck-driver-tart` on every platform but an Intel
Mac), `potluck-conformance` and `potluck-e2b` in `~/.local/bin`. `POTLUCK_INSTALL_DIR` changes the directory; the daemon runs drivers only from its own
directory (see [DRIVERS.md](../DRIVERS.md)). `potluck-e2b` is optional: it lets E2B's SDKs run commands in your
pool's sandboxes ([daemon.md](daemon.md#e2b-gateway)); ignore it if you do not use E2B.

### What is checked

Each release publishes `manifest.json`, which names the SHA-256 of every tarball, and two signatures of it made with
the same Ed25519 release key: `manifest.json.sshsig` (SSHSIG, namespace `potluck-release`) and `manifest.json.sig`
(raw Ed25519). The script carries the public half of each trusted key (`RELEASE_KEYS`, the list in
`internal/upgrade/release_keys.txt`; a test holds them equal) and, before any program from the release runs:

1. verifies `manifest.json.sshsig` with `ssh-keygen -Y verify` (OpenSSH 8.1 or newer: macOS 10.15 and later, and Linux
   with `openssh-client`) against those keys;
2. without a usable ssh-keygen, or when the release has no `manifest.json.sshsig` or it does not verify, verifies
   `manifest.json.sig` with `openssl pkeyutl -verify -rawin` (OpenSSL 3 or newer, as in current Ubuntu, Debian and
   Fedora; macOS's LibreSSL cannot check Ed25519, so on a Mac step 1 is the check);
3. counts a tool only if it verifies a built-in probe signature and refuses that probe changed, and only if it also
   refuses the release's manifest with one byte added, so a tool that says yes to anything is never trusted;
4. refuses an unsigned release, a manifest neither signature verifies, a tarball whose SHA-256 is not the one the
   verified manifest names, and, with `--version` or a token, a signed manifest of another release than the one
   asked for.

A download without a token takes the newest release's version from its manifest and fetches the signatures and the
tarball from that release's own tag, so two releases' files never mix.

If potluck is already installed, the script still checks the signature, then hands the release to that potluck
(`potluck upgrade --release`), which checks the raw signature again with the keys built into it and the tarball's
SHA-256, then installs it.

With neither tool, the script installs nothing and says what to install (on Debian or Ubuntu:
`sudo apt-get install openssh-client`), unless potluck is already installed, whose own check is then the one made.
`--unsafe-skip-signature-check` installs a download from GitHub without the check, trusting HTTPS alone, and says so in
capitals; it never applies to `--from`, and a signature that does not verify is refused even with it. Before publishing,
the release workflow runs the script on the new Linux build for its runner's architecture, with only the system's
tools, and requires the SSHSIG check to pass, so a release whose SSHSIG does not verify with the keys the script
carries is never published.

Which keys are trusted, and how the signing key is replaced: [release-keys.md](release-keys.md).

### Options

- With no token the script makes no GitHub API call: it downloads the newest release from github.com's release
  links, which GitHub's anonymous API limit (60 calls an hour per address, shared by everyone behind one office, CI
  or cloud NAT) does not count.
- `GH_TOKEN` (or `GITHUB_TOKEN`), if set, makes the script read the release through GitHub's API with that token. A
  private repository needs one that can read it (fetch the script with
  `curl -fsSL -H "Authorization: Bearer $GH_TOKEN" …`). A token GitHub refuses fails the download; it is not ignored.
- `--join '<invite link or code>'` joins the pool of that link or code once potluck is installed (`potluck join`), so a friend with
  an invite needs one line: `curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | sh -s -- --join '<invite code>'`.
  If the join fails, potluck stays installed and the script says to run `potluck join` again.
- When the install directory is not on PATH, the script adds `export PATH="$HOME/.local/bin:$PATH"` (fish:
  `fish_add_path`) once to the startup file of your shell (`$SHELL`): `~/.zshrc`, `~/.bashrc` on Linux,
  `~/.bash_profile` on a Mac, else `~/.profile`, or fish's `conf.d/potluck.fish`. A new terminal then finds
  `potluck`. `--no-modify-path` leaves every startup file alone and prints the line instead.
- `sh install.sh --version potluck-main-<12 hex>` installs that release instead of the newest.
- `sh install.sh --from <dir>` installs from a release's files you already downloaded (`manifest.json`, its
  signatures and the tarball for this machine).
- With curl missing, a wget that has `--https-only` works; one without it (BusyBox) needs `--version`.

## Upgrading

`potluck upgrade` checks the newest release with the keys built into the installed potluck and installs it
(`--release <dir>` from a directory, `--dry-run` to only check). The new build's preflight must accept this home
first, each replaced program is kept as `<name>.prev`, the drivers it replaced are re-pinned, and a running daemon is
restarted on it (or, if the new one does not come up, the old programs and the old daemon come back). `potluck` itself
is placed last, and one cut off part way (the Mac restarts) is finished by the next `potluck upgrade`, which starts the daemon
the first one stopped, and goes back to the programs before both if that does not come up.

### Daily upgrades

```sh
potluck upgrade enable    # install the daily job
potluck upgrade status    # on or off, where the job is, how the last run ended
potluck upgrade disable   # remove it
```

`enable` writes a LaunchAgent (`~/Library/LaunchAgents/sh.potluck.update.plist`) on macOS or a systemd user
timer (`~/.config/systemd/user/potluck-update.timer`) on Linux. It needs no root. About 03:00 (the minute is set
by the machine's name, so machines spread over the hour) it runs `potluck upgrade --dir <where potluck is
installed>`, which has the safeguards above: only a release signed by a trusted key, never an older one, and the
old programs and daemon come back if the new daemon does not start. Its output goes to `$POTLUCK_HOME/update.log`.
The directory potluck is installed in must be one you can write (the default `~/.local/bin` is). On Linux the
timer runs only while you are logged in unless you run `sudo loginctl enable-linger $USER`. The job needs no
token while the repository is public.

If another unit or agent on the machine already runs `potluck upgrade`, `enable` says so and changes nothing;
`disable` removes only the job `enable` wrote.

## Removing it

`potluck uninstall` takes off what the script and the first commands put on the machine: the programs and their
`.prev` copies, the PATH line in your shell's startup file, the daily update job, the host override file, the home (`~/.potluck`) and, on a Mac, the managed Softnet. It prints its plan first and asks; `--dry-run` only prints. What it does, in order, and what
it leaves for you to remove (Tailscale, the menu bar app and, unless you pass `--with-tart`, the Tart Potluck installed
and the images it pulled) is in [lending.md](lending.md#pause-leave-and-remove-everything).

On a Mac the managed Softnet's removal is a root step, which `uninstall` runs through sudo. An app without a
terminal runs `potluck uninstall --yes --no-sudo --json` instead: it does the steps before the root step and stops
there, answering

```json
{"done": ["stop sharing ..."], "admin_step": {"why": "...", "argv": ["/Users/you/.local/bin/potluck", "softnet", "uninstall", "--admin-step"]}, "left": ["remove the managed Softnet ...", "..."], "not_removed": ["..."]}
```

The app runs `argv` as root (see [softnet.md](softnet.md#without-a-terminal---no-sudo)) and runs the same
uninstall again, which goes on to the end and answers with no `admin_step` and an empty `left`. Every step is safe
to repeat. Without `--json` it says the root command in words. A step that fails exits non-zero with the same
answer plus `"error"`, that step first in `left`; when `--force` would let it go on, also `"force"`, with a question
to ask the person (`confirm`, `detail`) and the arguments to run (`args`).

`potluck --json uninstall --dry-run` answers the plan and changes nothing: `left` is every step, `pools_run` the pools
this machine runs, `warning` what removing the home loses for good, and `remove` the question to ask and the
arguments that run the plan. Every answer also has `view`, the same facts in plain words for a window: `heading`,
`lead`, `failed` (what to say when the root step failed), `warning`, the steps `done` and `left` (each with an `id`, and `will` and `did` words), `stays`, the box for
`--with-tart` (`choice`: its `label`, whether it is `on`, and the `args` that plan with it the other way) and
`details`, the terminal's words. The menu bar app's uninstall window ([menubar.md](menubar.md#uninstall-potluck)) is drawn
from it.

## From source

```sh
go build -o ~/bin/ ./cmd/potluck ./cmd/potluck-driver-tart ./cmd/potluck-conformance
```

In a git checkout, `potluck --version` names the commit. Put `~/bin` on your `PATH`.
