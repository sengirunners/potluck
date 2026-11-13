// Command potluck-driver-tart is a SPEC 20 run target that runs each job in
// a throwaway VM (Tart on a Mac; Incus or an OCI runtime on Linux; package
// vmhost). cmd/zm-run-tart builds the same program under its old name.
package main

import (
	"os"

	"github.com/sengirunners/potluck/vmhost"
)

func main() { os.Exit(vmhost.Main(os.Args[1:])) }
