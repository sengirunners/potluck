// Command potluck-e2b is the E2B gateway: the E2B SDK on boxes, as a client
// of this machine's potluck daemon (package internal/e2bgateway).
package main

import (
	"os"

	"github.com/sengirunners/potluck/cli"
	"github.com/sengirunners/potluck/internal/e2bgateway"
)

func main() {
	cli.UseHome() // potluck's home and words; cmd/zm-e2b keeps zm's
	os.Exit(e2bgateway.Main(os.Args[1:]))
}
