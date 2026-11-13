// Command release-sign signs a Potluck release: it reads the SHA256SUMS
// scripts/release wrote and writes manifest.json (schema potluck-release/1)
// and manifest.json.sig beside it, signed with the Ed25519 key in
// POTLUCK_RELEASE_KEY (base64 of the 32-byte seed, as scripts/release-keygen
// prints it). The release workflow is the only place that key exists.
//
//	POTLUCK_RELEASE_KEY=… go run ./scripts/release-sign -version main-1eacf23e722f -commit <sha> [-out dist]
package main

import (
	"crypto/ed25519"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/sengirunners/potluck/kit/release"
)

func main() {
	version := flag.String("version", "", "the release version (as scripts/release stamped it)")
	commit := flag.String("commit", "", "the full commit it was built from")
	out := flag.String("out", "dist", "the directory holding SHA256SUMS")
	flag.Parse()
	if err := run(*version, *commit, *out); err != nil {
		fmt.Fprintln(os.Stderr, "release-sign:", err)
		os.Exit(1)
	}
}

func run(version, commit, out string) error {
	key, err := release.ParseSeed(os.Getenv("POTLUCK_RELEASE_KEY"))
	if err != nil {
		return fmt.Errorf("POTLUCK_RELEASE_KEY: %w", err)
	}
	m, err := release.Potluck.SignDir(out, version, commit, time.Now(), key)
	if err != nil {
		return err
	}
	fmt.Printf("signed %s (%d files) with %s\n", m.Version, len(m.Files), release.FormatKey(key.Public().(ed25519.PublicKey)))
	return nil
}
