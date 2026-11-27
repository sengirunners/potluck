# Release keys and how to rotate them

Every Potluck release carries a manifest of its tarballs' SHA-256 sums, signed with an Ed25519 key twice: raw
(`manifest.json.sig`) and as SSHSIG (`manifest.json.sshsig`, namespace `potluck-release`, which `ssh-keygen -Y verify`
reads). This page says
which keys a build trusts, what checks a release with them, and how to replace the signing key without stranding any
install.

## Where the keys are

- **The trusted list:** `internal/upgrade/release_keys.txt`, one `ed25519:<base64>` public key per line. It is
  built into every potluck. Normally it holds one key; during a rotation, two.
- **install.sh** carries the same list as `RELEASE_KEYS`, because a first install has no potluck yet to check with.
  `TestInstallScriptCarriesTheReleaseKeys` fails if the two differ. For ssh-keygen it turns each key into an
  allowed_signers line itself, so there is no second copy to keep in step.
- **The signing key:** the private seed of one key on the list, only in the `potluck-release` environment's secret
  `POTLUCK_RELEASE_KEY` (deploying from main only, see [public-sync.md](public-sync.md)), plus one copy kept offline
  by the owners. Never in a repository, chat, issue, log or file left on disk.
- **Config `update_keys`** (`$POTLUCK_HOME/config.json`) adds keys on one machine, for a local or test channel.

## What checks a release

| check | keys it uses |
| --- | --- |
| `potluck upgrade` (`internal/upgrade.Stage`, called from `cli/upgrade.go`) | the built-in list plus config `update_keys` |
| `install.sh` | `RELEASE_KEYS` plus `POTLUCK_UPDATE_KEY`: the SSHSIG with ssh-keygen, else the raw signature with OpenSSL 3 ([install.md](install.md#what-is-checked)) |
| `install.sh` when a potluck is installed | the above, then that potluck's own `potluck upgrade` |
| the release workflow's "verify with the built-in keys" step | the new release's own potluck, with an empty home |
| the release workflow's "install with install.sh" step | `install.sh` with only the system's tools, which must check the SSHSIG |

A release verifies when any trusted key signed it. An empty list, a line that is not a key or a key listed twice
refuses the upgrade with an error naming `release_keys.txt`; a malformed `update_keys` entry refuses it with an error
naming `config update_keys`. Nothing is installed. The release
workflow refuses to publish a release that its own potluck or install.sh cannot verify, so a secret that matches no
listed key never publishes.

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

## Apple's signing keys

Separate from the release key, and not needed for a release. With them, the release workflow signs the Mac programs
and `Potluck.app` with a Developer ID certificate, and `.github/workflows/app.yml` then publishes `Potluck.zip`: the app
with that release in it, signed, notarized by Apple and stapled. `Potluck.zip` is never published otherwise.

The repository variable `POTLUCK_APPLE_SIGNING` turns it on (`on`). Without it the release workflow says so and passes,
nothing of Apple's runs, and the app ships ad hoc signed. Deleting it turns signing off again: the quick way out when
Macs or Apple's services hold up releases (a run that already waits for a Mac must be cancelled; run the workflow again). But a lender's signed app that a later release replaces with an ad hoc one may lose its login item.

They are in the GitHub environment `potluck-apple` of the repository the releases run in:

| name | kind | what it is |
| --- | --- | --- |
| `POTLUCK_SIGN_KEY` | secret | the Developer ID Application certificate's private key: unencrypted RSA PEM |
| `POTLUCK_NOTARY_KEY` | secret | an App Store Connect API key's `AuthKey_*.p8` text |
| `POTLUCK_SIGN_CERT` | variable | the certificate, then Apple's intermediate that issued it, both as PEM (public) |
| `POTLUCK_NOTARY_KEY_ID` | variable | that API key's id (not a secret) |
| `POTLUCK_NOTARY_ISSUER` | variable | its issuer id (not a secret) |

With one missing the `sign` job fails, and the release ships as with none.

Notarizing uses an API key, not an Apple ID's app-specific password: `notarytool` reads the API key from a file, while
a password goes in its arguments or is typed. An API key also belongs to the team, not a person, and can be limited
and revoked by itself. The two are told apart by `macos/PotluckMenu/sign.sh`; a person's own Mac may use either
([menubar.md](menubar.md#signing-and-notarizing)).

### Setting them up

1. **Make the environment first**, with main as its only branch, before any secret. (A workflow that names an
   environment that does not exist makes one with no rule at all.) No required reviewers and no wait timer: a release would wait for them.

   ```sh
   repo=<public repository>
   gh api -X PUT repos/$repo/environments/potluck-apple -F 'deployment_branch_policy[protected_branches]=false' \
     -F 'deployment_branch_policy[custom_branch_policies]=true'
   gh api -X POST repos/$repo/environments/potluck-apple/deployment-branch-policies -f name=main -f type=branch
   gh api repos/$repo/environments/potluck-apple/deployment-branch-policies --jq '.branch_policies[].name'   # main, only
   ```

2. **The certificate.** The Apple team's Account Holder makes a "Developer ID Application" certificate (developer.apple.com,
   Certificates) from a certificate signing request whose private key you hold, and gets a `.cer` file. Download
   Apple's "Developer ID - G2" intermediate (apple.com/certificateauthority). Then, with both as PEM in one file:

   ```sh
   { openssl x509 -inform der -in developerID_application.cer; openssl x509 -inform der -in DeveloperIDG2CA.cer; } > cert.pem
   gh variable set POTLUCK_SIGN_CERT -e potluck-apple -R $repo < cert.pem
   gh secret set POTLUCK_SIGN_KEY -e potluck-apple -R $repo < <private key PEM>
   ```

   A key in a `.p12` comes out with `openssl pkcs12 -in <file>.p12 -nocerts -nodes` (into a file only you can read,
   deleted after).
3. **The notary key.** App Store Connect, Users and Access, Integrations, Team Keys: make a key with the Developer role.
   Its `.p8` can be downloaded once.

   ```sh
   gh secret set POTLUCK_NOTARY_KEY -e potluck-apple -R $repo < AuthKey_<key id>.p8
   gh variable set POTLUCK_NOTARY_KEY_ID -e potluck-apple -R $repo --body <key id>
   gh variable set POTLUCK_NOTARY_ISSUER -e potluck-apple -R $repo --body <issuer id>
   ```

4. **Turn it on**, last: `gh variable set POTLUCK_APPLE_SIGNING -R $repo --body on` (a variable of the repository, not
   the environment).
5. **Check it** on the next release that a push to main makes: the `sign` and `try` jobs pass, then the `app` workflow
   does, and the release has `Potluck.zip`. While signing fails there is one open issue, "Signing the Mac app and
   programs failing"; while the zip does, "Potluck.zip failing". A failed `app` run can be re-run (all jobs: what they hand on is kept a day). A release whose `sign`
   failed is out unsigned and stays so: do not re-run it, the next push signs. A release started by hand
   (`workflow_dispatch`) is not signed with Apple's key and has no `Potluck.zip`.

Keep one offline copy of the private key with the owners, as for the release key. When the certificate expires or the
team changes, replace `POTLUCK_SIGN_CERT` and `POTLUCK_SIGN_KEY` together. Programs already signed keep working: the
signatures carry Apple's time. If the key leaks, the Account Holder revokes the certificate (macOS then refuses what it
signed) and the secret is replaced; a leaked notary key is revoked in App Store Connect.

### What can read what

- Only the `sign` job (release.yml) and the `notarize` job (app.yml) name the environment: on GitHub-hosted Macs, with a
  read-only token, for a push to main. (app.yml runs only when release.yml's own token started it.) No pull request and
  no other branch can.
- `sign` is given the signing key alone; `notarize` both keys. Each is in the environment of one step, the one that
  runs `signing-keychain.sh open`, which takes it out of its environment before it starts any program. From then it is
  in a temporary keychain and, for the notary key, a file only that user reads; `signing-keychain.sh close` removes
  them even when signing failed. Nothing secret is in a program's arguments.
- Those jobs run `sign.sh` and `signing-keychain.sh` from the workflow's own commit, Apple's tools, and GitHub's
  checkout and artifact actions pinned by commit, checked out after the files to sign are
  unpacked, in directories of their own with no links. They run nothing of the commit being released: the app and programs
  they sign are only read. But they do sign what those earlier jobs built from it.
- The jobs that build (`menubar`, `programs`, `fullapp`) and the one that runs the signed programs (`try`) run the
  released commit's code, hold no Apple secret and can write nothing. `publishapp` can write to the repository, has no
  Apple secret, and runs only `publish-app.sh` and Apple's checks.
- The `release` job builds the Mac programs itself as well, and takes a signed one only when the signature is all that
  differs from its own (`scripts/release/signed.go`); else it ships its own. So the release key still vouches only for
  what that job built, and the manifest's SHA-256 sums cover the signed files. The app's own program is not compared.
