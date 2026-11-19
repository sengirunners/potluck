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
	AfterLend *AfterLend // optional
}

type AfterLend struct {
	Run             func(w io.Writer)
	Skip, SkipUsage string // the flag that skips it ("no-auto-update"), and its help
}

func Commands(h Host) []*cobra.Command
func Report(h Host, err error) int
func ExitCode(err error) (code int, show error, ok bool)
var ProgramCommands = []string{"daemon", "mcp", "status", "upgrade"}
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
  `cli/pool.go`).
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
- **Report** prints an error a command returns as `potluck` does, on the host's stderr and in its words (an API
  error as JSON with `--json`; Potluck's hints for a daemon that is not running or a tailnet name that is taken), and returns
  the exit code: a sandbox command's own, 3 when the daemon is not running, 2 for a usage error, else 1.
- **ExitCode** is for a host that reports errors itself: it maps an error to a sandbox command's own exit code
  (`sandbox exec`, `run`), or the code of a refusal whose output already said why. Any other error is the host's to
  report.

`Commands` never calls `cli.UseHome` and never changes `config.Program` or `config.Names`: the host keeps its own
home, machine record and home claim. Build the commands anew for each run of the host (they hold that run's
state). A host can add the same command at two places, built twice with two prefixes: `supper pool sandbox` and
`supper sandbox` are one command with two spellings.

## What a host gets

Every top-level command of `potluck` except `ProgramCommands`: `create`, `invite`, `join`, `leave`, `forget`,
`remotes`, `doctor`, `isolation`, `host` (with `status`, `set`, `pause`, `resume`, `stop`, `shelf`), `ci`, `sandbox`, `usage`,
`sealing`, `run`, `agent-task`, `agent-runner`, `runners`, `secrets` and `driver`.

`daemon`, `status`, `upgrade` and `mcp` are not mounted: they run, report on and replace one program's daemon and
binary, or serve that program's MCP tools, and a host has its own. A hint that names one uses the program's name
(`supper status`).

## What still goes through kit's word table

Messages from kit and the compute packages are written in zm's words and rewritten for the running program by
`config.Text` with `config.Program` and `config.Verbs` (`kit/config/program.go`). The commands still pass their
human output through it (`say`, the `words` writer, error reports), so a host sets `config.Program` and
`config.Verbs` for its own words. New code names commands with the prefix, never through that table.

## Not yet the host's

Some hints still name the `potluck` program's own flag and variables: `--pool` (where the pool is ambiguous) and
`POTLUCK_MACHINE`, `$POTLUCK_HOME/...` (the tailnet name hint, `driver pin`'s and `isolation test`'s help).

## The menu bar item

The menu bar item runs the program of the machine's live daemon with Potluck's words (`<program> host status
--json`, `<program> host set drain`; [menubar.md](menubar.md)). A program whose prefix is more than its name
answers those only once it also accepts Potluck's command words at its top level.
