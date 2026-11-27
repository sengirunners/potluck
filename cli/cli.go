// Package cli is the potluck command: the daemon (potluck daemon) and the
// commands people run against it. Another program built on Potluck mounts
// the same commands in its own tree (Commands, mount.go): every hint they
// print names a command through the host's prefix (app.cmd).
//
// Each run builds a fresh command tree around an app, which holds the host
// (its prefix, global flags, client and streams), so tests can run many
// commands in one process (cli_test.go). A program built on the potluck
// command itself (Program, RunProgram) brings its own daemon wiring,
// commands and MCP tools.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sengirunners/potluck/attach"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tunnel"
)

// programName is the potluck program's name: its root command and
// config.Program.
const programName = "potluck"

// joinPage is the potluck program's invite page (Host.JoinLink): it shows a
// friend the one line that installs Potluck and joins. It reads the code from
// the link's fragment, which the site never receives.
const joinPage = "https://potluck.sh/join"

// Potluck's home: POTLUCK_HOME, else ~/.potluck, with its socket
// potluck.sock. Never zm's: a zm home holds migrations this build does not
// know, and its daemon already serves everything potluck's does.
const (
	EnvHome    = "POTLUCK_HOME"
	HomeDir    = ".potluck"
	SocketName = "potluck.sock"
)

// Potluck's machine name: POTLUCK_MACHINE, else config.json's, else the host
// name with "-potluck" after it. The suffix keeps potluck's tailnet node
// (potluck-<machine>) from taking the name of a zm daemon's node on the same
// machine and tailnet, which would break invites and joins: a zm home made
// from the rename's step 2 on names its node potluck-<host> too (zm's older
// homes keep zm-<host>).
const (
	EnvMachine    = "POTLUCK_MACHINE"
	MachineSuffix = "-potluck"
)

// Potluck's other variables: who the human is, and none of zm's remote
// client (ZM_HOST): potluck commands talk to this machine's daemon, or,
// inside a box, to the authority its credential names.
const EnvHuman = "POTLUCK_HUMAN"

// EnvBoxCredential is the path of a box credential file (what the
// potluck-token kind delivers into a box, in $POTLUCK_SECRETS). When it is
// set, every command talks to the authority that file names, over its
// tunnel, with no daemon (docs/daemon.md "Calling the authority from a
// box").
const EnvBoxCredential = "POTLUCK_BOX_CREDENTIAL"

// UseHome points kit/config at Potluck's home, machine name, variables and
// program: kit's messages name potluck's commands, and say so of the ones
// potluck has none of (lacks). Main calls it first; a test that starts
// daemons in this package calls it before loading any config.
func UseHome() {
	config.Use(config.Names{HomeEnvs: []string{EnvHome}, HomeDirs: []string{HomeDir}, Sockets: []string{SocketName}})
	config.MachineEnv, config.MachineSuffix = EnvMachine, MachineSuffix
	config.HumanEnv, config.HostEnv = EnvHuman, ""
	useProgram(Program{})
}

// nameTaken is the hint for a tailnet node whose name another node holds
// (kit/tailnet's invite and join refusals name a --machine flag and a
// console step; potluck's remedy is its machine variable).
func (a *app) nameTaken() string {
	return "another node on your tailnet already has this machine's name. Give " + a.name() + " its own: " +
		"set POTLUCK_MACHINE=<name> (or \"machine\" in $POTLUCK_HOME/config.json), restart the daemon " +
		"(" + a.cmd("daemon stop") + ", then " + a.cmd("daemon --detach") + ") and run this again"
}

// Main runs the potluck command with the process's arguments and returns
// its exit code.
func Main() int { return MainProgram(Program{}) }

// signalContext ends when one of caughtSignals arrives.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), caughtSignals()...)
}

// caughtSignals end a command's context. SIGHUP too: a closed terminal ends
// a command as Ctrl-C does, so what it holds (potluck run's sandbox) is
// released. Not when started with SIGHUP ignored (nohup).
func caughtSignals() []os.Signal {
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	return sigs
}

// app is one run of Potluck's commands for a host (mount.go): how they
// are typed there, its global flags, its client and its streams.
type app struct {
	host Host
	prog Program // what this binary adds to Potluck (program.go); the potluck program's alone
	// own: this is the potluck program's own run (RunProgram), whose daemon
	// and home follow attach.go's rule; a mounted run uses the host's
	// client and home.
	own bool
	// noStdin: the host gave no Stdin, so commands read an empty one; host
	// --invite-stdin reads the process's own (stdinInvite).
	noStdin bool
	// running is the command being executed (set before its RunE), so
	// the potluck program's client can tell whether it may start the daemon.
	running *cobra.Command
	// The daemon this run uses (home, attach.go), decided once.
	homeDone bool
	homeV    daemonHome
	homeErr  error
	// joinNote is the last note a waiting join printed (askForJoin).
	joinNote string
}

// Run runs one potluck command line and returns its exit code: 0, 1 for
// refusals and errors, 2 for usage, 3 when the daemon is not running, or a
// sandbox command's own exit code.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return RunProgram(ctx, Program{}, args, stdin, stdout, stderr)
}

// root is the potluck program's command tree: its own commands
// (ProgramCommands) and the mountable ones, with --json and --pool.
func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:   programName,
		Short: "Pool your machines with friends and run sandboxes on them",
		Long: `Potluck pools machines: one person creates a pool and invites friends; a
friend joins it, and may share a machine with it (` + a.cmd("host") + `). Anyone in
the pool can then create sandboxes on the shared machines and run commands
in them. A local daemon (` + a.cmd("daemon") + `) does the work; every command talks
to it over a socket in $POTLUCK_HOME (default ~/.potluck). On a machine
whose daemon is another program's built on Potluck, and with no
~/.potluck, the commands use that daemon instead (docs/daemon.md).`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       api.Version + " (" + api.Commit + ")",
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			a.running = cmd
		},
	}
	var asJSON bool
	var pool string
	root.PersistentFlags().BoolVar(&asJSON, "json", false, "print JSON")
	root.PersistentFlags().StringVarP(&pool, "pool", "P", "", "the pool (default: the only pool this machine is in)")
	a.host.JSON = func() bool { return asJSON }
	a.host.Pool = func() string { return pool }
	root.AddCommand(a.programCommands()...)
	root.AddCommand(a.mountable()...)
	a.addCommands(root)
	return root
}

// report prints err (as JSON with --json) and returns the exit code.
func (a *app) report(err error) int {
	if code, show, ok := ExitCode(err); ok {
		if show != nil {
			a.report(show)
		}
		return code
	}
	var e *api.Error
	if client.NotRunning(err) { // Potluck's hint, with the host's prefix
		e = &api.Error{Code: api.CodeUnavailable, Message: "the " + a.name() + " daemon is not running",
			Hint:  "start it with: " + a.cmd("daemon --detach") + " (most commands start it on their own)",
			Plain: "Potluck isn't running. Start it, then try again." + plainGetHelp}
		if a.own { // a mounted run's daemon is the host's: attach.go is not consulted
			if o := a.attachedTo(); o != nil {
				e = &api.Error{Code: api.CodeUnavailable, Message: fmt.Sprintf("%s's daemon (home %s), which potluck's commands use, stopped answering", o.Program, attach.Tilde(o.Home)),
					Hint: "start it again with that program's command", Plain: o.Program + ", which Potluck on this Mac is part of, stopped answering. Start it, then try again." + plainGetHelp}
			}
		}
	}
	words := api.PlainForm(err)
	if e == nil && !errors.As(err, &e) && words != "" && a.asJSON() {
		e = &api.Error{Code: api.CodeInternal, Message: err.Error()}
	}
	if e != nil {
		if e.Plain == "" && words != "" {
			c := *e
			c.Plain = words
			e = &c
		}
		if e.Code == api.CodeConflict && strings.Contains(e.Message, "is taken on this tailnet") {
			e.Hint = a.nameTaken()
		}
		if a.asJSON() {
			b, _ := json.Marshal(api.ErrorResponse{Error: e})
			_ = json.NewEncoder(a.host.Stderr).Encode(json.RawMessage(textclean.JSON(b)))
		} else {
			msg := a.name() + ": " + e.Message
			if e.Hint != "" {
				msg += "\n  " + e.Hint
			}
			fmt.Fprintln(a.host.Stderr, textclean.Clean(msg))
		}
		if client.NotRunning(err) {
			return 3
		}
		return 1
	}
	fmt.Fprintln(a.host.Stderr, a.name()+":", textclean.Clean(err.Error()))
	if strings.Contains(err.Error(), "unknown command") || strings.Contains(err.Error(), "flag") ||
		strings.Contains(err.Error(), "arg(s)") {
		return 2
	}
	return 1
}

// exitSilently ends a command with this exit code and no message: its
// output already said why.
type exitSilently int

func (e exitSilently) Error() string { return fmt.Sprintf("exit %d", int(e)) }

// exitWith ends a command with this exit code, printing err (when set) as
// report does (a sandbox command's exit code).
type exitWith struct {
	code int
	err  error
}

func (e exitWith) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("exit %d", e.code)
}

// client returns the host's client for this machine's daemon (Host.Client)
// and its client config, with the host's pool flag applied to a copy.
func (a *app) client() (*client.Client, *config.Config, error) {
	c, cfg, err := a.host.Client()
	if err != nil {
		return nil, nil, err
	}
	if p := a.pool(); p != "" {
		c = inPool(c, p)
	}
	return c, cfg, nil
}

// ownClient is the potluck program's Host.Client: a client for this
// machine's daemon on its socket: potluck's own, or the daemon of another
// program this run uses (home, attach.go). It starts potluck's own daemon
// when none answers, unless the running command never does
// (autostartAllowed); never another program's. It reads none of zm's
// client variables (ZM_HOST, ZM_TOKEN, ZM_AS, ZM_PROJECT, ZM_SESSION).
// With POTLUCK_BOX_CREDENTIAL set it is instead a client for the authority
// that credential names, and starts nothing.
func (a *app) ownClient() (*client.Client, *config.Config, error) {
	h, err := a.home()
	if err != nil {
		return nil, nil, err
	}
	cfg := h.Cfg
	var c *client.Client
	if path := os.Getenv(EnvBoxCredential); path != "" {
		cred, err := tunnel.ReadBoxCredential(path)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", EnvBoxCredential, err)
		}
		if c, err = tunnel.BoxClient(cred); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", EnvBoxCredential, err)
		}
	} else {
		c = client.NewUnix(cfg.SocketPath())
		c.Cwd, _ = os.Getwd()
		// Another program's daemon, or one on its home, is never started from here.
		if h.Owner == "" && autostartAllowed(a.running, os.Getenv) {
			c.Autostart = func(ctx context.Context) error { return a.autostart(ctx, cfg) }
		}
	}
	return c, cfg, nil
}

// readClient is a client for this machine's daemon that never starts it
// (host status, asked every few seconds; doctor and isolation, which only
// read): the potluck program's own socket, or a copy of the host's client
// with its autostart off. No pool applied.
func (a *app) readClient() (*client.Client, error) {
	if !a.own {
		c, _, err := a.host.Client()
		if err != nil {
			return nil, err
		}
		x := *c
		x.Autostart = nil
		return &x, nil
	}
	cfg, err := a.loadClient()
	if err != nil {
		return nil, err
	}
	return client.NewUnix(cfg.SocketPath()), nil
}

// inPool is a client for this machine's daemon with the pool as its
// project: --pool, else the only pool this machine runs or joined. The
// daemon sends a joined pool's calls on to the machine that runs it.
func (a *app) inPool(ctx context.Context) (*client.Client, error) {
	c, _, err := a.client()
	if err != nil {
		return nil, err
	}
	if c.Project != "" {
		return c, nil
	}
	p, err := a.onlyPool(ctx, c)
	if err != nil {
		return nil, err
	}
	return inPool(c, p), nil
}

// onlyPool is the one pool this machine runs or joined.
func (a *app) onlyPool(ctx context.Context, c *client.Client) (string, error) {
	slugs, err := poolSlugs(ctx, c)
	if err != nil {
		return "", err
	}
	switch len(slugs) {
	case 1:
		return slugs[0], nil
	case 0:
		return "", api.Plainly(errors.New("this machine is in no pool yet: "+a.cmd("create <pool>")+", or "+a.cmd("join '<invite code>'")),
			"Paste the invite you were sent first, then press the button again.")
	}
	return "", &manyPoolsError{Pools: slugs}
}

// poolSlugs is the pools this machine runs or joined.
func poolSlugs(ctx context.Context, c *client.Client) ([]string, error) {
	l, err := c.ProjectList(ctx)
	if err != nil {
		return nil, err
	}
	var slugs []string
	for _, p := range l.Projects {
		if !slices.Contains(slugs, p.Slug) {
			slugs = append(slugs, p.Slug)
		}
	}
	return slugs, nil
}

// manyPoolsError is onlyPool's answer on a machine in several pools. A
// command whose pool is an argument (potluck host <pool>) says so instead
// of --pool.
type manyPoolsError struct{ Pools []string }

func (e *manyPoolsError) Error() string {
	return fmt.Sprintf("this machine is in more than one pool (%s): name one with --pool", strings.Join(e.Pools, ", "))
}

// inPool is c with the pool as its project.
func inPool(c *client.Client, pool string) *client.Client {
	x := *c
	x.Project, x.ProjectFrom = pool, "flag"
	return &x
}

// printJSON writes v indented to the app's stdout.
func (a *app) printJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(a.host.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(json.RawMessage(textclean.JSON(b)))
}

// say prints one line (or more) for people on stdout.
func (a *app) say(format string, args ...any) {
	fmt.Fprintln(a.host.Stdout, strings.TrimRight(textclean.Clean(fmt.Sprintf(format, args...)), "\n"))
}

// call is a common RunE body: build a client, run fn, print its result.
// human formats it for people; nil (or --json) prints JSON.
func call[T any](a *app, cmd *cobra.Command, fn func(context.Context, *client.Client) (T, error), human func(T) string) error {
	c, _, err := a.client()
	if err != nil {
		return err
	}
	v, err := fn(cmd.Context(), c)
	if err != nil {
		return err
	}
	if a.asJSON() || human == nil {
		return a.printJSON(v)
	}
	if s := human(v); s != "" {
		a.say("%s", s)
	}
	return nil
}
