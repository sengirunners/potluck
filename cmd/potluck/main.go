// Command potluck is Potluck's one binary: the daemon (potluck daemon) and
// the commands people run against it. Its home is $POTLUCK_HOME, default
// ~/.potluck; it never reads ZM_HOME. The commands are in package cli.
package main

import (
	"os"

	"github.com/sengirunners/potluck/cli"
)

func main() { os.Exit(cli.Main()) }
