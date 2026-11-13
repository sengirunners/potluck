# Move one job onto the lent Mac: `potluck ci pr`

```sh
potluck ci pr <owner/repo> [--job <id>] [--workflow <file>] [--dry-run] [--json]
              [--labels a,b,c | --size <size> --os <os>]
```

It changes one job's `runs-on` in a repository's workflow so the repository's
own branches run that job on the Mac this machine lends, and prepares the
change as a pull request. Pull requests from forks keep running on the
GitHub-hosted runner the job uses today.

## What it writes

A job that says `runs-on: macos-15` becomes

```yaml
runs-on: ${{ fromJSON(github.event.pull_request && github.event.pull_request.head.repo.full_name != github.repository && '["macos-15"]' || '["self-hosted","macos","<pool label>"]') }}
```

- For a pull request whose head repository is not this one (a fork, or a
  deleted fork), the guard is true and the job asks for `macos-15`.
- For a push, a dispatch, a schedule, or a pull request from one of the
  repository's own branches, the guard is false and the job asks for
  `self-hosted`, the OS and the pool's label (`<label>:<size>` with `--size`).
  The broker places a job that names no OS on linux, so the OS is always
  written (`--os`, default `macos`).

The guard alone does not keep a fork's code off the Mac. A `pull_request` run
uses the workflow file from the pull request's merge commit, so a fork can
change `runs-on` to the Mac's labels. What stops that run is the broker: it
refuses jobs from forks (`runners/jobs.go`) and the Mac never gets them. That
refusal is off for a private repository whose owner turned on
`allow_fork_prs`; there a fork's pull request can reach the Mac.

The labels are this machine's pool's, from the daemon (`potluck runners
setup`), checked with the broker's own label rule; `--labels` gives them
directly and needs no daemon; they must name the OS.

Only that one value changes. Comments, quoting, key order and line endings
stay as they are; the edit is checked to change nothing else in the decoded
workflow. Undo by putting the old label back.

The command refuses, saying why, when the job does not exist, calls a reusable
workflow, has no `runs-on`, or its `runs-on` is not one GitHub-hosted label on
one line: a matrix value, another expression, an existing guard with other
labels, a runner group or `labels:` mapping, several labels, a YAML anchor or
alias, a YAML tag, a block scalar, a value on the line after `runs-on:`. It
also refuses a workflow with no `on` key (its triggers are unknown), and one
with any trigger an outsider can start other than
`pull_request` (`issue_comment`, `workflow_run`, `merge_group`, ...: there
the guard is always false; and `pull_request_review` or
`pull_request_review_comment`, where an outsider's review of a pull request
from the repository's own branch would start the job on the lent Mac). Keys
are matched without regard to case (`On:` is `on`, `Runs-On:` is `runs-on`),
and an unquoted `on` read as YAML 1.1's `true` still counts as `on`. A job
that already has exactly this value is reported as `done`. `--workflow` and
the files it searches must be named with letters, digits, `.`, `_` and `-`.

Without `--job` it takes the one job that runs on a GitHub-hosted macOS label (`macos-15`, `macos-latest`, ...;
as a scalar or a one-element list, or already guarded), in every workflow file or in the one `--workflow` names. It
refuses when there is none or more than one, and names them.

Without `--workflow` it looks for the job in every file in `.github/workflows`
at the default branch and needs exactly one to have it.

## The answer with `--json`

`--json` prints one object and nothing else on stdout (a refusal is still an error on stderr):

| field | |
| --- | --- |
| `state` | `done` (the job already has this change), `merge` (a pull request with it is open), `script` (prepared: run the script) or `dry_run` |
| `repo`, `job`, `workflow`, `labels` | what was decided: the job's id (named or picked), its file, what own branches ask for |
| `url` | `merge`: the open pull request |
| `script`, `next` | `script`: the script's path, and `next`, the command line (`sh <script>`); `dry_run`: `next` is the command that prepares it |
| `diff` | the change to the workflow, unified; absent for `done` |
| `notes` | what it noticed on the way (files skipped, lint problems the file already had) |

## How the pull request is opened

The workflow is read with your `gh` sign-in. The GitHub App this machine uses
has Contents: read, Actions: write, Administration: write and Members: read
(`ghapp` `Manifest.permissions`). Changing a workflow file through the API
takes Contents: write and Workflows: write, and opening a pull request takes
Pull requests: write, so the command does not use the App for this. It prints
the diff, then writes the new file, the request bodies and a script
(`open-pr.sh`) to a new temporary directory and prints the script. The last
line is `next: sh <script>`. The script, run with your `gh` (it needs the
`workflow` scope: `gh auth refresh -s workflow`):

1. creates the branch `potluck/runs-on-<job>` at the default branch's head
   (or `-2`, `-3`, ... when an old branch of that name has no open pull
   request),
2. commits the new file there with the contents API, against the file's
   blob, so a file changed since the command read it is refused by GitHub,
3. runs `gh pr create`.

The script deletes its temporary directory when it ends, also when a step
fails; after a failure, run `potluck ci pr` again.

When a pull request from that branch is already open, the command prints its
URL and stops. `--dry-run` prints the diff and the command line to run next,
and writes nothing.

Before the diff it runs the fork-safe lint (`internal/workflowlint`) over the
file before and after the edit. A problem only the edited file has stops the
command; problems the file had already are printed as notes.

## Is `fromJSON` in `runs-on` allowed?

What GitHub's documentation says (checked 2026-11-12):

- `runs-on` takes a string, a variable holding a string, an array of strings,
  or a `group`/`labels` mapping ("Choosing the runner for a job").
- `jobs.<job_id>.runs-on` may use the `github`, `needs`, `strategy`,
  `matrix`, `vars` and `inputs` contexts (the contexts reference's
  availability table). `fromJSON` is a general function, not one of the
  special functions that table limits.
- `fromJSON` returns the JSON value of a string, arrays included; the
  documented example feeds a matrix.
- `&&` binds tighter than `||`, and this repository's own CI already uses the
  string form of the guard (`GUARD && 'ubuntu-latest' || ...`) in `runs-on`.

The documentation shows no example of `fromJSON` returning an array for
`runs-on`. The form is widely used, but a job placed on a lent Mac through it,
and a fork pull request kept on `macos-15` through it, have not been run live
here yet. That live check has not been run yet.
