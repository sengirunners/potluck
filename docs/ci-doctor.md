# Diagnose GitHub Actions jobs

`potluck ci doctor owner/repo [--json]` reads the broker's settings, GitHub
connection, workflows and queued jobs. It changes no GitHub or broker settings.
It prints 17 numbered checks. Each failure has one next step. Any failure makes
the command exit nonzero; warnings alone do not. JSON includes `reports` (each
with `repo`, `checked_at`, and `checks`), optional `warnings` and `skipped`.
Each check has `number`, `status` (`pass`, `warn`, `fail`), `message`, and `fix`.
`warnings` also say when `config.json` still sets `allow_public_repos`, which does
nothing any more (a public repository is served after `potluck runners allow
owner/name --public`).

Without a repository, it checks the connected repositories (`potluck runners
repos`), then the other repositories in the broker's job log, whose jobs came
here and were refused (check 5 says why). Name a repository to check any other.
Each command or self-check has a shared limit of 60 gh calls, pagination
included. When the budget runs out, the remaining checks are unavailable
warnings and the repositories not reached are listed in `skipped`.

The command and its API (`GET /v0/runners/doctor?repo=owner/repo`) are local to
the broker's machine. App checks use the configured App key. Workflow and
waiting-job reads use that machine owner's `gh` sign-in, as do the repository's
details when the App does not cover it. Sign in there with `gh auth login`.
Tokens are never passed to the broker over its API or included in diagnostics.
A missing sign-in is an unavailable warning, never a pass.

The checks ask the broker's own rules, never a copy of them:

- Check 4 also fails when the confirmed link can no longer use the pool's
  machines (its token was revoked).
- Check 5 asks the broker's admission of a delivery from the repository, named
  as GitHub names it now (a renamed repository's deliveries carry its new
  name), with its connection record, its id, and the owner's yes for a public
  one. Its next
  step is the command the refusal names (`potluck runners allow owner/name`,
  with `--public` for a public repository).
- Check 7 reads a public repository's approval rule for outside contributors
  through the App and judges it as `potluck runners repos` does.
- Check 10 asks the broker's event rule of each event a pool job's workflow
  runs on, for a run of the repository's own branches: `allow_fork_prs` serves
  every event of a private repository and never of a public one. A reusable
  workflow (`workflow_call`) has its caller's event. Where outside pull requests
  are refused, a `pull_request` job without the same-repository guard fails; a
  guard the doctor cannot prove is a warning. A fork-guarded `runs-on` (the
  form `potluck ci pr` writes, read by `internal/forkguard`, the same rule as
  the fork-safe lint) also counts as the guard when every event an outsider can
  start is a pull request event: as written, a fork's pull request runs on
  the hosted label. (A fork can rewrite `runs-on` in its pull request; the
  broker's refusal of fork jobs is what keeps those off the pool, except under
  `allow_fork_prs`.) Checks 9, 11 and 12 judge such a job by
  the labels own branches ask for. A guarded `runs-on` routed through
  `needs.<job>.outputs` is still dynamic.
- Check 9 warns about a pool job that names no OS: the broker places it on
  linux, as GitHub's own runners do. It fails when no machine could take it.
- Check 11 asks the broker's runtime rule
  ([workflow-job-limits.md](workflow-job-limits.md)) for each event the job
  runs on, over the workflow file at the default branch. Where the broker gives
  `job_ttl_s` instead of the job's `timeout-minutes` (a matrix or a reusable
  workflow call anywhere in the file, a dynamic or ambiguous job name, a pull
  request or other event whose workflow revision it cannot pin, a value it
  cannot read), that is a warning naming the reason.
- Check 12 asks placement (the same rule as `RunsOn`) for each pool job, with
  the longest of those runtimes plus the start-up allowance, and for the pool
  with any job. One doctor run reads the pool's machines once. Busy or cooling
  machines are a warning. So is a machine whose capabilities are unknown: its
  describe failed, or it is not listed now but served such a job before (a Mac
  asleep or offline; for the pool, any job before). A size, OS or image no
  machine could serve even when free fails.
- Check 13 reads the boxes' GitHub runner refresh result (describe's
  `runners`): a failed refresh fails, with the binding, image and reason.

Labels: a self-hosted job with only GitHub's default labels, or with another
fleet's labels, is not this pool's: it is never a failure or a missing
delivery (another fleet's labels are a warning). Check 16 examines queued and
in-progress runs: an in-progress workflow can still have a waiting job. Queued
pool jobs are compared with the broker's job log and its deliveries not applied
yet, read after GitHub's answers. A job queued less than two minutes ago is
not counted: its delivery may be on the way. Missing deliveries point to a failed
prerequisite check, or to GitHub's webhook delivery log when the cause is
unknown. Check 6 reports that this build serves a fork's own branches. If an
older daemon lacks the doctor API, the CLI reports check 6 as a failure with an
update and restart step.

- Check 15 reads the broker's own record of how it learns of waiting jobs
  (the `check` field of `GET /v0/runners/setup`): the installation's last
  round in which every list answered, and the lists that failed or were cut
  since. Webhook mode keeps a catch-up check, so both modes have one. It
  passes when that round is at most two catch-up intervals (or two check
  intervals, if longer) plus a minute and the last whole round's length old.
  It reads only the broker's record, so it still answers when GitHub does not
  (checks 1 and 2 unavailable): the owner's installation is then the one
  linked here.
  It fails only when rounds keep failing: failed rounds (or rounds that could
  not begin) one after another, with none complete between, for 30 minutes
  or three check intervals, whichever is longer (`failing_since` in the
  record; a sleep starts it again). One connected repository whose lists
  keep failing fails it for its whole installation. It also fails when no
  round has ended for that long while the daemon stayed awake as long
  (`awake_since`): a round is stuck, or the checker stopped. A failure more
  recent than that, a round cut by the checker's budget or GitHub's rate
  limit, and an old round with nothing since while the daemon has not been
  awake as long (a Mac that just woke) are warnings.
- Check 17 compares the build the daemon runs with the release this machine
  installed last through the release channel. An older release build still
  running is a warning with a restart step; a build made from source (`go
  build` or `go install` in a checkout) is a warning that says a restart runs
  it again; a build with no installed release to compare is `unavailable`. It never says a build is the newest: the doctor does not read
  the channel. It fails when the in-memory runner settings differ from disk.

Missing producers stay visible as `unavailable` warnings:

| Check | Missing fact |
| --- | --- |
| 14 | Image Xcode inventory |

Every five minutes the broker runs checks 3, 4, 12, 13, 15 and 16 on the
connected repositories. Each run starts where the last one stopped, so a list
longer than one run's gh budget is covered in turn. It records results in
`runners_doctor` and sends a notice through the daemon's owner notification
hook on a transition into failure: once per repository (3), per installation
(4, 15) or for the pool (12, 13). Check 16 is recorded per repository, but its
cause is the installation's: while one repository's failure of it was told
(and checked again within the hour), another repository of that installation
starting to fail it is not told again. Likewise a check 15 failure that reads
the same as another installation's told within the hour (one cause, such as
the broker's rounds not beginning at all) is not told again. A warning neither notifies nor resets
that. Results survive restarts; one not checked again for a week (a
repository disconnected) is deleted. When embedded in zm, that hook supplies
the person's For-you surface; standalone Potluck logs a notice when no
notifier is registered. The broker does not run workflow lint on this timer.

## Check 18: the probe

`potluck ci doctor owner/repo --probe` runs the 17 checks, then prepares a
probe of the whole path: a generated one-job workflow
(`.github/workflows/potluck-probe.yml`) on a throwaway branch
`potluck/probe-<time>-<random>`. The branch is one commit with no parent that
holds only that file, so the push starts the probe and no other workflow
(deleting the branch afterwards still starts the default branch's workflows
that run `on: delete`, if there are any). Its
job runs on a push to that branch only, with no token permissions and no
checkout, asks for this machine's pool labels (or `--labels`, `--size`,
`--os`, as `potluck ci pr` takes them), and prints the machine's OS, macOS
version, Xcode and free disk.

The GitHub App cannot write to a repository, so the command does not push the
probe. It writes the workflow and a script, prints the script, and its next
step is `sh <script>`. The script uses your own `gh` sign-in (pushing a
workflow file needs gh's workflow scope: `gh auth refresh -s workflow`). It
pushes the branch, waits up to 20 minutes for the run to finish, prints what the
job reported (or says the log did not come back), and deletes the branch, also when the job fails, never starts, or
the script is stopped (interrupt, terminate or hang-up); a run still waiting
then is cancelled, and a cancel GitHub refuses is reported. When it runs to
the end it prints `pass:` or `fail:` with the next step; it exits nonzero on
any failure or stop. Once running it removes the directory it was written in when it ends, so after a
failure run `potluck ci doctor owner/repo --probe` again for a new script (a script
refused for its wait or poll leaves the directory).
`POTLUCK_PROBE_WAIT` and `POTLUCK_PROBE_POLL` (whole seconds, the poll at least 1)
change the wait and the poll; anything else is refused before it pushes. The commit is
sent as JSON, so an older `gh` works. When GitHub does not accept the branch the
script prints gh's own error: a missing workflow scope, a ruleset and an outage
all look like that, and a branch that was made anyway still has its run
cancelled and is deleted.

Check 18 is a warning while the probe is ready and not run. It is not prepared
when any other check failed, or when the daemon gave no checks for the
repository: its job would wait on that failure. It fails when your `gh`
sign-in cannot see the repository (GitHub answers not found) or cannot push to
it, when `--labels` does not name this machine's pool (another machine would
take the job), and on a public or internal repository: there the branch and the
job are visible to anyone, or to everyone in the enterprise, so the probe
needs the repository owner's yes first, given with `--public`. That yes is for the
probe's branch and job only; it is not stored, and serving the repository's
jobs still needs the stored yes check 5 reads (`potluck runners allow
owner/name --public`). With `--json` the script
is not printed; check 18's `fix` is the command that runs it.
