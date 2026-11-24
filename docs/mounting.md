# Mounting Potluck's commands in another program

A program built on Potluck (its daemon is `kit/server` with `wiring.Compute` and modules of its own) can carry
Potluck's commands in its own command tree, instead of keeping copies of them. `cli.Commands` builds them for
that program; the `potluck` program builds its own tree the same way.

## The host

```go
type Host struct {
	Prefix string                                         // how a person types Potluck's root here
	JSON   func() bool                                    // the host's --json
	Pool   func() string                                  // the host's pool (project) flag
	Client func() (*client.Client, *config.Config, error) // the host's client for this machine's daemon
	RunnersRoute func(ctx context.Context, home string, st runners.Setup) (string, error) // optional
	Stdin  io.Reader
	Stdout, Stderr io.Writer
	AfterLend *AfterLend                            // optional
	AfterJoin func(w io.Writer, r api.JoinResponse) // optional
}

type AfterLend struct {
	Run             func(w io.Writer)
	Skip, SkipUsage string // the flag that skips it ("no-auto-update"), and its help
}

func Commands(h Host) []*cobra.Command
func Report(h Host, err error) int
func ExitCode(err error) (code int, show error, ok bool)
var ProgramCommands = []string{"daemon", "mcp", "status", "upgrade", "uninstall"}
```

- **Prefix** is how a person types Potluck's root in the program: `potluck` for the `potluck` program; `supper pool`
  for a program `supper` that adds the commands under its word `pool`; `supper` for one that adds them at its top
  level. It is words of letters, digits, `.`, `_` and `-` with one space between (`Commands` panics on anything
  else), since hints and generated scripts carry it unquoted. Every hint a command prints that names a Potluck command is the prefix and that command's words
  (`supper pool host stop`). The prefix's first word is the program's name: it starts the lines a command prints
  about itself (`supper: sent TERM`) and names the commands each program has of its own (`supper daemon
  --detach`). One exception: the commands `invite` prints for the friend who gets the code are the `potluck`
  program's (`potluck join '<code>'`), since a friend's machine needs only Potluck, with the host's spelling of
  the same command beside them, whole on one line (`supper pool join '<code>'`; `friendCmd`, `otherSpelling` in
  `cli/pool_members.go`). A team project's invite (`invite <project> --as <name>`) prints the host's own spelling,
  since a member of it uses the host program there.
- **JSON** and **Pool** read the host's own global flags. The commands define no persistent flags, so they sit
  under any parent without adding flags to it. Pool, when given, is applied to a copy of the client.
- **Client** is the host's client for this machine's daemon and its client config, whose home the commands read and
  write files in (driver pins, `doctor`'s and `host`'s state directory, the package shelf); a config with no home is
  refused. The host decides which daemon, whether to start one, and any remote mode. Commands that only read
  (`host status`, which the menu bar item asks every few seconds, `doctor`, the `isolation` listing) use a copy of that client
  with its autostart off. The rule in [daemon.md](daemon.md#one-daemon-per-machine) for which daemon `potluck`
  uses is the `potluck` program's alone.
- **RunnersRoute** (optional) is for a host whose own proxy fronts the runner broker, such as a tunnel it set
  up: `runners setup` calls it once the broker listens, to point that proxy at the listener (`st.Listening`). What
  it returns is printed after setup's line and is `route` in setup's `--json`; its error fails the command (the
  setup is saved and the listener open by then, so running setup again once it is fixed is safe).
- **AfterLend**, when set, is the host program's own step once a `host` run lent this machine (at least one size
  shared; never in a dry run), such as turning on its daily update. It runs once per run, its words on stdout (stderr
  under `--json`). `Skip` adds that flag to `host` (and its old spelling `host share`); given, the step does not run.
  Potluck knows nothing else of it.
- **AfterJoin**, when set, is the host program's next step after `join` joined a team project that is not one of a
  pool's areas (what to do there in that program), its words to stdout. It is not called under `--json`.
- **Report** prints an error a command returns as `potluck` does, on the host's stderr, the message as written (an API
  error as JSON with `--json`; Potluck's hints for a daemon that is not running or a tailnet name that is taken), and returns
  the exit code: a sandbox command's own, 3 when the daemon is not running, 2 for a usage error, else 1.
- **ExitCode** is for a host that reports errors itself: it maps an error to a sandbox command's own exit code
  (`sandbox exec`, `run`), or the code of a refusal whose output already said why. Any other error is the host's to
  report.

`Commands` never calls `cli.UseHome` and never changes `config.Program`, `config.Prefix`, `config.Lacks` or `config.Names`: the host keeps its own
home, machine record and home claim. Build the commands anew for each run of the host (they hold that run's
state). A host can add the same command at two places, built twice with two prefixes: `supper pool sandbox` and
`supper sandbox` are one command with two spellings.

## What a host gets

Every top-level command of `potluck` except `ProgramCommands`: `create`, `invite` (with `ls`, `rm`), `join`, `leave`, `forget`,
`remotes`, `doctor`, `isolation`, `host` (with `status`, `set`, `pause`, `resume`, `stop`, `shelf`), `ci`, `sandbox`, `usage`,
`sealing`, `run`, `agent-task`, `agent-runner`, `runners`, `secrets` and `driver`.

`daemon`, `status`, `upgrade`, `uninstall` and `mcp` are not mounted: they run, report on, replace and remove one
program's daemon and binary, or serve that program's MCP tools, and a host has its own. A hint that names one uses
the program's name (`supper status`).

## Linking an installation from the program

A program whose daemon embeds the runner broker (`wiring.Compute`) may have its own way to know who runs a GitHub
account, such as its own GitHub sign-in when the App is installed. It then links that installation to a pool area
in-process, with no browser step:

```go
func (s *runners.Service) LinkVerified(ctx context.Context, installation int64, area, by string) (runners.Link, error)
```

The program's module gets the `*runners.Service` from the modules `wiring.Compute` returns. There is no route for
this: only code compiled into the daemon can call it, so no network caller and no area admin can skip the link page
([daemon.md](daemon.md#links)) through it. What that page proves (the person signing in administers the
installation's account, and said yes to this area) is the program's to have proved; `by` names who did, and is the
link's `linked_by`. The rest is the page's own: the installation must be this App's (`not_found` otherwise), its
account must be in `runners.accounts` when that is set (`forbidden`), and the link mints the same broker identity
token, capped to the pool project and the area, so the area's budget and ledger apply. An installation already linked
to another area is refused (`conflict`; its admin unlinks it first); one already linked to the same area is answered
as it is. The App needs no Members read for it.

## Seeing, holding and ending the broker's jobs

The same program reads, holds and ends jobs in-process (`runners/embed.go`, `runtarget/rt/rt.go`); none of it has a
route.

```go
func (s *runners.Service) Job(ctx context.Context, id int64) (runners.Job, error)
func (s *runners.Service) Jobs(ctx context.Context, installation int64, limit int) ([]runners.Job, error)
func (s *runners.Service) EndJob(ctx context.Context, id int64, why string) error
rt.From(d).OnBoxRequest(func(ctx context.Context, project, area string, req rt.BoxRequest, live func() ([]computeapi.RunTargetHandle, error)) error)
```

- `Job` is one job as the broker recorded it: id, run, repository, installation, linked area, labels, state and
  why, the binding and handle of the box it runs on (or waits to start on), and GitHub's `started_at` and
  `completed_at` (when the broker saw the start or end, where GitHub gave no time). A job the broker never had is `not_found`. `Jobs` lists one installation's, most recently changed
  first.
- `OnBoxRequest` is `OnBoxAcquire` with the request: the binding, its size as its driver describes it, whether it is
  the start of a stopped box, and, for a box the broker acquires, the job (`req.Job`: id, repository, installation,
  labels). Only code in the daemon can set `req.Job`. The live boxes `live` reads carry their binding and size name;
  the binding's describe (`List` with describe) gives that size's CPUs and memory. A broker job's box is asked about
  twice per try: once before the runner is minted (`BoxAllowed`), once by the acquire itself. Subscribe while
  wiring, before the daemon starts; `OnBoxAcquire` subscribers (the usage module's limits) are asked as before.
- A subscriber that refuses a broker job's box holds the job: the broker asks before it mints a runner, so nothing
  is minted; the job stays `waiting` with the refusal on its row, and is asked again on the waiting serve's backoff
  (at most `WaitMax`, 5 minutes, apart, and when a box is freed). The binding is not counted as refusing, so other
  areas' jobs go on being served there. GitHub cancels a job queued for a day, and the broker gives up on it then.
- `EndJob` ends a job running on a pool box: its box is released, so GitHub fails the job, and the broker neither
  serves it again nor re-runs it. `why` is written on its row. A job not running yet, or being ended for another
  reason (its time limit), is `conflict`. A release the host does not answer is the error, and the job is as it
  was until the program asks again (if the host did release the box, the broker then takes its end for a loss and
  re-runs the job). A job that ran on a runner minted for another job leaves that job queued, served on a new box.

## Kit's messages

Messages from kit and the compute packages (an API error's hint, a refusal from the daemon or a driver) name
commands through `kit/config/program.go`:

- `config.Command` names one of the program's own commands (kit's team, tunnel and tailnet commands, `daemon`,
  `status`): `config.Program` and Potluck's words (`supper join <code>`).
- `config.PotluckCommand` names one of Potluck's commands (`host`, `doctor`, `sandbox rm`, `driver pin`):
  `config.Prefix`, or `config.Program` when the prefix is empty (`supper pool doctor`).

A host sets `config.Program` to its name and `config.Prefix` to its `Host.Prefix` first in main, before it prints
anything, builds its commands (some flag help is made while the tree is built) or starts a daemon. It sets
`config.Lacks` when it carries only some of the commands kit names: the `potluck` program answers from its own
command tree, so a command it has none of, such as `tunnel setup`, is named as missing. The daemon passes its prefix
to the drivers it starts (`POTLUCK_PREFIX`, [protocol.md](protocol.md)). A daemon's messages are its program's: a
`potluck` command using another program's daemon prints that daemon's words. Nothing rewrites a message's words on
the way out. The commands' own hints use `Host.Prefix`.

## Not yet the host's

Some hints still name the `potluck` program's own flag and variables: `--pool` (where the pool is ambiguous) and
`POTLUCK_MACHINE`, `$POTLUCK_HOME/...` (the tailnet name hint, `driver pin`'s and `isolation test`'s help).

## The menu bar item

The menu bar item runs the program of the machine's live daemon with Potluck's words (`<program> host status
--json`, `<program> host set drain`; [menubar.md](menubar.md)). A program whose prefix is more than its name
answers those only once it also accepts Potluck's command words at its top level.
