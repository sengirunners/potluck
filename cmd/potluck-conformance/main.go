// Command potluck-conformance is the driver conformance suite (package internal/conformancecmd; docs/conformance.md).
package main

import (
	"os"

	"github.com/sengirunners/potluck/internal/conformancecmd"
)

func main() { os.Exit(conformancecmd.Main(os.Args[1:])) }
