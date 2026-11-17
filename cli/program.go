package cli

import (
	"context"
	"io"
	"os"
	"slices"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/server"
	sandboxmcp "github.com/sengirunners/potluck/mcp"
	"github.com/sengirunners/potluck/wiring"
)

// Program is a binary built on the potluck command: the daemon its
// `daemon` command runs, and the commands and MCP tools its modules add.
// The zero Program is Potluck itself (wiring.Compute, Potluck's commands,
// the sandbox tools).
type Program struct {
	// Wiring builds the daemon's modules (nil: wiring.Compute). Their
	// routes, migrations, probe parts, health parts and request gates are
	// the daemon's; `upgrade --preflight` describes this build with it.
	Wiring server.Wiring
	// Commands are added to the command tree after Potluck's own. A name
	// (or alias) the tree already has panics.
	Commands []Command
	// Tools are added to `mcp`'s server after the sandbox tools (a name
	// already taken panics, mcp.Add).
	Tools []sandboxmcp.Tools
}

// Command builds one of a program's commands for one run of the binary.
type Command func(env Env) *cobra.Command

// Env is one run of the command as a program's command sees it: the global
// flags, the streams and this machine's daemon.
type Env struct{ a *app }

// Client is a client for this machine's daemon with --pool applied. It
// starts the daemon when none answers, as Potluck's own commands do.
func (e Env) Client() (*client.Client, error) {
	c, _, err := e.a.client()
	return c, err
}

// InPool is Client with the pool as its project: --pool, else the only
// pool this machine runs or joined.
func (e Env) InPool(ctx context.Context) (*client.Client, error) { return e.a.inPool(ctx) }

// JSON reports whether --json was given.
func (e Env) JSON() bool { return e.a.asJSON() }

// PrintJSON writes v, indented and with control characters cleaned, to
// stdout.
func (e Env) PrintJSON(v any) error { return e.a.printJSON(v) }

// Say prints a line for people on stdout, in this program's words.
func (e Env) Say(format string, args ...any) { e.a.say(format, args...) }

// Stdin, Stdout and Stderr are the run's streams.
func (e Env) Stdin() io.Reader  { return e.a.host.Stdin }
func (e Env) Stdout() io.Writer { return e.a.host.Stdout }
func (e Env) Stderr() io.Writer { return e.a.host.Stderr }

// MainProgram is Main for p.
func MainProgram(p Program) int {
	UseHome()
	stampVersion()
	ctx, stop := signalContext()
	defer stop()
	return RunProgram(ctx, p, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

// RunProgram is Run for p.
func RunProgram(ctx context.Context, p Program, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := programApp(p, stdin, stdout, stderr)
	root := a.root()
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		return a.report(err)
	}
	return 0
}

// programApp is the potluck program's own run of p: the host whose prefix
// is "potluck", whose client is ownClient and whose flags root defines.
func programApp(p Program, stdin io.Reader, stdout, stderr io.Writer) *app {
	a := newApp(Host{Prefix: programName, Stdin: stdin, Stdout: stdout, Stderr: stderr})
	a.prog, a.own = p, true
	a.host.Client = a.ownClient
	return a
}

// wiring is the daemon's wiring: the program's, else Potluck's.
func (p Program) wiring() server.Wiring {
	if p.Wiring != nil {
		return p.Wiring
	}
	return wiring.Compute
}

// addCommands adds the program's commands to root, refusing a name or
// alias root already answers to.
func (a *app) addCommands(root *cobra.Command) {
	for _, build := range a.prog.Commands {
		cmd := build(Env{a})
		for _, name := range append([]string{cmd.Name()}, cmd.Aliases...) {
			if slices.ContainsFunc(root.Commands(), func(c *cobra.Command) bool { return c.Name() == name || c.HasAlias(name) }) {
				panic("cli: command " + name + " is already in the tree")
			}
		}
		root.AddCommand(cmd)
	}
}
