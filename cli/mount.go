package cli

// Mounting Potluck's commands in another program (docs/mounting.md): the
// Host seam, the commands a host gets, and the one rule for how a hint
// names a Potluck command.

import (
	"context"
	"errors"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runners"
)

// Host is a program whose command tree holds Potluck's commands
// (Commands): how a person types Potluck's root there, the host's own
// global flags, its client for this machine's daemon and its streams. The
// potluck program is one host (Prefix "potluck", its --json and --pool, its
// own client: Run).
type Host struct {
	// Prefix is how a person types Potluck's root in this program:
	// "potluck" for the potluck program, "supper pool" for a program that
	// mounts the commands under its word "pool", "supper" for one that
	// mounts them at its top level. Every hint that names a Potluck command
	// is Prefix and that command's words. Its first word is the program's
	// name: it starts the lines a command prints about itself ("supper:
	// ...") and names the commands every program has of its own
	// (ProgramCommands: "supper daemon --detach").
	Prefix string
	// JSON reports the host's --json flag (nil: never JSON).
	JSON func() bool
	// Pool is the host's pool (project) flag ("" when not given; nil: never
	// given). A command applies it to a copy of Client's client.
	Pool func() string
	// Client is the host's client for this machine's daemon and its client
	// config (the home the commands read files from): its home, its
	// autostart, its remote mode. Required.
	Client func() (*client.Client, *config.Config, error)
	// RunnersRoute, when set, routes GitHub's calls to the runner broker
	// after runners setup turned it on: for a host whose own proxy fronts
	// the broker (such as a tunnel it set up), which now points that proxy
	// at the broker's listener (st.Listening). It says what it changed (""
	// for nothing): setup prints that after its own line, and its --json
	// carries it as "route". home is the client config's home.
	RunnersRoute func(ctx context.Context, home string, st runners.Setup) (string, error)
	// Stdin is the commands' input (nil: none, but host --invite-stdin, which
	// the background run of host --detach gets, then reads the process's
	// own). Stdout and Stderr are required.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// JoinLink is the host's invite page (https://example.com/join): invite
	// prints a link to it carrying the code (project.JoinLink), first, so a
	// friend opens one link. "" prints the code alone.
	JoinLink string
	// AfterLend, when set, is the host program's own step once this
	// machine is lent: host runs it once a run shared at least one size
	// (never in a dry run), such as installing the program's daily update.
	AfterLend *AfterLend
	// AfterJoin, when set, is the host program's next step after join
	// joined a team project that is not one of a pool's areas (what to do
	// there in that program, such as its work), its words to w. It is not
	// called under --json.
	AfterJoin func(w io.Writer, r api.JoinResponse)
}

// AfterLend is a host program's step after a host run lent this machine,
// and the flag that skips it for one run. Potluck runs it and knows nothing
// else of it.
type AfterLend struct {
	// Run does the step, its words to w: stdout, or stderr under --json so
	// stdout stays the run's JSON.
	Run func(w io.Writer)
	// Skip is the flag that skips it, without dashes ("no-auto-update"),
	// and SkipUsage its help. Skip "" adds no flag.
	Skip, SkipUsage string
}

// ProgramCommands are the potluck program's own commands, never mounted:
// they run, report on and replace or remove the program's daemon and binary
// (daemon, status, upgrade, uninstall) or serve its MCP tools (mcp, whose tools are the
// program's). A host has its own; hints name them with the program's name
// (Host.Prefix's first word).
var ProgramCommands = []string{"daemon", "mcp", "status", "upgrade", "uninstall"}

// Commands builds Potluck's commands for h: every top-level command but
// ProgramCommands, each usable under any parent. They read --json and the
// pool through h, talk to the daemon through h's client, and define no
// persistent flags. Build them anew for each run of the host. A host maps
// an error they return to an exit code with ExitCode.
func Commands(h Host) []*cobra.Command {
	h.validate()
	return newApp(h).mountable()
}

// validate panics on a Host that Commands and Report cannot use.
func (h Host) validate() {
	switch {
	case !config.ValidPrefix(h.Prefix):
		panic("cli: Host.Prefix must be how a person types Potluck's root, words of letters, digits, '.', '_' and '-' with one space between, e.g. \"potluck\": got " + strconv.Quote(h.Prefix))
	case h.Client == nil:
		panic("cli: Host.Client is required")
	case h.Stdout == nil || h.Stderr == nil:
		panic("cli: Host.Stdout and Host.Stderr are required")
	case h.AfterLend != nil && h.AfterLend.Run == nil:
		panic("cli: Host.AfterLend.Run is required")
	case h.AfterLend != nil && h.AfterLend.Skip != "" && !flagName.MatchString(h.AfterLend.Skip):
		panic("cli: Host.AfterLend.Skip is a flag name: got " + strconv.Quote(h.AfterLend.Skip))
	}
}

// flagName is a long flag's name without its dashes.
var flagName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// newApp is one run of the commands for h.
func newApp(h Host) *app {
	noStdin := h.Stdin == nil
	if noStdin {
		h.Stdin = strings.NewReader("")
	}
	return &app{host: h, noStdin: noStdin}
}

// mountable are Potluck's commands but ProgramCommands: what Commands
// gives a host, and what the potluck program's root holds beside its own.
func (a *app) mountable() []*cobra.Command {
	return []*cobra.Command{a.tryCmd(), a.createCmd(), a.inviteCmd(), a.joinCmd(), a.leaveCmd(), a.disbandCmd(), a.forgetCmd(), a.remotesCmd(), a.authorityCmd(),
		a.doctorCmd(), a.isolationCmd(), a.hostCmd(), a.ciCmd(), a.sandboxCmd(), a.usageCmd(), a.sealingCmd(),
		a.runCmd(), a.agentTaskCmd(), a.agentRunnerCmd(), a.runnersCmd(), a.secretsCmd(), a.driverCmd(), a.softnetCmd(), a.tartCmd()}
}

// programCommands are ProgramCommands, built.
func (a *app) programCommands() []*cobra.Command {
	return []*cobra.Command{a.daemonCmd(), a.mcpCmd(), a.statusCmd(), a.upgradeCmd(), a.uninstallCmd()}
}

// prefix is Host.Prefix: how a person types Potluck's root in a program.
// Every hint that names a Potluck command is built with it.
type prefix string

// cmd is a Potluck command as a person types it in the program:
// cmd("host status") is "potluck host status" in the potluck program and
// "supper pool host status" where the prefix is "supper pool". A program's
// own command (ProgramCommands) takes the program's name: "supper daemon
// stop".
func (p prefix) cmd(words string) string {
	first, _, _ := strings.Cut(words, " ")
	if slices.Contains(ProgramCommands, first) {
		return p.name() + " " + words
	}
	return string(p) + " " + words
}

// name is the program's name (the prefix's first word): what starts a line
// a command prints about itself ("potluck: sent TERM").
func (p prefix) name() string {
	n, _, _ := strings.Cut(string(p), " ")
	return n
}

// cmd and name are this run's (prefix).
func (a *app) cmd(words string) string { return prefix(a.host.Prefix).cmd(words) }
func (a *app) name() string            { return prefix(a.host.Prefix).name() }

// asJSON reports whether the host's --json was given.
func (a *app) asJSON() bool { return a.host.JSON != nil && a.host.JSON() }

// pool is the host's pool flag ("" when not given).
func (a *app) pool() string {
	if a.host.Pool == nil {
		return ""
	}
	return a.host.Pool()
}

// Report prints err, returned by one of Potluck's commands built for h, as
// the potluck program does (h's --json; the message as written, with
// Potluck's hints for a daemon that is not running or a tailnet name that
// is taken) on h.Stderr, and returns its exit code: ExitCode's, 3 when the
// daemon is not running, 2 for a usage error, else 1. A host with its own
// error reporting uses ExitCode instead.
func Report(h Host, err error) int {
	h.validate()
	return newApp(h).report(err)
}

// ExitCode is the exit code err, returned by one of Potluck's commands,
// asks for: a sandbox command's own exit code, or the code of a refusal (2
// for a usage error) whose output already said why. show is what to print
// first (nil: nothing). ok is false for any other error, which the host
// reports as its own.
func ExitCode(err error) (code int, show error, ok bool) {
	var quiet exitSilently
	if errors.As(err, &quiet) {
		return int(quiet), nil, true
	}
	var ew exitWith
	if errors.As(err, &ew) {
		return ew.code, ew.err, true
	}
	return 0, nil, false
}
