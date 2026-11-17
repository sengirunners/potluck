// Package cli is the potluck command: the daemon (potluck daemon) and the
// commands people run against it. The commands were copied from zm's cmd/zm
// (pool, host, sandbox, job, target, runners and the team commands) and use
// Potluck's words; zm keeps its own copies for now.
//
// Each Run builds a fresh command tree around an app, which holds the
// global flags and where output goes, so tests can run many commands in one
// process (cli_test.go). A program built on this command (Program,
// RunProgram) brings its own daemon wiring, commands and MCP tools.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// words. Main calls it first; a test that starts daemons in this package
// calls it before loading any config.
func UseHome() {
	config.Use(config.Names{HomeEnvs: []string{EnvHome}, HomeDirs: []string{HomeDir}, Sockets: []string{SocketName}})
	config.MachineEnv, config.MachineSuffix = EnvMachine, MachineSuffix
	config.HumanEnv, config.HostEnv = EnvHuman, ""
	config.Program, config.Verbs = "potluck", Verbs
}

// Verbs maps zm's command words, as kit's messages name them, to potluck's
// (config.Text rewrites kit's messages with it). An empty value is a zm
// command potluck has none of yet.
var Verbs = map[string]string{
	// The same words in both.
	"daemon": "daemon", "daemon --detach": "daemon --detach", "daemon stop": "daemon stop",
	"join": "join", "leave": "leave", "invite": "invite", "remotes": "remotes", "forget": "forget",
	"sandbox": "sandbox", "sandbox create": "sandbox create", "sandbox rm": "sandbox rm",
	"sandbox exec": "sandbox exec", "sandbox ls": "sandbox ls",
	"runners": "runners", "runners setup": "runners setup", "runners link": "runners link",
	"runners unlink": "runners unlink", "status": "status",
	// Other words.
	"host share":  "host",
	"pool host":   "host",
	"host doctor": "doctor",
	"host stop":   "host stop",
	"box exec":    "sandbox exec",
	"job run":     "run",
	"pool create": "create",
	// zm's alone, for now (with the subcommands kit's messages name).
	"daemon update": "", "daemon stop --end-sessions": "", "invite ls": "", "invite rm": "",
	"tunnel": "", "tunnel setup": "", "tunnel setup --quick": "", "tunnel status": "", "tunnel remove": "",
	"tunnel key escrow": "", "tailnet": "", "tailnet up": "", "tailnet down": "", "peers": "",
	"target": "", "target ls": "", "target bind": "", "target unbind": "", "machine-class": "",
	"login": "", "login github": "", "identity": "", "identity add": "", "identity unlink": "",
	"grant": "", "revoke": "", "token": "", "token mint": "", "token ls": "", "token revoke": "",
	"project": "", "project use": "", "project set": "", "project why": "", "project team": "",
	"area": "", "area add": "", "area set": "", "init": "", "tracker": "", "tracker rm": "",
	"backup": "", "backup restore": "", "outbox": "", "inbox": "", "ls": "", "exec": "",
	"exec --cancel": "", "task": "", "task status": "", "session": "", "session stop": "",
	"ask": "", "ask new": "", "ask answer": "", "work": "", "work import": "", "webhooks": "", "connect": "",
}

// words writes human text built from kit's messages in potluck's words
// (config.Text). Never for JSON (a key may be "zm") or a sandbox's output.
type words struct{ w io.Writer }

func (t words) Write(p []byte) (int, error) {
	if _, err := io.WriteString(t.w, config.Text(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// nameTaken is the hint for a tailnet node whose name another node holds
// (kit/tailnet's invite and join refusals name zm's remedies).
const nameTaken = "another node on your tailnet already has this machine's name. Give potluck its own: " +
	"set POTLUCK_MACHINE=<name> (or \"machine\" in $POTLUCK_HOME/config.json), restart the daemon " +
	"(potluck daemon stop, then potluck daemon --detach) and run this again"

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

// app is one run of the command: its global flags and its streams.
type app struct {
	prog    Program // what this binary adds to Potluck (program.go)
	json    bool
	project string
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	// running is the command being executed (set before its RunE), so
	// client can tell whether it may start the daemon.
	running *cobra.Command
}

// Run runs one potluck command line and returns its exit code: 0, 1 for
// refusals and errors, 2 for usage, 3 when the daemon is not running, or a
// sandbox command's own exit code.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return RunProgram(ctx, Program{}, args, stdin, stdout, stderr)
}

func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:   "potluck",
		Short: "Pool your machines with friends and run sandboxes on them",
		Long: `Potluck pools machines: one person creates a pool and invites friends; a
friend joins it, and may share a machine with it (potluck host). Anyone in
the pool can then create sandboxes on the shared machines and run commands
in them. A local daemon (potluck daemon) does the work; every command talks
to it over a socket in $POTLUCK_HOME (default ~/.potluck).`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       api.Version + " (" + api.Commit + ")",
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			a.running = cmd
		},
	}
	root.PersistentFlags().BoolVar(&a.json, "json", false, "print JSON")
	root.PersistentFlags().StringVarP(&a.project, "pool", "P", "", "the pool (default: the only pool this machine is in)")
	root.AddCommand(a.daemonCmd(), a.statusCmd(),
		a.createCmd(), a.inviteCmd(), a.joinCmd(), a.leaveCmd(), a.forgetCmd(), a.remotesCmd(),
		a.doctorCmd(), a.isolationCmd(), a.hostCmd(), a.ciCmd(),
		a.sandboxCmd(), a.mcpCmd(), a.usageCmd(), a.sealingCmd(), a.runCmd(), a.agentRunnerCmd(), a.runnersCmd(), a.secretsCmd(), a.driverCmd(), a.upgradeCmd())
	a.addCommands(root)
	return root
}

// report prints err (as JSON with --json) and returns the exit code.
func (a *app) report(err error) int {
	var quiet exitSilently
	if errors.As(err, &quiet) {
		return int(quiet)
	}
	var ew exitWith
	if errors.As(err, &ew) {
		if ew.err != nil {
			a.report(ew.err)
		}
		return ew.code
	}
	var e *api.Error
	if client.NotRunning(err) { // kit's words name zm's commands
		e = &api.Error{Code: api.CodeUnavailable, Message: "the potluck daemon is not running",
			Hint: "start it with: potluck daemon --detach (most commands start it on their own)"}
	}
	if e != nil || errors.As(err, &e) {
		if e.Code == api.CodeConflict && strings.Contains(e.Message, "is taken on this tailnet") {
			e.Hint = nameTaken
		}
		x := *e // kit's words in potluck's, as text and as JSON
		x.Message, x.Hint = config.Text(e.Message), config.Text(e.Hint)
		e = &x
		if a.json {
			b, _ := json.Marshal(api.ErrorResponse{Error: e})
			_ = json.NewEncoder(a.stderr).Encode(json.RawMessage(textclean.JSON(b)))
		} else {
			msg := "potluck: " + e.Message
			if e.Hint != "" {
				msg += "\n  " + e.Hint
			}
			fmt.Fprintln(a.stderr, textclean.Clean(msg))
		}
		if client.NotRunning(err) {
			return 3
		}
		return 1
	}
	fmt.Fprintln(a.stderr, "potluck:", textclean.Clean(config.Text(err.Error())))
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

// client returns a client for this machine's daemon on its socket, with
// --pool applied. It starts the daemon when none answers, unless the
// running command never does (autostartAllowed). It reads none of zm's
// client variables (ZM_HOST, ZM_TOKEN, ZM_AS, ZM_PROJECT, ZM_SESSION).
// With POTLUCK_BOX_CREDENTIAL set it is instead a client for the authority
// that credential names, and starts nothing.
func (a *app) client() (*client.Client, *config.Config, error) {
	cfg, err := config.LoadClient()
	if err != nil {
		return nil, nil, err
	}
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
		if autostartAllowed(a.running, os.Getenv) {
			c.Autostart = func(ctx context.Context) error { return a.autostart(ctx, cfg) }
		}
	}
	if a.project != "" {
		c.Project, c.ProjectFrom = a.project, "flag"
	}
	return c, cfg, nil
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
	p, err := onlyPool(ctx, c)
	if err != nil {
		return nil, err
	}
	return inPool(c, p), nil
}

// onlyPool is the one pool this machine runs or joined.
func onlyPool(ctx context.Context, c *client.Client) (string, error) {
	l, err := c.ProjectList(ctx)
	if err != nil {
		return "", err
	}
	var slugs []string
	for _, p := range l.Projects {
		if !slices.Contains(slugs, p.Slug) {
			slugs = append(slugs, p.Slug)
		}
	}
	switch len(slugs) {
	case 1:
		return slugs[0], nil
	case 0:
		return "", errors.New("this machine is in no pool yet: potluck create <pool>, or potluck join '<invite code>'")
	}
	return "", fmt.Errorf("this machine is in more than one pool (%s): name one with --pool", strings.Join(slugs, ", "))
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
	enc := json.NewEncoder(a.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(json.RawMessage(textclean.JSON(b)))
}

// say prints one line (or more) for people on stdout.
func (a *app) say(format string, args ...any) {
	fmt.Fprintln(a.stdout, strings.TrimRight(textclean.Clean(config.Text(fmt.Sprintf(format, args...))), "\n"))
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
	if a.json || human == nil {
		return a.printJSON(v)
	}
	if s := human(v); s != "" {
		a.say("%s", s)
	}
	return nil
}
