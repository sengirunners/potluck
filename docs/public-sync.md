# Public sync

Potluck has two repositories:

- `sengirunners/potluck-private`: where the work happens. Branches, pull requests, history and the release
  environment's original setup live here.
- `sengirunners/potluck`: the public repository. Its main is a series of single commits, each a copy of one
  private main commit's tree, made by the project identity `Sengi, Inc. <ops@sengi.run>` and signed off by it.

The copy goes one way, private to public. Nothing else goes over: no history, branches, tags or pull request refs,
because the private history carries personal addresses.

## When

Once per release: when a private main commit should reach people who install Potluck. A push to the public main
starts its CI and its release workflow, which publishes the signed release that `install.sh` and `potluck upgrade`
install.

## How

From a clone of the private repository, with `origin` pointing at `potluck-private`:

```sh
git fetch origin
git switch --detach origin/main             # run the script that is in the commit you publish
scripts/public-sync.sh origin/main          # makes the commit, scans and prints it; pushes nothing
scripts/public-sync.sh origin/main --push   # the same, then pushes it to the public main
```

The script fetches the private main itself and refuses to run unless it is the same file as the
`scripts/public-sync.sh` in the commit being published, so a checkout that is behind cannot publish with older
rules. An older checkout may carry a script without that check: fetch and switch as above first.

The script refuses a commit that does not contain the private commit the public main already carries: it copies
the whole tree, so publishing an older commit would undo everything merged since. When two people publish close
together, the second publishes `origin/main` again after `git fetch origin`.

Before `--push`, read the gitleaks output and the changed file list. Test fixtures and public keys are expected.
Stop if you see a real secret, a person's address, a private repository's name or a machine's name: fix it on the
private main first, then sync again.

The public main is protected (the `go` check is required), so the push needs an admin. The public repository
refuses new branches and force pushes from anyone, and new tags from anyone but its release workflow, so a clone
whose `origin` still names the public repository cannot put private history there.

## Holds: a security fix reaches our machines first

A published fix shows where the flaw was. So a fix for anything in `SECURITY.md`'s scope (a guest reaching the
host, the network or another guest; a secret reaching the wrong place; a VM outliving its lease; a forged handle)
is installed on every machine we run for other people before it is published.

- The pull request that makes the fix also adds a file under `public-hold/` at the top of the repository, named
  for the fix, saying in a line or two what must be installed where. One file per fix: a later fix gets its own
  file, never a line in an existing one, because removing a file releases everything it stood for.
- `scripts/public-sync.sh --push` refuses while the commit being published, or the private main, carries a hold.
  Any file under `public-hold/` is a hold, a README too. So is any path with `public-hold` in it under another
  spelling (`Public_Hold`, `publichold`, a folder elsewhere in the tree), so a misnamed hold still holds.
  The whole tree goes over in a sync, so one hold stops every release; install the fix soon.
- Install from the private main: build the programs from that commit and put them on the machines. A public
  release cannot be the way they get it.
- When the fix is on those machines, a pull request removes the hold file and says where the fix was installed.
  The next sync publishes everything. No hold file goes over, because the script never publishes a commit that
  carries one.

The script cannot tell that a fix needed a hold and did not get one. That is for the fix's author and reviewer.
It also cannot stop a checkout whose script is from before holds existed.

The hold is for machines we run for other people. People who run Potluck on their own machines get the fix from
the release, so publish as soon as the hold is off.

## Outside contributions

A pull request on the public repository is reviewed there. To take it, apply its diff to the private repository as
a private pull request (credit the author in the commit message with `Co-authored-by:`), merge it, and close the
public pull request with a link to the release that carries it. Never merge on the public repository: its main would
then differ from the private tree and the next sync would undo the change.

## Settings the public repository keeps

- Actions: approval required for fork pull request runs from all outside collaborators; default workflow token
  read-only.
- Private vulnerability reporting on (`SECURITY.md`).
- Branch protection on main: the `go` check required.
- Environment `potluck-release`, deploying from main only, holding the secret `POTLUCK_RELEASE_KEY`.
- Tag rulesets: "tags only from the release workflow" refuses creating, moving or deleting any tag but
  `refs/tags/potluck-main-*` and `refs/tags/mcp-bundle-v*` (the MCP bundle's releases, `mcp-registry/README.md`);
  "only project-identity commits" holds those tags to the project identity's commits.
- Runners: `ci.yml`'s `go` job and `release.yml` run on a push to main on `vars.CI_ROUTE_LABEL` /
  `vars.RELEASE_RUNNER` or the Sengi label `sengi-standard-*`. Whichever runners serve them must accept this
  repository, or the required `go` check stays queued and no release is made. A self-hosted runner group serves it
  only when the group lists it by name; fork pull requests run on GitHub-hosted runners.
