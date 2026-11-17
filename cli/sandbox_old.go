package cli

// Old spellings of sandbox commands that programs outside Potluck still
// run (docs/coming-from-zm.md): spawn, output, kill and renew. Each is the
// new command underneath, built by the same function with the same flags
// and the same --json answer, hidden from help; its help names the command
// it is. sandbox cp's old two-argument form is read by oldCpArgs.

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/boxcp"
	"github.com/sengirunners/potluck/kit/api"
)

// sandboxOldSpellings are the hidden old spellings, each built from its
// new command.
func (a *app) sandboxOldSpellings() []*cobra.Command {
	spawn := a.sandboxExecCmd() // exec --detach, always
	d := spawn.Flags().Lookup("detach")
	d.DefValue, d.Hidden = "true", true
	_ = d.Value.Set("true")
	run := spawn.RunE
	spawn.RunE = func(cmd *cobra.Command, args []string) error {
		if d.Value.String() != "true" {
			return exitWith{2, errors.New("sandbox spawn always detaches; " + a.cmd("sandbox exec") + " waits")}
		}
		return run(cmd, args)
	}
	return []*cobra.Command{
		a.oldSpelling(spawn, "spawn <sandbox> -- <argv...>", "sandbox exec --detach"),
		a.oldSpelling(a.sandboxLogsCmd(), "output <sandbox> <pid>", "sandbox logs"),
		a.oldSpelling(a.sandboxSignalCmd(), "kill <sandbox> <pid> [--signal TERM|INT|KILL]", "sandbox signal"),
		a.oldSpelling(a.sandboxExtendCmd(), "renew <sandbox> [--ttl 10m]", "sandbox extend"),
	}
}

// oldSpelling hides cmd under the old word in use; its help says it is
// the command now spelled is.
func (a *app) oldSpelling(cmd *cobra.Command, use, is string) *cobra.Command {
	cmd.Use, cmd.Hidden, cmd.Example = use, true, ""
	cmd.Short = "The old spelling of " + a.cmd(is) + ", kept for programs that run it"
	cmd.Long = cmd.Short + ": the same command, flags and answer. Use " + a.cmd(is) + "."
	return cmd
}

// oldCpArgs reads sandbox cp's old two-argument form, one argument the
// sandbox's path as box:<machine-size>@<machine>/<id>:<path> and the other a
// local path, as the three arguments cp takes now: the sandbox, then the
// two paths, the sandbox's starting with ":".
func oldCpArgs(args []string) ([]string, error) {
	h0, p0, box0, err := boxcp.Split(args[0])
	if err != nil {
		return nil, err
	}
	h1, p1, box1, err := boxcp.Split(args[1])
	if err != nil {
		return nil, err
	}
	local := func(p string) string { // a local path that starts with ":" is written ./:name
		if strings.HasPrefix(p, ":") {
			return "./" + p
		}
		return p
	}
	switch {
	case box0 && !box1:
		return []string{h0, ":" + p0, local(args[1])}, nil
	case box1 && !box0:
		return []string{h1, local(args[0]), ":" + p1}, nil
	}
	return nil, api.Errorf(api.CodeBadRequest, "one side is local and the other box:<machine-size>@<machine>/<id>:<path>")
}
