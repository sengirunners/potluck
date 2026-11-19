package cli

import (
	"fmt"

	"github.com/sengirunners/potluck/runtarget/rt"
)

// execStopped is the line sandbox exec prints on stderr when its command was
// stopped at its timeout (the answer's timed_out): how long it ran, which
// limit that was, and what to do instead. asked is --timeout as given (0: the
// default); ran is the timeout_s the host answered it ran under, which is
// less than asked when the sandbox's lease ends first.
func (a *app) execStopped(asked, ran int) string {
	want := asked
	if want == 0 {
		want = rt.ExecDefaultS
	}
	if ran == 0 {
		ran = want
	}
	detach := "--detach for longer work (" + a.cmd("sandbox logs") + " reads its output)"
	switch {
	case ran < want:
		return fmt.Sprintf("stopped after %d s, when the sandbox's lease ends: %s, then rerun, or use %s", ran, a.cmd("sandbox extend"), detach)
	case asked == 0:
		return fmt.Sprintf("stopped after %d s (the default): rerun with --timeout <seconds, up to %d>, or %s", ran, rt.ExecMaxS, detach)
	case ran < rt.ExecMaxS:
		return fmt.Sprintf("stopped after %d s (--timeout %d): rerun with a longer --timeout (up to %d), or %s", ran, asked, rt.ExecMaxS, detach)
	default:
		return fmt.Sprintf("stopped after %d s (--timeout's most): rerun with %s", ran, detach)
	}
}
