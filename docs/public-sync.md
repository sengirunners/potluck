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
scripts/public-sync.sh origin/main          # makes the commit, scans and prints it; pushes nothing
scripts/public-sync.sh origin/main --push   # the same, then pushes it to the public main
```

Before `--push`, read the gitleaks output and the changed file list. Test fixtures and public keys are expected.
Stop if you see a real secret, a person's address, a private repository's name or a machine's name: fix it on the
private main first, then sync again.

The public main is protected (the `go` check is required), so the push needs an admin. The public repository
refuses new branches and force pushes from anyone, and new tags from anyone but its release workflow, so a clone
whose `origin` still names the public repository cannot put private history there.

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
- Runners: `ci.yml`'s `go` job and `release.yml` run on a push to main on `vars.CI_ROUTE_LABEL` /
  `vars.RELEASE_RUNNER` or the Sengi label `sengi-standard-*`. Whichever runners serve them must accept this
  repository, or the required `go` check stays queued and no release is made. A self-hosted runner group serves it
  only when the group lists it by name; fork pull requests run on GitHub-hosted runners.
