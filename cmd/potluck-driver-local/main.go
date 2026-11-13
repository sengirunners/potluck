// Command potluck-driver-local is the local driver: each run on this machine, as the daemon's user (package internal/localdriver).
package main

import (
	"os"

	"github.com/sengirunners/potluck/internal/localdriver"
)

func main() { os.Exit(localdriver.Main()) }
