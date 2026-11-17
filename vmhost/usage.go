package vmhost

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// driverUsage is what potluck-driver-tart prints when a person, not the
// daemon, runs it: the daemon sends it one JSON request on stdin and takes
// one JSON answer, so a bare run says what it is instead of failing on JSON.
const driverUsage = `potluck-driver-tart is the VM driver the Potluck daemon runs for you. It reads one
JSON request on standard input and prints one JSON answer, so you rarely run it
by hand. To share a machine, use potluck host (potluck host --help). To
wind a machine down or stop lending it, use potluck host set drain or
potluck host set closed.

` + hostUsage + `
With no other arguments it reads one JSON request on standard input (what the daemon sends).
Setup and settings: cmd/potluck-driver-tart/README.md in the Potluck source; the protocol: docs/protocol.md.
`

// usageOnly answers a person who runs the driver without a request: --help
// (or -h, help) prints and exits 0; a terminal on stdin with
// nothing to read, or an argument the driver does not know, prints the usage
// and exits 2 (a real terminal; /dev/null is not one). The daemon's runs (a
// request on a pipe, no arguments) are not touched. done says the program
// should exit with code.
func usageOnly(argv []string, stdin *os.File, stdout, stderr io.Writer) (code int, done bool) {
	if len(argv) > 0 {
		switch argv[0] {
		case "-h", "--help", "help", "-help":
			fmt.Fprint(stdout, driverUsage)
			return 0, true
		}
		fmt.Fprintf(stderr, "potluck-driver-tart: unknown argument %q\n\n%s", argv[0], driverUsage)
		return 2, true
	}
	if term.IsTerminal(int(stdin.Fd())) {
		fmt.Fprint(stderr, driverUsage)
		return 2, true
	}
	return 0, false
}
