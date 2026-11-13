// Command zm-run-local is potluck-driver-local under its name from before the rename, so build lists and updaters that name this path keep working.
package main

import (
	"os"

	"github.com/sengirunners/potluck/internal/localdriver"
)

func main() { os.Exit(localdriver.Main()) }
