// Command release-keygen prints a new Ed25519 key pair for Potluck's release
// channel: the private seed for the potluck-release environment's secret
// POTLUCK_RELEASE_KEY, and the public key for internal/upgrade.ReleaseKey
// (or a machine's config update_keys). It writes nothing to disk.
//
//	go run ./scripts/release-keygen
package main

import (
	"fmt"
	"os"

	"github.com/sengirunners/potluck/kit/release"
)

func main() {
	seed, pub, err := release.NewKey()
	if err != nil {
		fmt.Fprintln(os.Stderr, "release-keygen:", err)
		os.Exit(1)
	}
	fmt.Println("secret POTLUCK_RELEASE_KEY (potluck-release environment only):", seed)
	fmt.Println("public key (internal/upgrade.ReleaseKey):                      ", pub)
}
