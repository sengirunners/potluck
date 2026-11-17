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

- A release lists the SHA-256 of each tarball in a manifest, and the manifest carries an Ed25519 signature.
- If potluck is already installed, it checks the signature with the keys built into it before anything from the
  release runs. That is only as good as the first install of that potluck.
- On a first install the script checks the signature itself, with the release keys it carries and an openssl that
  can verify Ed25519 (OpenSSL 3 or later). macOS's own LibreSSL cannot, and Homebrew's `openssl@3` is keg-only, so
  it is not on `PATH` after `brew install openssl@3`: run
  `curl -fsSL https://raw.githubusercontent.com/sengirunners/potluck/main/install.sh | PATH="$(brew --prefix openssl@3)/bin:$PATH" sh`
  instead.
- With no such openssl, the script says the signature was not checked, trusts the download over HTTPS, and still
  checks the tarball against the manifest. The potluck it installs then checks the manifest's signature against the
  keys built into itself, but those came in the same download, so it adds no independent proof. To get the full
  check, remove the installed programs and run the script again with an OpenSSL 3 first on `PATH`; once potluck is
  installed, the script hands the release to that potluck instead.

Which keys are trusted, and how the signing key is replaced: [release-keys.md](release-keys.md).

### Options

- With no token the script makes no GitHub API call: it downloads the newest release from github.com's release
  links, which GitHub's anonymous API limit (60 calls an hour per address, shared by everyone behind one office, CI
  or cloud NAT) does not count.
- `GH_TOKEN` (or `GITHUB_TOKEN`), if set, makes the script read the release through GitHub's API with that token. A
  private repository needs one that can read it (fetch the script with
  `curl -fsSL -H "Authorization: Bearer $GH_TOKEN" …`). A token GitHub refuses fails the download; it is not ignored.
- `sh install.sh --from <dir>` installs from a release's files you already downloaded. It needs `--trust-dir` when
  no signature check can be made.

## Upgrading

`potluck upgrade` checks the newest release with the keys built into the installed potluck and installs it
(`--release <dir>` from a directory, `--dry-run` to only check). The new build's preflight must accept this home
first, each replaced program is kept as `<name>.prev`, the drivers it replaced are re-pinned, and a running daemon is
restarted on it (or, if the new one does not come up, the old programs and the old daemon come back).

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

## From source

```sh
go build -o ~/bin/ ./cmd/potluck ./cmd/potluck-driver-tart ./cmd/potluck-conformance
```

In a git checkout, `potluck --version` names the commit. Put `~/bin` on your `PATH`.
