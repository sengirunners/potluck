# Release keys and how to rotate them

Every Potluck release carries a manifest of its tarballs' SHA-256 sums, signed with an Ed25519 key. This page says
which keys a build trusts, what checks a release with them, and how to replace the signing key without stranding any
install.

## Where the keys are

- **The trusted list:** `internal/upgrade/release_keys.txt`, one `ed25519:<base64>` public key per line. It is
  built into every potluck. Normally it holds one key; during a rotation, two.
- **install.sh** carries the same list as `RELEASE_KEYS`, because a first install has no potluck yet to check with.
  `TestInstallScriptCarriesTheReleaseKeys` fails if the two differ.
- **The signing key:** the private seed of one key on the list, only in the `potluck-release` environment's secret
  `POTLUCK_RELEASE_KEY` (deploying from main only, see [public-sync.md](public-sync.md)), plus one copy kept offline
  by the owners. Never in a repository, chat, issue, log or file left on disk.
- **Config `update_keys`** (`$POTLUCK_HOME/config.json`) adds keys on one machine, for a local or test channel.

## What checks a release

| check | keys it uses |
| --- | --- |
| `potluck upgrade` (`internal/upgrade.Stage`, called from `cli/upgrade.go`) | the built-in list plus config `update_keys` |
| `install.sh` when a potluck is installed | that potluck's own `potluck upgrade` |
| `install.sh` on a first install | `RELEASE_KEYS` plus `POTLUCK_UPDATE_KEY`, with OpenSSL 3 |
| the release workflow's "verify with the built-in keys" step | the new release's own potluck, with an empty home |

A release verifies when any trusted key signed it. An empty list, a line that is not a key or a key listed twice
refuses the upgrade with an error naming `release_keys.txt`; a malformed `update_keys` entry refuses it with an error
naming `config update_keys`. Nothing is installed. The release
workflow refuses to publish a release that its own potluck cannot verify, so a secret that matches no listed key
never publishes.

The daemon has no updater of its own: `potluck upgrade` is the only path, and it restarts the daemon on the new build.

These do not use the release key:

- **Driver pins** (`driver_pins.json`, [DRIVERS.md](../DRIVERS.md)) are SHA-256 sums of driver files. `potluck
  upgrade` re-pins only the files it placed from a release it already verified, so a pin's trust comes from that
  verification, not from a key of its own.
- **zm's release channel** uses the same verification code (`kit/release.Verify`, which takes a list of keys) with
  its own key and its own `update_keys`. Rotating Potluck's key does not touch zm's.

## Rotating the key

Do the steps in order. Each install only learns a new key by installing a release that lists it, so the next key is
listed first, used for signing only after installs have it, and the old one is dropped last.

1. **Make the next key**, on a machine you trust, outside any repository:
   `go run ./scripts/release-keygen -seed-file <path>`. It writes the seed to a new file readable only by you and
   prints only the public key. Copy the seed file to the offline backup. Do not set the secret yet.
2. **List it.** Add the public key as a second line of `internal/upgrade/release_keys.txt` and to `RELEASE_KEYS` in
   `install.sh` (space separated). Merge to main; outsiders get it when the private tree is next copied to the public
   repository ([public-sync.md](public-sync.md)). The next release is still signed with the current key, so every
   install takes it as before, and from then on trusts both keys.
3. **Wait for installs to take that release.** `potluck --version` on a machine names its release; it must be the
   release from step 2 or newer. Releases are tagged `potluck-main-<commit>`, so the commit tells you. Wait as long as
   you can: an install still older than step 2 refuses every later release ("does not verify with any configured
   update key") and needs `install.sh` again, or the next public key in its config `update_keys`.
4. **Sign with the next key.** Set the secret from the seed file without printing it:
   `gh secret set POTLUCK_RELEASE_KEY -e potluck-release -R <public repository> < <path>` (the file's trailing
   newline is fine: the signer trims it), then start the release
   workflow (`gh workflow run release -R <public repository> --ref main`). Its verify step proves the new secret
   matches a listed key. Installs from step 2 on take this release.
5. **Drop the old key.** Remove its line from `release_keys.txt` and from `RELEASE_KEYS`; merge and sync. Installs
   stop trusting it when they take the next release. Then destroy the old seed's offline copy and delete the step 1
   seed file from disk (the new seed's offline copy stays).

## If the signing key leaked

The overlap above assumes the old key is still safe. If it is not, anyone holding it can sign a release that every
install accepts until it has dropped that key, but only if they can publish it where installs look (the public
repository's releases) or hand someone a `--release` directory.

1. Stop signing with it: delete the `POTLUCK_RELEASE_KEY` secret from the `potluck-release` environment.
2. Yank any release you did not make: `gh release edit <tag> --prerelease -R <public repository>` (installs skip
   prereleases).
3. Do steps 1, 2, 4 and 5 at once: list only the new key, set the new secret, release. Installs older than that
   release still trust only the leaked key and refuse the new one: tell users to run `install.sh` again, and
   publish the new key's fingerprint somewhere other than the repository so they can check it.
