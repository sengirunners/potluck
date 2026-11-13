// Command zm-e2b is the E2B gateway under its name from before the rename,
// so build lists and updaters that name this path keep working. It is a
// client of this machine's zm daemon (e2bgateway.UseOldHome: zm's home and
// socket), where potluck-e2b is the potluck daemon's (package
// internal/e2bgateway).
package main

import (
	"os"

	"github.com/sengirunners/potluck/internal/e2bgateway"
)

func main() {
	e2bgateway.UseOldHome()
	os.Exit(e2bgateway.Main(os.Args[1:]))
}
