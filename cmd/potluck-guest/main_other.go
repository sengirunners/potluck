//go:build !linux

// Command potluck-guest runs inside a Linux guest only (main_linux.go).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "potluck-guest runs inside a Linux guest")
	os.Exit(2)
}
