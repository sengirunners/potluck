package cli

// Old spellings of host commands that programs outside Potluck still run
// (docs/coming-from-zm.md): host share and host doctor. Each is the new
// command underneath, built by the same function with the same flags and
// the same --json answer, hidden from help.

import (
	"errors"

	"github.com/spf13/cobra"
)

// hostOldSpellings are host's hidden subcommands share and doctor.
func (a *app) hostOldSpellings() []*cobra.Command {
	return []*cobra.Command{a.hostShareOld(), a.hostDoctorOld()}
}

// hostShareOld is host with the pool as --pool, not an argument: the old
// host share --pool <pool>. Without --pool it shares as host with no
// argument does (the host's pool flag, else the only pool). In the potluck
// program --pool (-P) is its global flag already.
func (a *app) hostShareOld() *cobra.Command {
	var s shareFlags
	var pool string
	cmd := &cobra.Command{
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			arg := pool
			if arg == "" {
				arg = a.pool()
			}
			return a.runHost(cmd, &s, arg)
		},
	}
	s.add(cmd, a)
	// The potluck program's own --pool (-P) is already the pool; a host
	// program's pool flag has its own name, so the old --pool is added.
	if !a.own {
		cmd.Flags().StringVar(&pool, "pool", "", "the pool to share with: host's argument")
	}
	a.oldSpelling(cmd, "share [--pool <pool>] [--image name@sha256:<digest>]... [flags]", "host")
	cmd.Long += " --pool <pool> is host's argument."
	return cmd
}

// hostDoctorOld is doctor. Its --json answer exits 0 whatever it found, as
// the old spelling did: a program reads the answer, not the exit code.
func (a *app) hostDoctorOld() *cobra.Command {
	cmd := a.doctorCmd()
	run := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		err := run(cmd, args)
		if quiet := exitSilently(0); a.asJSON() && errors.As(err, &quiet) {
			return nil // the answer, printed, says what doctor found
		}
		return err
	}
	return a.oldSpelling(cmd, "doctor [flags]", "doctor")
}
