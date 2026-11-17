// Command release-keygen makes a new Ed25519 key pair for Potluck's release
// channel. It writes the private seed (the value of the potluck-release
// environment's secret POTLUCK_RELEASE_KEY) to a new file readable only by
// you, and prints only the public key, for internal/upgrade/release_keys.txt
// (or a machine's config update_keys). The seed is never printed, so it
// stays out of terminal scrollback and logs. docs/release-keys.md says what
// to do with each half.
//
//	go run ./scripts/release-keygen -seed-file <path>
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/sengirunners/potluck/kit/release"
)

func main() {
	seedFile := flag.String("seed-file", "", "write the private seed to this new file (mode 0600; an existing file is refused)")
	flag.Parse()
	if err := run(os.Stdout, *seedFile); err != nil {
		fmt.Fprintln(os.Stderr, "release-keygen:", err)
		os.Exit(1)
	}
}

func run(out io.Writer, seedFile string) error {
	if seedFile == "" {
		return errors.New("-seed-file is required: the seed is written there, never printed")
	}
	seed, pub, err := release.NewKey()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(seedFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(seed + "\n"); err != nil {
		f.Close()
		os.Remove(seedFile)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(seedFile)
		return err
	}
	_, err = fmt.Fprintf(out, "seed written to %s (the secret POTLUCK_RELEASE_KEY; never commit or print it)\npublic key (a line of internal/upgrade/release_keys.txt): %s\n", seedFile, pub)
	return err
}
