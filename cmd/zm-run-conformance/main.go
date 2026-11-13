// Command zm-run-conformance is potluck-conformance under its name from before the rename, so build lists and updaters that name this path keep working.
package main

import (
	"os"

	"github.com/sengirunners/potluck/internal/conformancecmd"
)

func main() { os.Exit(conformancecmd.Main(os.Args[1:])) }
