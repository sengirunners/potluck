// Command zm-run-tart is potluck-driver-tart under its name from before the rename, so build lists and updaters that name this path keep working.
package main

import (
	"os"

	"github.com/sengirunners/potluck/vmhost"
)

func main() { os.Exit(vmhost.Main(os.Args[1:])) }
