package hostprobe

import (
	"context"

	"github.com/sengirunners/potluck/hostswap"
)

// readSwap is the machine's swap (hostswap): its state, which goes in the
// report and the host's advert, and the note doctor prints when it is not
// safe for box secrets.
func readSwap(ctx context.Context, run Runner, osName string) hostswap.Reading {
	if _, real := run.(ExecRunner); real {
		return hostswap.Read(ctx, hostswap.ExecRunner{}, osName) // the one a host's advert reads through
	}
	return hostswap.Read(ctx, swapRunner{run}, osName)
}

// swapRunner lends a hostprobe Runner to hostswap.
type swapRunner struct{ Runner }

func (s swapRunner) Run(ctx context.Context, name string, args ...string) (hostswap.Result, error) {
	res, err := s.Runner.Run(ctx, name, args...)
	return hostswap.Result{Stdout: res.Stdout, Exit: res.Exit}, err
}
